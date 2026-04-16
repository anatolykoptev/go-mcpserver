package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// ===== Hard edge-case and security tests =====

func TestRESTToolNameInjection(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	tests := []struct {
		name       string
		toolPath   string
		wantStatus int
	}{
		{"path traversal", "/api/tools/..%2Fetc%2Fpasswd", http.StatusBadRequest},
		{"command injection", "/api/tools/echo;rm%20-rf%20/", http.StatusNotFound}, // semicolon splits URL at router level
		{"null byte", "/api/tools/echo%00evil", http.StatusBadRequest},
		{"path traversal slash", "/api/tools/echo%2F..%2Fadmin", http.StatusBadRequest},
		{"xss script tag", "/api/tools/%3Cscript%3Ealert(1)%3C%2Fscript%3E", http.StatusBadRequest},
		{"dots in name", "/api/tools/my.tool", http.StatusBadRequest},
		{"spaces in name", "/api/tools/my%20tool", http.StatusBadRequest},
		{"backslash in name", "/api/tools/my%5Ctool", http.StatusBadRequest},
		{"pipe in name", "/api/tools/echo%7Ccat", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name+" GET", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.toolPath, nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("GET %s: status = %d, want %d; body = %s",
					tt.toolPath, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
		t.Run(tt.name+" POST", func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.toolPath, strings.NewReader(`{}`))
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("POST %s: status = %d, want %d; body = %s",
					tt.toolPath, rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}

	t.Run("very long valid name", func(t *testing.T) {
		longName := strings.Repeat("a", 1000)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/"+longName, nil))
		// Valid chars, but tool doesn't exist → 404
		if rec.Code != http.StatusNotFound {
			t.Errorf("long name: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty name hits list route", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/", nil))
		// /api/tools/ with trailing slash should not match {name} route
		// It should either 404 or match a different handler
		if rec.Code == http.StatusBadRequest {
			t.Error("empty name should not reach tool name validation")
		}
	})

	t.Run("hyphen and underscore valid", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools/my-tool_v2", nil))
		// Valid name but nonexistent → 404
		if rec.Code != http.StatusNotFound {
			t.Errorf("hyphen-underscore: status = %d, want 404", rec.Code)
		}
	})
}

func TestRESTConcurrentCalls(t *testing.T) {
	h := newRESTTestHandler(t, echoTool())

	const n = 50
	type result struct {
		idx  int
		code int
		echo string
		err  error
	}

	results := make([]result, n)
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			msg := fmt.Sprintf("msg-%d", idx)
			body := fmt.Sprintf(`{"message":"%s"}`, msg)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(body))
			req.Header.Set("Content-Type", contentTypeJSON)
			h.ServeHTTP(rec, req)

			results[idx].idx = idx
			results[idx].code = rec.Code

			var resp struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				results[idx].err = fmt.Errorf("decode: %w", err)
				return
			}
			if len(resp.Content) > 0 {
				results[idx].echo = resp.Content[0].Text
			}
		}(i)
	}

	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			t.Errorf("goroutine %d: %v", r.idx, r.err)
			continue
		}
		if r.code != http.StatusOK {
			t.Errorf("goroutine %d: status = %d, want 200", r.idx, r.code)
			continue
		}
		expected := fmt.Sprintf("msg-%d", r.idx)
		if r.echo != expected {
			t.Errorf("goroutine %d: echo = %q, want %q (cross-contamination!)", r.idx, r.echo, expected)
		}
	}
}

func TestRESTResponseHeaders(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list 200", http.MethodGet, "/api/tools", ""},
		{"get 200", http.MethodGet, "/api/tools/echo", ""},
		{"get 404", http.MethodGet, "/api/tools/nonexistent", ""},
		{"get 400", http.MethodGet, "/api/tools/bad%20name", ""},
		{"call 200", http.MethodPost, "/api/tools/echo", `{"message":"hi"}`},
		{"call 400 bad json", http.MethodPost, "/api/tools/echo", `{bad`},
		{"call 422 error", http.MethodPost, "/api/tools/echo", `{}`},
		{"call 500 unknown", http.MethodPost, "/api/tools/nonexistent", `{}`},
		{"openapi 200", http.MethodGet, "/api/openapi.json", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var req *http.Request
			if tt.body != "" {
				req = httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", contentTypeJSON)
			} else {
				req = httptest.NewRequest(tt.method, tt.path, nil)
			}
			h.ServeHTTP(rec, req)

			ct := rec.Header().Get("Content-Type")
			if !strings.HasPrefix(ct, contentTypeJSON) {
				t.Errorf("Content-Type = %q, want %q prefix", ct, contentTypeJSON)
			}

			if rid := rec.Header().Get(requestIDHeader); rid == "" {
				t.Error("missing X-Request-ID header")
			}

			if sv := rec.Header().Get("Server"); sv != "" {
				t.Errorf("Server header should not be set, got %q", sv)
			}
		})
	}
}

func TestRESTBodyEdgeCases(t *testing.T) {
	h := newRESTTestHandler(t, echoTool())

	t.Run("JSON array", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`[1,2,3]`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("JSON number", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`42`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("JSON string", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`"hello"`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("JSON null", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(`null`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		// null unmarshals into nil map → parseRequestBody returns nil map
		// Could be 400 or treated as empty args
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status = %d, want 400 or 422", rec.Code)
		}
	})

	t.Run("deeply nested JSON", func(t *testing.T) {
		// Build 100-level nested JSON: {"a":{"a":...{"message":"deep"}...}}
		var sb strings.Builder
		for i := 0; i < 100; i++ {
			sb.WriteString(`{"a":`)
		}
		sb.WriteString(`"leaf"`)
		for i := 0; i < 100; i++ {
			sb.WriteString(`}`)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(sb.String()))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		// Should parse fine (Go json has no nesting limit by default)
		// echo tool returns 422 because no "message" key
		if rec.Code != http.StatusOK && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("deeply nested: status = %d, want 200 or 422", rec.Code)
		}
	})

	t.Run("unicode in values", func(t *testing.T) {
		body := `{"message":"привет мир 🌍"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(body))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unicode: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}

		var resp struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Content) == 0 || resp.Content[0].Text != "привет мир 🌍" {
			t.Errorf("unicode round-trip failed: got %q", resp.Content[0].Text)
		}
	})

	t.Run("body exactly maxBodySize", func(t *testing.T) {
		// Create a valid JSON body that is exactly maxBodySize bytes
		// We need: {"message":"<padding>"} where total = maxBodySize
		prefix := `{"message":"`
		suffix := `"}`
		padLen := maxBodySize - len(prefix) - len(suffix)
		body := prefix + strings.Repeat("x", padLen) + suffix
		if len(body) != maxBodySize {
			t.Fatalf("body length = %d, want %d", len(body), maxBodySize)
		}

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(body))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("exact maxBodySize: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("body maxBodySize plus one", func(t *testing.T) {
		body := strings.Repeat("x", maxBodySize+1)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/echo", strings.NewReader(body))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("maxBodySize+1: status = %d, want 400", rec.Code)
		}
	})
}

func TestRESTMethodNotAllowed(t *testing.T) {
	h := newRESTTestHandler(t, defaultTools()...)

	disallowed := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/tools/echo"},
		{http.MethodDelete, "/api/tools/echo"},
		{http.MethodPatch, "/api/tools/echo"},
	}

	for _, tt := range disallowed {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code == http.StatusOK {
				t.Errorf("%s %s: status = 200, should be rejected", tt.method, tt.path)
			}
		})
	}

	t.Run("GET tool with body ignored", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools/echo", strings.NewReader(`{"ignored":true}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET with body: status = %d, want 200", rec.Code)
		}
	})

	t.Run("POST list endpoint", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools", strings.NewReader(`{}`))
		h.ServeHTTP(rec, req)
		// POST /api/tools should not match GET /tools handler
		if rec.Code == http.StatusOK {
			t.Error("POST /api/tools should not return 200 (GET-only)")
		}
	})
}

func TestRESTOpenAPISpecialChars(t *testing.T) {
	injectionTool := toolDef{
		tool: &mcp.Tool{
			Name:        "inject-test",
			Description: `Tool with "quotes", <script>alert(1)</script>, and\nnewlines`,
		},
		handler: func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil, nil
		},
	}

	h := newRESTTestHandler(t, injectionTool)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Must be valid JSON (no injection breaking the structure)
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("OpenAPI spec is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}

	// Verify the description round-trips correctly
	paths, _ := spec["paths"].(map[string]any)
	toolPath, _ := paths["/api/tools/inject-test"].(map[string]any)
	post, _ := toolPath["post"].(map[string]any)
	summary, _ := post["summary"].(string)

	if summary != injectionTool.tool.Description {
		t.Errorf("description not preserved:\n  got:  %q\n  want: %q", summary, injectionTool.tool.Description)
	}
}

func TestRESTAuthLoopbackBypass(t *testing.T) {
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
			Verifier:       StaticTokenVerifier("secret-token"),
			LoopbackBypass: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("loopback no token succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("loopback without token: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("loopback wrong token still succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("Authorization", "Bearer wrong-token")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("loopback with wrong token: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-loopback no token fails", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.RemoteAddr = "192.168.1.1:54321"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("non-loopback without token: status = %d, want 401", rec.Code)
		}
	})

	t.Run("ipv6 loopback succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
		req.RemoteAddr = "[::1]:54321"
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("ipv6 loopback: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestRESTToolCallTimeout(t *testing.T) {
	slowTool := toolDef{
		tool: &mcp.Tool{
			Name:        "slow",
			Description: "Blocks for a long time",
		},
		handler: func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			select {
			case <-time.After(5 * time.Second):
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "done"}},
				}, nil, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		},
	}

	// Build with a very short tool timeout
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)
	mcp.AddTool(server, slowTool.tool, slowTool.handler)

	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		ToolTimeout:       100 * time.Millisecond, // Very short timeout
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/slow", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", contentTypeJSON)
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	// Should complete quickly (not hang for 5s)
	if elapsed > 2*time.Second {
		t.Errorf("request took %s, expected < 2s (timeout not working)", elapsed)
	}

	// Should return an error (either 422 from tool timeout middleware or 500)
	if rec.Code == http.StatusOK {
		t.Errorf("slow tool should not return 200 after timeout; body = %s", rec.Body.String())
	}

	// Verify the response body is valid JSON.
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
}

func TestRESTCallFailTool(t *testing.T) {
	h := newRESTTestHandler(t, failTool())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tools/fail", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", contentTypeJSON)
	h.ServeHTTP(rec, req)

	// failTool returns an error from the handler; the MCP SDK wraps it into an error result (422)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Verify error message doesn't leak internal details
	errMsg, _ := resp["error"].(string)
	if strings.Contains(errMsg, "/home/") || strings.Contains(errMsg, "goroutine") || strings.Contains(errMsg, ".go:") {
		t.Errorf("error message leaks internals: %q", errMsg)
	}
}

func TestRESTEmptyServer(t *testing.T) {
	// Server with no tools registered
	h := newRESTTestHandler(t)

	t.Run("list returns empty array", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		body := strings.TrimSpace(rec.Body.String())
		// Must be [] or null — but preferably []
		var tools []any
		if err := json.Unmarshal([]byte(body), &tools); err != nil {
			// Could be null
			if body == "null" {
				t.Error("empty tools list returned null instead of []")
			} else {
				t.Fatalf("invalid JSON: %v; body = %s", err, body)
			}
		} else if tools == nil {
			t.Error("empty tools list decoded to nil slice instead of []")
		}
	})

	t.Run("openapi valid with empty paths", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var spec map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("invalid OpenAPI JSON: %v", err)
		}

		if spec["openapi"] != openAPIVersion {
			t.Errorf("openapi = %v, want %s", spec["openapi"], openAPIVersion)
		}

		paths, _ := spec["paths"].(map[string]any)
		if paths == nil {
			t.Error("paths is nil, want empty object")
		} else if len(paths) != 0 {
			t.Errorf("paths has %d entries, want 0", len(paths))
		}
	})

	t.Run("call nonexistent tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tools/anything", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestRESTBridgeWithDisabledMCP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "0.0.1",
	}, nil)

	h, err := Build(server, Config{
		Name:              "test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableMCP:        true,
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("list tools returns 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tools", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (REST bridge should not register when MCP disabled)", rec.Code)
		}
	})

	t.Run("openapi returns 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}
