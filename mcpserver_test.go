package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestValidate(t *testing.T) {
	t.Run("empty Name returns error", func(t *testing.T) {
		err := validate(Config{Version: "1.0.0"})
		if err == nil {
			t.Fatal("expected error for empty Name")
		}
		if !strings.Contains(err.Error(), "Name") {
			t.Errorf("error = %q, want mention of Name", err)
		}
	})

	t.Run("empty Version returns error", func(t *testing.T) {
		err := validate(Config{Name: "svc"})
		if err == nil {
			t.Fatal("expected error for empty Version")
		}
		if !strings.Contains(err.Error(), "Version") {
			t.Errorf("error = %q, want mention of Version", err)
		}
	})

	t.Run("valid config passes", func(t *testing.T) {
		err := validate(Config{Name: "svc", Version: "1.0.0"})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestIsStdio(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"no args", []string{"binary"}, false},
		{"unrelated", []string{"binary", "--port", "8080"}, false},
		{"stdio flag", []string{"binary", "--stdio"}, true},
		{"stdio with other flags", []string{"binary", "--verbose", "--stdio", "--debug"}, true},
		{"stdio as value not flag", []string{"binary", "--mode", "--stdio-mode"}, false},
		{"stdio equals form", []string{"binary", "--stdio=true"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origArgs := os.Args
			t.Cleanup(func() { os.Args = origArgs })

			os.Args = tt.args
			if got := isStdio(); got != tt.want {
				t.Errorf("isStdio() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWithDefaults(t *testing.T) {
	t.Run("zero values", func(t *testing.T) {
		cfg := withDefaults(Config{})
		if cfg.Port != defaultPort {
			t.Errorf("Port = %q, want %q", cfg.Port, defaultPort)
		}
		if cfg.ReadTimeout != defaultReadTimeout {
			t.Errorf("ReadTimeout = %v, want %v", cfg.ReadTimeout, defaultReadTimeout)
		}
		if cfg.WriteTimeout != defaultWriteTimeout {
			t.Errorf("WriteTimeout = %v, want %v", cfg.WriteTimeout, defaultWriteTimeout)
		}
		if cfg.ShutdownTimeout != defaultShutdownTimeout {
			t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, defaultShutdownTimeout)
		}
	})

	t.Run("MCP_PORT env", func(t *testing.T) {
		t.Setenv(portEnvVar, "9999")
		cfg := withDefaults(Config{})
		if cfg.Port != "9999" {
			t.Errorf("Port = %q, want %q", cfg.Port, "9999")
		}
	})

	t.Run("explicit port overrides env", func(t *testing.T) {
		t.Setenv(portEnvVar, "9999")
		cfg := withDefaults(Config{Port: "7777"})
		if cfg.Port != "7777" {
			t.Errorf("Port = %q, want %q", cfg.Port, "7777")
		}
	})

	t.Run("custom timeouts preserved", func(t *testing.T) {
		cfg := withDefaults(Config{
			ReadTimeout:     5 * time.Second,
			WriteTimeout:    600 * time.Second,
			ShutdownTimeout: 30 * time.Second,
		})
		if cfg.ReadTimeout != 5*time.Second {
			t.Errorf("ReadTimeout = %v, want 5s", cfg.ReadTimeout)
		}
		if cfg.WriteTimeout != 600*time.Second {
			t.Errorf("WriteTimeout = %v, want 600s", cfg.WriteTimeout)
		}
		if cfg.ShutdownTimeout != 30*time.Second {
			t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
		}
	})
}

func TestMetricsHandler(t *testing.T) {
	metricsText := "requests_total 42\nerrors_total 0"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(metricsText))
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if rec.Body.String() != metricsText {
		t.Errorf("body = %q, want %q", rec.Body.String(), metricsText)
	}
}

func TestBuildMiddleware(t *testing.T) {
	t.Run("default config builds 3 middleware", func(t *testing.T) {
		logger := testLogger()
		mws := buildMiddleware(Config{}, logger)
		// recovery + requestID + requestLog = 3
		if len(mws) != 3 {
			t.Errorf("len(mws) = %d, want 3", len(mws))
		}
	})

	t.Run("all disabled builds 1 middleware", func(t *testing.T) {
		logger := testLogger()
		mws := buildMiddleware(Config{
			DisableRecovery:   true,
			DisableRequestLog: true,
		}, logger)
		// only requestID
		if len(mws) != 1 {
			t.Errorf("len(mws) = %d, want 1", len(mws))
		}
	})

	t.Run("CORS adds middleware", func(t *testing.T) {
		logger := testLogger()
		mws := buildMiddleware(Config{CORSOrigins: []string{"*"}, CORSMaxAge: 3600}, logger)
		// recovery + requestID + requestLog + CORS = 4
		if len(mws) != 4 {
			t.Errorf("len(mws) = %d, want 4", len(mws))
		}
	})

	t.Run("custom middleware appended", func(t *testing.T) {
		logger := testLogger()
		noop := func(next http.Handler) http.Handler { return next }
		mws := buildMiddleware(Config{Middleware: []Middleware{noop}}, logger)
		// recovery + requestID + requestLog + custom = 4
		if len(mws) != 4 {
			t.Errorf("len(mws) = %d, want 4", len(mws))
		}
	})
}

func TestRunWithExternalContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "ctx-server",
		Version: "0.0.1",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	shutdownCalled := make(chan struct{})
	cfg := Config{
		Name:              "ctx-server",
		Version:           "0.0.1",
		Port:              "19877",
		Context:           ctx,
		DisableRequestLog: true,
		OnShutdown: func() {
			close(shutdownCalled)
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(server, cfg)
	}()

	// Wait for server to start.
	for range 50 {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:19877/health") //nolint:noctx
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
	}

	// Cancel context instead of sending SIGINT.
	cancel()

	select {
	case <-shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("OnShutdown was not called within 5s after context cancel")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
}

func TestRunBindFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	// Start first server to occupy a port.
	server1 := mcp.NewServer(&mcp.Implementation{
		Name: "blocker", Version: "0.0.1",
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh1 := make(chan error, 1)
	go func() {
		errCh1 <- Run(server1, Config{
			Name:              "blocker",
			Version:           "0.0.1",
			Port:              "19879",
			Context:           ctx,
			DisableRequestLog: true,
		})
	}()

	// Wait for first server to start.
	for range 50 {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:19879/health") //nolint:noctx
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
	}

	// Try to start second server on the same port — should return error.
	server2 := mcp.NewServer(&mcp.Implementation{
		Name: "conflict", Version: "0.0.1",
	}, nil)

	err := Run(server2, Config{
		Name:              "conflict",
		Version:           "0.0.1",
		Port:              "19879",
		Context:           context.Background(),
		DisableRequestLog: true,
	})
	if err == nil {
		t.Fatal("Run should return error when port is occupied")
	}
	if !strings.Contains(err.Error(), "bind") && !strings.Contains(err.Error(), "address already in use") {
		t.Errorf("unexpected error: %v", err)
	}

	cancel()
	<-errCh1
}

func TestBuild(t *testing.T) {
	t.Run("invalid config returns error", func(t *testing.T) {
		_, err := Build(nil, Config{})
		if err == nil {
			t.Fatal("expected error for invalid config")
		}
	})

	t.Run("nil server without DisableMCP returns error", func(t *testing.T) {
		_, err := Build(nil, Config{Name: "svc", Version: "1.0.0"})
		if err == nil {
			t.Fatal("expected error for nil server")
		}
		if !strings.Contains(err.Error(), "server must not be nil") {
			t.Errorf("error = %q, want mention of nil server", err)
		}
	})

	t.Run("DisableMCP with nil server works", func(t *testing.T) {
		h, err := Build(nil, Config{
			Name:       "svc",
			Version:    "1.0.0",
			DisableMCP: true,
		})
		if err != nil {
			t.Fatalf("Build error: %v", err)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("/health status = %d, want 200", rec.Code)
		}
	})

	t.Run("custom Routes appear in handler", func(t *testing.T) {
		h, err := Build(nil, Config{
			Name:       "svc",
			Version:    "1.0.0",
			DisableMCP: true,
			Routes: func(mux *http.ServeMux) {
				mux.HandleFunc("GET /custom", func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusTeapot)
				})
			},
		})
		if err != nil {
			t.Fatalf("Build error: %v", err)
		}

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/custom", nil))
		if rec.Code != http.StatusTeapot {
			t.Errorf("/custom status = %d, want 418", rec.Code)
		}
	})
}

func TestRunDisableMCP(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := Config{
		Name:              "no-mcp",
		Version:           "0.0.1",
		Port:              "19880",
		Context:           ctx,
		DisableMCP:        true,
		DisableRequestLog: true,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- Run(nil, cfg) }()

	// Wait for server to start.
	for range 50 {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:19880/health") //nolint:noctx
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			break
		}
	}

	// /health should work.
	resp, err := http.Get("http://127.0.0.1:19880/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/health status = %d, want 200", resp.StatusCode)
	}

	// /mcp should return 404 (not registered).
	resp, err = http.Post("http://127.0.0.1:19880/mcp", "application/json", nil) //nolint:noctx
	if err != nil {
		t.Fatalf("POST /mcp failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/mcp status = %d, want 404", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
}

func TestRunNilServerReturnsError(t *testing.T) {
	err := Run(nil, Config{Name: "svc", Version: "1.0.0"})
	if err == nil {
		t.Fatal("expected error for nil server without DisableMCP")
	}
	if !strings.Contains(err.Error(), "server must not be nil") {
		t.Errorf("error = %q, want mention of nil server", err)
	}
}

func TestMCPMiddlewareApplied(t *testing.T) {
	var called atomic.Bool
	mw := func(h mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			called.Store(true)
			return h(ctx, method, req)
		}
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "mw-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:                  "mw-test",
		Version:               "0.0.1",
		MCPReceivingMiddleware: []mcp.Middleware{mw},
		DisableRequestLog:     true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0.1"}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.ServeHTTP(rec, req)

	if !called.Load() {
		t.Error("MCP receiving middleware was not called")
	}
}

func TestBuildStreamableHTTPOptions(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "opts-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:              "opts-test",
		Version:           "0.0.1",
		JSONResponse:      true,
		SessionTimeout:    5 * time.Minute,
		DisableRequestLog: true,
	})
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0.1"}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	h.ServeHTTP(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (JSONResponse=true)", ct)
	}
}

func TestRunIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "test-server",
		Version: "0.0.1",
	}, nil)

	shutdownCalled := make(chan struct{})
	cfg := Config{
		Name:              "test-server",
		Version:           "0.0.1",
		Port:              "19876",
		DisableRequestLog: true, // suppress log noise in tests
		OnShutdown: func() {
			close(shutdownCalled)
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(server, cfg)
	}()

	// Wait for server to start.
	var lastErr error
	for range 50 {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:19876/health") //nolint:noctx
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			lastErr = nil
			break
		}
	}
	if lastErr != nil {
		t.Fatalf("server did not start: %v", lastErr)
	}

	// Verify health endpoint.
	resp, err := http.Get("http://127.0.0.1:19876/health") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health failed: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	if body["service"] != "test-server" {
		t.Errorf("service = %q, want test-server", body["service"])
	}

	// Verify X-Request-ID header is present.
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header missing from response")
	}

	// Verify liveness endpoint.
	liveResp, err := http.Get("http://127.0.0.1:19876/health/live") //nolint:noctx
	if err != nil {
		t.Fatalf("GET /health/live failed: %v", err)
	}
	liveResp.Body.Close()
	if liveResp.StatusCode != http.StatusOK {
		t.Errorf("/health/live status = %d, want %d", liveResp.StatusCode, http.StatusOK)
	}

	// Send SIGINT to trigger shutdown.
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)

	select {
	case <-shutdownCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("OnShutdown was not called within 5s")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s")
	}
}
