package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultRESTPrefix = "/api"
	contentTypeJSON   = "application/json"
	openAPIVersion    = "3.1.0"
	maxBodySize       = 1 << 20 // 1 MB
)

var validToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// restBridge proxies HTTP requests to an in-process MCP client session.
type restBridge struct {
	session     *mcp.ClientSession
	prefix      string
	cfg         Config
	logger      *slog.Logger
	cachedOnce  sync.Once
	cachedTools []*mcp.Tool
	cachedErr   error
}

// startRESTBridge creates an in-process MCP client, connects it to the server,
// and registers REST endpoints on the mux.
func startRESTBridge(ctx context.Context, server *mcp.Server, mux *http.ServeMux, cfg Config, logger *slog.Logger) error {
	serverT, clientT := mcp.NewInMemoryTransports()

	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		return err
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    cfg.Name + "-rest-bridge",
		Version: cfg.Version,
	}, nil)

	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return err
	}

	prefix := cfg.RESTPrefix
	if prefix == "" {
		prefix = defaultRESTPrefix
	}
	prefix = strings.TrimRight(prefix, "/")

	b := &restBridge{
		session: session,
		prefix:  prefix,
		cfg:     cfg,
		logger:  logger,
	}

	restMux := http.NewServeMux()
	restMux.HandleFunc("GET /tools", b.handleListTools)
	restMux.HandleFunc("GET /tools/{name}", b.handleGetTool)
	restMux.HandleFunc("POST /tools/{name}", b.handleCallTool)
	restMux.HandleFunc("GET /openapi.json", b.handleOpenAPI)

	var handler = http.StripPrefix(prefix, restMux)
	if cfg.BearerAuth != nil {
		handler = applyBearerAuth(handler, cfg.BearerAuth)
	}
	mux.Handle(prefix+"/", handler)

	go func() {
		<-ctx.Done()
		if err := session.Close(); err != nil {
			logger.Error("REST bridge session close error", slog.Any("error", err))
		}
	}()

	logger.Info("REST bridge enabled",
		slog.String("prefix", prefix),
	)
	return nil
}

// handleListTools returns all available tools as a JSON array.
func (b *restBridge) handleListTools(w http.ResponseWriter, r *http.Request) {
	tools, err := b.getTools(r.Context())
	if err != nil {
		b.writeError(w, http.StatusInternalServerError, "failed to list tools", err)
		return
	}
	if tools == nil {
		tools = []*mcp.Tool{}
	}
	b.writeJSON(w, http.StatusOK, tools)
}

// handleGetTool returns a single tool's schema, or 404 if not found.
func (b *restBridge) handleGetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validToolName.MatchString(name) {
		b.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid tool name",
		})
		return
	}

	tools, err := b.getTools(r.Context())
	if err != nil {
		b.writeError(w, http.StatusInternalServerError, "failed to list tools", err)
		return
	}

	for _, t := range tools {
		if t.Name == name {
			b.writeJSON(w, http.StatusOK, t)
			return
		}
	}

	b.writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "tool not found: " + name,
	})
}

// handleCallTool invokes an MCP tool and returns the result as JSON.
func (b *restBridge) handleCallTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validToolName.MatchString(name) {
		b.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid tool name",
		})
		return
	}

	args, err := parseRequestBody(r)
	if err != nil {
		b.writeError(w, http.StatusBadRequest, "invalid request body", err)
		return
	}

	result, err := b.session.CallTool(r.Context(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		b.writeError(w, http.StatusInternalServerError, "tool call failed", err)
		return
	}

	status := http.StatusOK
	if result.IsError {
		status = http.StatusUnprocessableEntity
	}

	resp := toolCallResponse{
		Content:    result.Content,
		Structured: result.StructuredContent,
		IsError:    result.IsError,
	}
	b.writeJSON(w, status, resp)
}

// handleOpenAPI generates and returns an OpenAPI 3.1 spec from tool schemas.
func (b *restBridge) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	tools, err := b.getTools(r.Context())
	if err != nil {
		b.writeError(w, http.StatusInternalServerError, "failed to list tools", err)
		return
	}

	spec := b.buildOpenAPISpec(tools)
	b.writeJSON(w, http.StatusOK, spec)
}

// toolCallResponse is the JSON envelope for tool call results.
type toolCallResponse struct {
	Content    []mcp.Content `json:"content"`
	Structured any           `json:"structured,omitempty"`
	IsError    bool          `json:"is_error"`
}

// getTools returns the cached tools list, fetching once on first call.
func (b *restBridge) getTools(ctx context.Context) ([]*mcp.Tool, error) {
	b.cachedOnce.Do(func() {
		b.cachedTools, b.cachedErr = b.listAllTools(ctx)
	})
	return b.cachedTools, b.cachedErr
}

// listAllTools fetches all tools using pagination.
func (b *restBridge) listAllTools(ctx context.Context) ([]*mcp.Tool, error) {
	var all []*mcp.Tool
	var cursor string

	for {
		params := &mcp.ListToolsParams{Cursor: cursor}
		result, err := b.session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return all, nil
}

// parseRequestBody reads the request body as a map of arguments.
// An empty body is treated as an empty arguments map.
// Bodies larger than 1 MB are rejected.
func parseRequestBody(r *http.Request) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodySize {
		return nil, errors.New("request body too large")
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(body, &args); err != nil {
		return nil, err
	}
	return args, nil
}

// buildOpenAPISpec generates an OpenAPI 3.1 specification from tool definitions.
func (b *restBridge) buildOpenAPISpec(tools []*mcp.Tool) map[string]any {
	paths := make(map[string]any, len(tools))
	for _, t := range tools {
		paths[b.prefix+"/tools/"+t.Name] = buildToolPath(t)
	}

	spec := map[string]any{
		"openapi": openAPIVersion,
		"info": map[string]string{
			"title":   b.cfg.Name,
			"version": b.cfg.Version,
		},
		"paths": paths,
	}

	if b.cfg.BearerAuth != nil {
		spec["components"] = map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]string{
					"type":   "http",
					"scheme": "bearer",
				},
			},
		}
		spec["security"] = []map[string]any{
			{"bearerAuth": []string{}},
		}
	}

	return spec
}

// buildToolPath creates the OpenAPI path item for a single tool.
func buildToolPath(t *mcp.Tool) map[string]any {
	op := map[string]any{
		"operationId": t.Name,
		"responses": map[string]any{
			"200": map[string]any{
				"description": "Successful tool call",
				"content": map[string]any{
					contentTypeJSON: map[string]any{
						"schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"content": map[string]any{
									"type":  "array",
									"items": map[string]string{"type": "object"},
								},
								"structured": map[string]string{"type": "object"},
								"is_error":   map[string]string{"type": "boolean"},
							},
						},
					},
				},
			},
			"400": map[string]any{
				"description": "Invalid request",
			},
			"422": map[string]any{
				"description": "Tool returned an error",
			},
			"500": map[string]any{
				"description": "Internal server error",
			},
		},
	}

	if t.Description != "" {
		op["summary"] = t.Description
	}

	if t.InputSchema != nil {
		inputSchema := normalizeSchema(t.InputSchema)
		op["requestBody"] = map[string]any{
			"content": map[string]any{
				contentTypeJSON: map[string]any{
					"schema": inputSchema,
				},
			},
		}
	}

	return map[string]any{"post": op}
}

// normalizeSchema converts the InputSchema (which may be *jsonschema.Schema
// or map[string]any) into a plain map suitable for JSON marshaling.
func normalizeSchema(schema any) any {
	if schema == nil {
		return nil
	}
	// If it's already a map, use it directly.
	if _, ok := schema.(map[string]any); ok {
		return schema
	}
	// Otherwise, round-trip through JSON to get a plain map.
	data, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return schema
	}
	return m
}

// writeJSON writes a JSON response with the given status code.
func (b *restBridge) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		b.logger.Error("failed to write JSON response", slog.Any("error", err))
	}
}

// writeError writes a JSON error response and logs the error.
func (b *restBridge) writeError(w http.ResponseWriter, status int, msg string, err error) {
	if err != nil {
		b.logger.Error(msg, slog.Any("error", err))
	} else {
		b.logger.Error(msg)
	}
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
