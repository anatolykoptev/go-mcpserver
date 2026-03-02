package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// === Health JSON injection tests ===

func TestHealthJSONSpecialChars(t *testing.T) {
	tests := []struct {
		name    string
		svcName string
		version string
	}{
		{"quotes in name", `my"service`, "1.0.0"},
		{"backslash in name", `my\service`, "1.0.0"},
		{"angle brackets", `<script>alert(1)</script>`, "1.0.0"},
		{"newline in version", "svc", "1.0\n.0"},
		{"unicode in name", "сервис", "1.0.0"},
		{"null byte in name", "svc\x00evil", "1.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registerHealth(mux, Config{Name: tt.svcName, Version: tt.version})

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			// The critical test: must be valid JSON.
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON for name=%q version=%q: %v\nbody: %s",
					tt.svcName, tt.version, err, rec.Body.String())
			}

			// Verify round-trip: values must survive encoding/decoding.
			if body["service"] != tt.svcName {
				t.Errorf("service = %q, want %q", body["service"], tt.svcName)
			}
			if body["version"] != tt.version {
				t.Errorf("version = %q, want %q", body["version"], tt.version)
			}
		})
	}
}

func TestHealthReadyErrorSpecialChars(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
	}{
		{"quotes in error", `connection "refused"`},
		{"backslash", `path\to\db`},
		{"newline", "line1\nline2"},
		{"unicode", "ошибка подключения"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			cfg := Config{
				Name:    "svc",
				Version: "1.0.0",
				ReadinessCheck: func() error {
					return errors.New(tt.errMsg)
				},
			}
			registerHealth(mux, cfg)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON for error=%q: %v\nbody: %s",
					tt.errMsg, err, rec.Body.String())
			}
			if body["error"] != tt.errMsg {
				t.Errorf("error = %q, want %q", body["error"], tt.errMsg)
			}
		})
	}
}

// === Config validation edge cases ===

func TestValidateEdgeCases(t *testing.T) {
	t.Run("both empty returns Name error first", func(t *testing.T) {
		err := validate(Config{})
		if err == nil {
			t.Fatal("expected error for empty config")
		}
		if got := err.Error(); got != "mcpserver: Config.Name is required" {
			t.Errorf("error = %q, want Name error", got)
		}
	})

	t.Run("Run rejects empty config", func(t *testing.T) {
		server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		err := Run(server, Config{})
		if err == nil {
			t.Fatal("Run should reject empty config")
		}
	})

	t.Run("Run rejects missing Version", func(t *testing.T) {
		server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		err := Run(server, Config{Name: "svc"})
		if err == nil {
			t.Fatal("Run should reject config without Version")
		}
	})
}

// === Context edge cases ===

func TestRunWithAlreadyCancelledContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name: "cancelled", Version: "0.0.1",
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel BEFORE Run.

	shutdownCalled := make(chan struct{})
	cfg := Config{
		Name:              "cancelled",
		Version:           "0.0.1",
		Port:              "19878",
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

	// Should shut down almost immediately since context is already done.
	select {
	case <-shutdownCalled:
		// Good — OnShutdown was called.
	case <-time.After(5 * time.Second):
		t.Fatal("OnShutdown not called within 5s for pre-cancelled context")
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

// === CORS edge cases ===

func TestCORSEdgeCases(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("empty Origins allows nothing", func(t *testing.T) {
		handler := CORS(CORSConfig{Origins: []string{}})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://example.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin should be empty for empty Origins, got %q", got)
		}
	})

	t.Run("Max-Age on GET request (not just preflight)", func(t *testing.T) {
		handler := CORS(CORSConfig{Origins: []string{"*"}, MaxAge: 600})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://example.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// Max-Age is set on all CORS responses, not just preflight.
		got := rec.Header().Get("Access-Control-Max-Age")
		if got != "600" {
			t.Errorf("Max-Age on GET = %q, want %q", got, "600")
		}
	})

	t.Run("Vary header for specific origin on preflight", func(t *testing.T) {
		handler := CORS(CORSConfig{Origins: []string{"https://a.com"}})(inner)

		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", "https://a.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("Vary = %q, want %q", got, "Origin")
		}
	})

	t.Run("wildcard does not set Vary", func(t *testing.T) {
		handler := CORS(CORSConfig{Origins: []string{"*"}})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://a.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Vary"); got != "" {
			t.Errorf("Vary should be empty for wildcard, got %q", got)
		}
	})
}

// === responseWriter edge cases ===

func TestResponseWriterDoubleWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, status: http.StatusOK}

	// First WriteHeader sets status.
	rw.WriteHeader(http.StatusCreated)
	if rw.status != http.StatusCreated {
		t.Errorf("status after first WriteHeader = %d, want %d", rw.status, http.StatusCreated)
	}

	// Second WriteHeader is ignored (wroteHeader guard).
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusCreated {
		t.Errorf("status after second WriteHeader = %d, want %d (should not change)", rw.status, http.StatusCreated)
	}
}

func TestResponseWriterImplicitWriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, status: http.StatusOK}

	// Write without explicit WriteHeader should trigger implicit 200.
	_, _ = rw.Write([]byte("hello"))

	if rw.status != http.StatusOK {
		t.Errorf("status = %d, want %d after implicit write", rw.status, http.StatusOK)
	}
	if !rw.wroteHeader {
		t.Error("wroteHeader should be true after Write")
	}
}
