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
