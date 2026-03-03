package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewTestServer(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ts-test", Version: "0.0.1"}, nil)
	ts := NewTestServer(t, server, Config{
		Name:              "ts-test",
		Version:           "0.0.1",
		DisableRequestLog: true,
	})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health status = %d, want 200", resp.StatusCode)
	}
}

func TestNewTestServerWithAuth(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ts-auth", Version: "0.0.1"}, nil)
	ts := NewTestServer(t, server, Config{
		Name:    "ts-auth",
		Version: "0.0.1",
		BearerAuth: &BearerAuth{
			Verifier: StaticTokenVerifier("test-secret"),
		},
		DisableRequestLog: true,
	})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/mcp without token: status = %d, want 401", resp.StatusCode)
	}
}
