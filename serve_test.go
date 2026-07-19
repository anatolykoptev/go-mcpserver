package mcpserver

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var registered atomic.Bool
	cfg := Config{
		Name:              "serve-test",
		Version:           "0.0.1",
		Port:              "0",
		Context:           ctx,
		DisableRequestLog: true,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(
			&mcp.Implementation{Name: "serve-test", Version: "0.0.1"},
			cfg,
			func(s *mcp.Server) {
				mcp.AddTool(s, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
				})
				registered.Store(true)
			},
		)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s")
	}

	if !registered.Load() {
		t.Error("register callback was not invoked")
	}
}

func TestConfigWithoutServerOpts(t *testing.T) {
	cache := mcp.NewSchemaCache()
	cfg := Config{
		Name:         "x",
		Version:      "0.0.1",
		Port:         "8080",
		JSONResponse: true,
		ToolTimeout:  5 * time.Second,
		KeepAlive:    30 * time.Second,
		SchemaCache:  cache,
	}

	stripped := cfg.withoutServerOpts()

	if stripped.KeepAlive != 0 {
		t.Errorf("KeepAlive = %v, want 0", stripped.KeepAlive)
	}
	if stripped.SchemaCache != nil {
		t.Error("SchemaCache should be nil after withoutServerOpts")
	}
	if stripped.Name != cfg.Name || stripped.Port != cfg.Port || stripped.JSONResponse != cfg.JSONResponse || stripped.ToolTimeout != cfg.ToolTimeout {
		t.Error("withoutServerOpts mutated unrelated fields")
	}
	if cfg.KeepAlive != 30*time.Second || cfg.SchemaCache != cache {
		t.Error("withoutServerOpts mutated the receiver")
	}
}

func TestWarnIgnoredServerOpts(t *testing.T) {
	impl := &mcp.Implementation{Name: "warn-test", Version: "0.0.1"}
	server := mcp.NewServer(impl, nil)

	mustWarn := func(t *testing.T, cfg Config) {
		t.Helper()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		cfg.Logger = logger

		if _, err := Build(server, cfg); err != nil {
			t.Fatalf("Build error: %v", err)
		}
		if !strings.Contains(buf.String(), "Config.KeepAlive/SchemaCache") {
			t.Errorf("expected warning, got: %s", buf.String())
		}
	}

	mustNotWarn := func(t *testing.T, cfg Config) {
		t.Helper()
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
		cfg.Logger = logger

		if _, err := Build(server, cfg); err != nil {
			t.Fatalf("Build error: %v", err)
		}
		if strings.Contains(buf.String(), "Config.KeepAlive/SchemaCache") {
			t.Errorf("unexpected warning: %s", buf.String())
		}
	}

	t.Run("SchemaCache warns", func(t *testing.T) {
		mustWarn(t, Config{Name: "warn-test", Version: "0.0.1", SchemaCache: mcp.NewSchemaCache()})
	})

	t.Run("KeepAlive warns", func(t *testing.T) {
		mustWarn(t, Config{Name: "warn-test", Version: "0.0.1", KeepAlive: 30 * time.Second})
	})

	t.Run("neither does not warn", func(t *testing.T) {
		mustNotWarn(t, Config{Name: "warn-test", Version: "0.0.1"})
	})
}

func TestNewServerDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	cfg := Config{
		Name:        "newserver-nowarn",
		Version:     "0.0.1",
		KeepAlive:   30 * time.Second,
		SchemaCache: mcp.NewSchemaCache(),
	}
	_ = NewServer(&mcp.Implementation{Name: "newserver-nowarn", Version: "0.0.1"}, cfg)

	if strings.Contains(buf.String(), "Config.KeepAlive/SchemaCache") {
		t.Errorf("NewServer should not emit the Run/Build warning, got: %s", buf.String())
	}
}
