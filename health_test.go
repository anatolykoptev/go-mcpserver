package mcpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterHealth(t *testing.T) {
	t.Run("default health endpoint", func(t *testing.T) {
		mux := http.NewServeMux()
		cfg := Config{Name: "my-svc", Version: "2.0.0"}
		registerHealth(mux, cfg)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
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
		if body["service"] != "my-svc" {
			t.Errorf("service = %q, want my-svc", body["service"])
		}
		if body["version"] != "2.0.0" {
			t.Errorf("version = %q, want 2.0.0", body["version"])
		}
	})

	t.Run("liveness always 200", func(t *testing.T) {
		mux := http.NewServeMux()
		registerHealth(mux, Config{Name: "svc", Version: "1.0.0"})

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %q, want ok", body["status"])
		}
	})

	t.Run("readiness OK when no check", func(t *testing.T) {
		mux := http.NewServeMux()
		registerHealth(mux, Config{Name: "svc", Version: "1.0.0"})

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}

		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %q, want ok", body["status"])
		}
	})

	t.Run("readiness 503 when check fails", func(t *testing.T) {
		mux := http.NewServeMux()
		cfg := Config{
			Name:    "svc",
			Version: "1.0.0",
			ReadinessCheck: func() error {
				return errors.New("db connection lost")
			},
		}
		registerHealth(mux, cfg)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}

		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if body["status"] != "unavailable" {
			t.Errorf("status = %q, want unavailable", body["status"])
		}
		if body["error"] != "db connection lost" {
			t.Errorf("error = %q, want %q", body["error"], "db connection lost")
		}
	})

	t.Run("disabled skips all endpoints", func(t *testing.T) {
		mux := http.NewServeMux()
		cfg := Config{
			Name:          "svc",
			Version:       "1.0.0",
			DisableHealth: true,
		}
		registerHealth(mux, cfg)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
		}
	})
}
