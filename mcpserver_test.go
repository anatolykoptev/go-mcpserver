package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
		mws := buildMiddleware(Config{CORSOrigins: []string{"*"}}, logger)
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
