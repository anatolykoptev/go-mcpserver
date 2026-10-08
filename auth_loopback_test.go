package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:12345", true},
		{"[::1]:12345", true},
		{"192.168.1.1:12345", false},
		{"10.0.0.1:12345", false},
	}
	for _, tt := range tests {
		r := &http.Request{RemoteAddr: tt.addr}
		got := isLoopback(r)
		if got != tt.want {
			t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

// loopbackIdentityFor builds a LoopbackIdentity that reads the caller
// name from a proxy-injected header — the deployment shape LoopbackBypass
// exists for.
func loopbackIdentityFor(header string, scopes map[string][]string) func(*http.Request) (*TokenInfo, error) {
	return func(r *http.Request) (*TokenInfo, error) {
		user := r.Header.Get(header)
		if user == "" {
			return nil, auth.ErrInvalidToken
		}
		s, ok := scopes[user]
		if !ok {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: user, Scopes: s}, nil
	}
}

// Regression for issue #33: with LoopbackBypass the bearer middleware is
// skipped, so RequestExtra.TokenInfo is nil and a ToolFilter that keys on
// identity silently sees "nil" for every caller. LoopbackIdentity must
// populate the same TokenInfo plumbing for bypassed requests, and the
// filter must apply identically to tools/list and tools/call.
func TestLoopbackBypass_ToolFilterSeesIdentity(t *testing.T) {
	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-loopback",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:       validVerifier,
			LoopbackBypass: true,
			ToolFilter:     scopeFilter,
			LoopbackIdentity: loopbackIdentityFor("X-MCP-User", map[string][]string{
				"writer": {"tool:allowed"},
			}),
		},
		DisableRequestLog: true,
	})

	// httptest.Server is loopback-only: the client connects from
	// 127.0.0.1, so it is admitted via the bypass without any bearer token.
	// The X-MCP-User header stands in for a trusted proxy's identity.
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		r := req.Clone(req.Context())
		r.Header.Set("X-MCP-User", "writer")
		return ts.Client().Transport.RoundTrip(r)
	})}
	transport := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           client,
		DisableStandaloneSSE: true,
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "lb-client", Version: "0.0.1"}, nil).
		Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "allowed" {
		names := make([]string, 0, len(list.Tools))
		for _, tool := range list.Tools {
			names = append(names, tool.Name)
		}
		t.Fatalf("tools/list = %v, want [allowed]", names)
	}

	denied, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "denied"})
	if err != nil {
		t.Fatalf("call denied: %v", err)
	}
	if !denied.IsError {
		t.Error("tools/call to a filtered tool must be denied, not just hidden from list")
	}

	allowed, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "allowed"})
	if err != nil {
		t.Fatalf("call allowed: %v", err)
	}
	if allowed.IsError {
		t.Error("tools/call to an allowed tool was denied")
	}
}

// LoopbackIdentity errors must reject the bypassed request — an identity
// the proxy sent but we don't recognise is not "anonymous", it is denied.
func TestLoopbackBypass_IdentityErrorRejects(t *testing.T) {
	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-loopback-deny",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:       validVerifier,
			LoopbackBypass: true,
			ToolFilter:     scopeFilter,
			LoopbackIdentity: func(_ *http.Request) (*TokenInfo, error) {
				return nil, auth.ErrInvalidToken
			},
		},
		DisableRequestLog: true,
	})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for rejected loopback identity", resp.StatusCode)
	}
}

// Without LoopbackIdentity the pre-existing behaviour holds: bypassed
// requests carry nil TokenInfo and a deny-on-nil filter denies everything —
// fail closed, not open.
func TestLoopbackBypass_NilIdentity_FailClosed(t *testing.T) {
	server := newFilterTestServer(t)
	ts := NewTestServer(t, server, Config{
		Name:    "filter-loopback-nil",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier:       validVerifier,
			LoopbackBypass: true,
			ToolFilter:     scopeFilter, // denies when info == nil
		},
		DisableRequestLog: true,
	})

	sess := connectMCP(t, ts, "")
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(list.Tools) != 0 {
		t.Errorf("nil identity must not see tools, got %d", len(list.Tools))
	}
	result, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "allowed"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Error("nil identity must not call tools")
	}
}

// The bypass must not become a privilege for non-loopback callers:
// a request whose RemoteAddr is not loopback still goes through the real
// verifier, even with LoopbackIdentity configured.
func TestLoopbackBypass_NonLoopbackStillAuthed(t *testing.T) {
	var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := applyBearerAuth(handler, &BearerAuth{
		Verifier:       validVerifier,
		LoopbackBypass: true,
		LoopbackIdentity: func(_ *http.Request) (*TokenInfo, error) {
			return &auth.TokenInfo{UserID: "loopback", Scopes: []string{"tool:allowed"}}, nil
		},
	})

	remote := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	remote.RemoteAddr = "203.0.113.9:4412"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, remote)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-loopback without token: %d, want 401", rec.Code)
	}

	remote = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	remote.RemoteAddr = "203.0.113.9:4412"
	remote.Header.Set("Authorization", "Bearer valid-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, remote)
	if rec.Code != http.StatusOK {
		t.Errorf("non-loopback with token: %d, want 200", rec.Code)
	}

	local := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	local.RemoteAddr = "127.0.0.1:4412"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, local)
	if rec.Code != http.StatusOK {
		t.Errorf("loopback bypassed: %d, want 200", rec.Code)
	}
}

// LoopbackIdentity must reach the handler's context too — handlers reading
// auth.TokenInfoFromContext see the same identity the filter saw.
func TestLoopbackBypass_IdentityReachesHandler(t *testing.T) {
	var got *auth.TokenInfo
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = auth.TokenInfoFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := applyBearerAuth(inner, &BearerAuth{
		Verifier:       validVerifier,
		LoopbackBypass: true,
		LoopbackIdentity: func(r *http.Request) (*TokenInfo, error) {
			return &auth.TokenInfo{UserID: r.Header.Get("X-MCP-User"), Scopes: []string{"s"}}, nil
		},
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
	req.RemoteAddr = "127.0.0.1:4412"
	req.Header.Set("X-MCP-User", "writer")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got == nil || got.UserID != "writer" {
		t.Fatalf("TokenInfoFromContext = %+v, want UserID writer", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
