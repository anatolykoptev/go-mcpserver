package mcpserver

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestChain(t *testing.T) {
	var order []string

	mwA := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "A-before")
			next.ServeHTTP(w, r)
			order = append(order, "A-after")
		})
	}
	mwB := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "B-before")
			next.ServeHTTP(w, r)
			order = append(order, "B-after")
		})
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		order = append(order, "handler")
		w.WriteHeader(http.StatusOK)
	})

	handler := Chain(inner, Middleware(mwA), Middleware(mwB))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	// First middleware (mwA) is outermost: A wraps B wraps handler.
	want := "A-before,B-before,handler,B-after,A-after"
	got := strings.Join(order, ",")
	if got != want {
		t.Errorf("execution order = %q, want %q", got, want)
	}
}

func TestChainEmpty(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	handler := Chain(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestRecovery(t *testing.T) {
	t.Run("panic returns 500", func(t *testing.T) {
		inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic("boom")
		})
		handler := Recovery(testLogger())(inner)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/explode", nil))

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("no panic passes through", func(t *testing.T) {
		inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
		})
		handler := Recovery(testLogger())(inner)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))

		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
		}
	})
}

func TestRequestID(t *testing.T) {
	t.Run("generates ID when missing", func(t *testing.T) {
		var capturedID string
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedID = RequestIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		handler := RequestID()(inner)

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if capturedID == "" {
			t.Fatal("expected generated request ID, got empty string")
		}
		// 16 bytes → 32 hex chars.
		if len(capturedID) != 32 {
			t.Errorf("request ID length = %d, want 32", len(capturedID))
		}
		// Must also be in response header.
		if got := rec.Header().Get("X-Request-ID"); got != capturedID {
			t.Errorf("response header X-Request-ID = %q, want %q", got, capturedID)
		}
	})

	t.Run("propagates existing ID", func(t *testing.T) {
		var capturedID string
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedID = RequestIDFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		handler := RequestID()(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Request-ID", "existing-id-123")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if capturedID != "existing-id-123" {
			t.Errorf("capturedID = %q, want %q", capturedID, "existing-id-123")
		}
		if got := rec.Header().Get("X-Request-ID"); got != "existing-id-123" {
			t.Errorf("response header = %q, want %q", got, "existing-id-123")
		}
	})

	t.Run("bare context returns empty", func(t *testing.T) {
		id := RequestIDFromContext(context.Background())
		if id != "" {
			t.Errorf("RequestIDFromContext on bare context = %q, want empty", id)
		}
	})
}

func TestRequestLog(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	})
	handler := RequestLog(testLogger())(inner)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/test", nil))

	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "ok")
	}
}

func TestResponseWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec}

	unwrapped := rw.Unwrap()
	if unwrapped != rec {
		t.Errorf("Unwrap() returned %T, want *httptest.ResponseRecorder", unwrapped)
	}
}

func TestCORS(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	t.Run("allow all origins", func(t *testing.T) {
		handler := CORS([]string{"*"})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://example.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin = %q, want %q", got, "*")
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})

	t.Run("specific origin allowed", func(t *testing.T) {
		handler := CORS([]string{"https://allowed.com", "https://other.com"})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://allowed.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.com" {
			t.Errorf("Allow-Origin = %q, want %q", got, "https://allowed.com")
		}
	})

	t.Run("origin not allowed", func(t *testing.T) {
		handler := CORS([]string{"https://allowed.com"})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Origin", "https://evil.com")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin should be empty for disallowed origin, got %q", got)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d (handler still executes)", rec.Code, http.StatusOK)
		}
	})

	t.Run("preflight returns 204", func(t *testing.T) {
		handler := CORS([]string{"*"})(inner)

		req := httptest.NewRequest(http.MethodOptions, "/", nil)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); got == "" {
			t.Error("preflight should set Access-Control-Allow-Methods")
		}
	})

	t.Run("no Origin header skips CORS headers", func(t *testing.T) {
		handler := CORS([]string{"*"})(inner)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		// No Origin header.

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Allow-Origin should be empty without Origin, got %q", got)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
		}
	})
}
