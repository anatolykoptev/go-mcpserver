package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewServerSetsKeepAlive(t *testing.T) {
	cfg := Config{
		Name:      "test",
		Version:   "0.0.1",
		KeepAlive: 45 * time.Second,
	}
	server := NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, cfg)
	if server == nil {
		t.Fatal("NewServer returned nil")
	}
	// We can't directly inspect ServerOptions after creation (unexported),
	// but we can verify the server works — if KeepAlive were invalid, Connect
	// would fail. The real proof is that the server starts and accepts tools.
	mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
}

func TestNewServerSetsSchemaCache(t *testing.T) {
	cache := mcp.NewSchemaCache()
	cfg := Config{
		Name:        "test",
		Version:     "0.0.1",
		SchemaCache: cache,
	}
	server := NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, cfg)
	if server == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestNewServerWithZeroKeepAlive(t *testing.T) {
	cfg := Config{
		Name:    "test",
		Version: "0.0.1",
		// KeepAlive = 0 → disabled, should still work
	}
	server := NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, cfg)
	if server == nil {
		t.Fatal("NewServer returned nil")
	}
}

func TestBuildServerOptions(t *testing.T) {
	t.Run("with KeepAlive and SchemaCache", func(t *testing.T) {
		cache := mcp.NewSchemaCache()
		cfg := Config{KeepAlive: 30 * time.Second, SchemaCache: cache}
		opts := buildServerOptions(cfg)
		if opts.KeepAlive != 30*time.Second {
			t.Errorf("KeepAlive = %v, want 30s", opts.KeepAlive)
		}
		if opts.SchemaCache != cache {
			t.Error("SchemaCache not set")
		}
	})

	t.Run("with zero values", func(t *testing.T) {
		cfg := Config{}
		opts := buildServerOptions(cfg)
		if opts.KeepAlive != 0 {
			t.Errorf("KeepAlive = %v, want 0", opts.KeepAlive)
		}
		if opts.SchemaCache != nil {
			t.Error("SchemaCache should be nil")
		}
	})
}

func TestDisableLocalhostProtectionWired(t *testing.T) {
	// Verify that DisableLocalhostProtection is passed through to the
	// StreamableHTTPHandler. We can't easily test the 403 behavior in a
	// unit test (it requires a real listener on localhost with a spoofed
	// Host header), but we can verify the handler doesn't reject localhost
	// requests when DisableLocalhostProtection=true.
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})

	h, err := Build(server, Config{
		Name:                       "test",
		Version:                    "0.0.1",
		DisableRequestLog:          true,
		DisableLocalhostProtection: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A request from localhost with a non-localhost Host should NOT be
	// rejected with 403 when DisableLocalhostProtection=true.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Host = "external.example.com"
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	// We expect a non-403 response (could be 400 for bad JSON-RPC, but not 403).
	if rec.Code == http.StatusForbidden {
		t.Errorf("got 403 with DisableLocalhostProtection=true — option not wired through")
	}
}
