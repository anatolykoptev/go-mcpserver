package mcpserver

import (
	"encoding/json"
	"log/slog"
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

func TestHealthHandler(t *testing.T) {
	cfg := Config{Name: "test-svc", Version: "1.2.3"}
	healthBody := `{"status":"ok","service":"` + cfg.Name + `","version":"` + cfg.Version + `"}`

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(healthBody))
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
	if body["service"] != "test-svc" {
		t.Errorf("service = %q, want test-svc", body["service"])
	}
	if body["version"] != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", body["version"])
	}
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

func TestRecoveryMiddleware(t *testing.T) {
	t.Run("panic returns 500", func(t *testing.T) {
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic("test panic")
		})
		handler := recoveryMiddleware(inner, defaultLogger())

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("no panic passes through", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		handler := recoveryMiddleware(inner, defaultLogger())

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
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
		Name:    "test-server",
		Version: "0.0.1",
		Port:    "0", // let OS pick a free port
		OnShutdown: func() {
			close(shutdownCalled)
		},
	}

	// Port 0 won't work with our Run (it uses ":0" and we can't discover the port).
	// Use a high random port instead.
	cfg.Port = "19876"

	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(server, cfg)
	}()

	// Wait for server to start.
	var lastErr error
	for range 50 {
		time.Sleep(50 * time.Millisecond)
		resp, err := http.Get("http://127.0.0.1:19876/health") //nolint:noctx // test code
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
	resp, err := http.Get("http://127.0.0.1:19876/health") //nolint:noctx // test code
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

	// Send SIGINT to trigger shutdown.
	p, _ := os.FindProcess(os.Getpid())
	_ = p.Signal(syscall.SIGINT)

	select {
	case <-shutdownCalled:
		// ok
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

func defaultLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
