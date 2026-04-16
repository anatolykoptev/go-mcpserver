package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newRESTTestHandler creates an HTTP handler with RESTBridge enabled and the
// given tools registered on the MCP server.
type toolDef struct {
	tool    *mcp.Tool
	handler func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error)
}

func newRESTTestHandler(t testing.TB, tools ...toolDef) http.Handler {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	for _, td := range tools {
		mcp.AddTool(server, td.tool, td.handler)
	}
	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func echoTool() toolDef {
	return toolDef{
		tool: &mcp.Tool{
			Name:        "echo",
			Description: "Echo tool for testing",
		},
		handler: func(_ context.Context, _ *mcp.CallToolRequest, input map[string]any) (*mcp.CallToolResult, any, error) {
			msg, _ := input["message"].(string)
			if msg == "" {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "message required"}},
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: msg}},
			}, map[string]string{"echo": msg}, nil
		},
	}
}

func failTool() toolDef {
	return toolDef{
		tool: &mcp.Tool{
			Name:        "fail",
			Description: "Always fails",
		},
		handler: func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return nil, nil, errors.New("intentional error")
		},
	}
}

func defaultTools() []toolDef {
	return []toolDef{echoTool(), failTool()}
}

// ---------- Tests ----------

func TestRESTListTools(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Errorf("Content-Type = %q, want %q", ct, contentTypeJSON)
	}

	var tools []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&tools); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}

	names := map[string]bool{}
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names[name] = true
		if name == "echo" {
			desc, _ := tool["description"].(string)
			if desc == "" {
				t.Error("echo tool missing description")
			}
		}
	}
	if !names["echo"] || !names["fail"] {
		t.Errorf("expected echo and fail tools, got %v", names)
	}
}

func TestRESTGetTool(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	t.Run("existing tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/echo", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var tool map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&tool); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if tool["name"] != "echo" {
			t.Errorf("name = %v, want echo", tool["name"])
		}
	})

	t.Run("nonexistent tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/nonexistent", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("invalid tool name", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/invalid%20name", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

func TestRESTCallTool(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	t.Run("successful call", func(t *testing.T) {
		body := `{"message":"hello"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(body))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var resp map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if isErr, _ := resp["is_error"].(bool); isErr {
			t.Error("expected is_error=false")
		}
		content, ok := resp["content"].([]any)
		if !ok || len(content) == 0 {
			t.Fatal("expected non-empty content")
		}
	})

	t.Run("empty body returns tool error", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", nil)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 422", rec.Code)
		}

		var resp map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if isErr, _ := resp["is_error"].(bool); !isErr {
			t.Error("expected is_error=true")
		}
	})

	t.Run("nonexistent tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/nonexistent", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`{bad`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("invalid tool name", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/bad%20name", strings.NewReader(`{}`))
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

func TestRESTCallToolBodyLimit(t *testing.T) {
	h := newRESTTestHandler(t, echoTool())

	bigBody := strings.Repeat("x", maxBodySize+1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(bigBody))
	req.Header.Set("Content-Type", contentTypeJSON)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request body") {
		t.Errorf("body = %q, want mention of request body", rec.Body.String())
	}
}

func TestRESTOpenAPI(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if spec["openapi"] != openAPIVersion {
		t.Errorf("openapi = %v, want %s", spec["openapi"], openAPIVersion)
	}

	info, ok := spec["info"].(map[string]any)
	if !ok {
		t.Fatal("missing info object")
	}
	if info["title"] != "test" {
		t.Errorf("info.title = %v, want test", info["title"])
	}
	if info["version"] != "0.0.1" {
		t.Errorf("info.version = %v, want 0.0.1", info["version"])
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths object")
	}
	echoPath, ok := paths["/api/tools/echo"]
	if !ok {
		t.Fatal("missing /api/tools/echo path")
	}
	echoObj, ok := echoPath.(map[string]any)
	if !ok {
		t.Fatal("echo path is not an object")
	}
	post, ok := echoObj["post"].(map[string]any)
	if !ok {
		t.Fatal("echo path missing post operation")
	}
	if _, ok := post["requestBody"]; !ok {
		t.Error("echo post missing requestBody")
	}
}

func TestRESTOpenAPIWithAuth(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	for _, td := range defaultTools() {
		mcp.AddTool(server, td.tool, td.handler)
	}

	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		BearerAuth: &BearerAuth{
			Verifier: StaticTokenVerifier("test-token"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode: %v", err)
	}

	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatal("missing components")
	}
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatal("missing securitySchemes")
	}
	if _, ok := schemes["bearerAuth"]; !ok {
		t.Error("missing bearerAuth scheme")
	}

	security, ok := spec["security"].([]any)
	if !ok || len(security) == 0 {
		t.Error("missing or empty security array")
	}
}

func TestRESTBridgeDisabled(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        false,
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestRESTCustomPrefix(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	for _, td := range defaultTools() {
		mcp.AddTool(server, td.tool, td.handler)
	}
	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		RESTPrefix:        "/v2",
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("custom prefix works", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/tools", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("default prefix returns 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestRESTAuth(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	for _, td := range defaultTools() {
		mcp.AddTool(server, td.tool, td.handler)
	}
	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		BearerAuth: &BearerAuth{
			Verifier: StaticTokenVerifier("test-token"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no token returns 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("valid token on list", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("valid token on call", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`{"message":"hi"}`))
		req.Header.Set("Content-Type", contentTypeJSON)
		req.Header.Set("Authorization", "Bearer test-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("invalid token returns 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

func TestParseRequestBody(t *testing.T) {
	t.Run("empty body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		args, err := parseRequestBody(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(args) != 0 {
			t.Errorf("expected empty map, got %v", args)
		}
	})

	t.Run("valid JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"key":"value","num":42}`))
		args, err := parseRequestBody(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if args["key"] != "value" {
			t.Errorf("key = %v, want value", args["key"])
		}
		if args["num"] != float64(42) {
			t.Errorf("num = %v, want 42", args["num"])
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{bad`))
		_, err := parseRequestBody(req)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		big := strings.Repeat("x", maxBodySize+1)
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(big))
		_, err := parseRequestBody(req)
		if err == nil {
			t.Fatal("expected error for oversized body")
		}
		if !strings.Contains(err.Error(), "request body too large") {
			t.Errorf("error = %q, want mention of request body too large", err)
		}
	})
}
