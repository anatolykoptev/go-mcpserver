package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func validVerifier(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if token == "valid-token" {
		return &auth.TokenInfo{
			Scopes:     []string{"mcp:read", "mcp:write"},
			Expiration: time.Now().Add(time.Hour),
		}, nil
	}
	return nil, auth.ErrInvalidToken
}

func TestBearerAuthRejectsMCP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "auth-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:    "auth-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier: validVerifier,
		},
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	// POST /mcp without token → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/mcp without token: status = %d, want 401", rec.Code)
	}

	// /health should still work without auth
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/health status = %d, want 200", rec.Code)
	}
}

func TestBearerAuthAcceptsMCP(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "auth-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:    "auth-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier: validVerifier,
		},
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0.1"}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Errorf("/mcp with valid token: status = %d, want pass-through", rec.Code)
	}
}

func TestBearerAuthScopeCheck(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "auth-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:    "auth-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier: validVerifier,
			Scopes:   []string{"admin"},
		},
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer valid-token")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("/mcp with wrong scope: status = %d, want 403", rec.Code)
	}
}

func TestProtectedResourceMetadata(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "meta-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:    "meta-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier: validVerifier,
			Metadata: &ProtectedResourceMetadata{
				Resource:             "https://example.com/mcp",
				AuthorizationServers: []string{"https://auth.example.com"},
			},
		},
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("metadata endpoint: status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var meta map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta["resource"] != "https://example.com/mcp" {
		t.Errorf("resource = %v, want https://example.com/mcp", meta["resource"])
	}
}

// bearerRoundTripper injects Authorization header into every request.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(r)
}

func authClient(ts *httptest.Server, token string) *http.Client {
	return &http.Client{
		Transport: &bearerRoundTripper{token: token, base: ts.Client().Transport},
	}
}

func connectMCP(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           authClient(ts, token),
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	sess, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func scopeFilter(_ context.Context, toolName string, info *TokenInfo) bool {
	if info == nil {
		return false
	}
	for _, s := range info.Scopes {
		if s == "tool:"+toolName {
			return true
		}
	}
	return false
}

func newFilterTestServer(t *testing.T) *mcp.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "filter-test", Version: "0.0.1"}, nil)
	for _, name := range []string{"allowed", "denied", "other"} {
		n := name
		mcp.AddTool(server, &mcp.Tool{Name: n}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + n}}}, nil, nil
		})
	}
	return server
}

func TestToolFilterHidesTools(t *testing.T) {
	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:   validVerifier,
			ToolFilter: scopeFilter,
		},
		DisableRequestLog: true,
	})

	sess := connectMCP(t, ts, "valid-token")
	result, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	// validVerifier returns scopes ["mcp:read", "mcp:write"]
	// scopeFilter expects "tool:<name>", so no tools should pass
	if len(result.Tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(result.Tools))
	}
}

func TestToolFilterBlocksCall(t *testing.T) {
	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:   validVerifier,
			ToolFilter: scopeFilter,
		},
		DisableRequestLog: true,
	})

	sess := connectMCP(t, ts, "valid-token")
	result, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "denied"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for denied tool")
	}
}

func TestToolFilterPassesCall(t *testing.T) {
	grantVerifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token == "valid-token" {
			return &auth.TokenInfo{
				Scopes:     []string{"tool:allowed"},
				Expiration: time.Now().Add(time.Hour),
			}, nil
		}
		return nil, auth.ErrInvalidToken
	}

	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-test",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:   grantVerifier,
			ToolFilter: scopeFilter,
		},
		DisableRequestLog: true,
	})

	sess := connectMCP(t, ts, "valid-token")
	result, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "allowed"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if result.IsError {
		t.Error("expected IsError=false for allowed tool")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected content")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "ok:allowed" {
		t.Errorf("unexpected content: %v", result.Content[0])
	}
}

func TestStaticTokenVerifier(t *testing.T) {
	v := StaticTokenVerifier("secret-123")

	info, err := v(context.Background(), "secret-123", nil)
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if info.Expiration.IsZero() {
		t.Error("expected non-zero expiration")
	}

	_, err = v(context.Background(), "wrong", nil)
	if err == nil {
		t.Error("invalid token: expected error")
	}
}
