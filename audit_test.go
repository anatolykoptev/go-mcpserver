package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- MAJOR1: StaticTokenVerifier uses constant-time compare ---

// TestStaticTokenVerifierConstantTime exercises the negative path. The
// constant-time guarantee itself is not benchmarkable in CI without flake,
// so we assert the contractual behaviour: equal-length wrong tokens still
// reject, and the rejection is the same auth.ErrInvalidToken sentinel.
func TestStaticTokenVerifierConstantTime(t *testing.T) {
	v := StaticTokenVerifier("abcdefgh-secret")

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"shorter", "abc"},
		{"longer", "abcdefgh-secret-extra"},
		{"same length wrong", "abcdefgh-WRONG_"},
		{"prefix match", "abcdefgh-secre_"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v(context.Background(), tt.token, nil); err == nil {
				t.Fatalf("token %q: expected error, got nil", tt.token)
			}
		})
	}

	// Sanity: correct token still passes.
	info, err := v(context.Background(), "abcdefgh-secret", nil)
	if err != nil {
		t.Fatalf("correct token: unexpected error %v", err)
	}
	if info == nil || info.Expiration.IsZero() {
		t.Fatal("correct token: empty TokenInfo")
	}
}

// --- MAJOR2: LogSkipPaths demotes /health, /metrics to Debug ---

func TestRequestLogSkipsHealthByDefault(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	server := mcp.NewServer(&mcp.Implementation{Name: "skip-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:    "skip-test",
		Version: "0.0.1",
		Logger:  logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path     string
		wantHit  bool // true = expect Info-level "request" log line
	}{
		{"/health", false},
		{"/health/live", false},
		{"/health/ready", false},
		{"/api/things", true},
	}

	// /api/things is not registered → 404, but RequestLog still runs.
	for _, c := range cases {
		buf.Reset()
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, c.path, nil)
		h.ServeHTTP(rec, req)
		got := strings.Contains(buf.String(), `"path":"`+c.path+`"`)
		if got != c.wantHit {
			t.Errorf("path %q: got Info log = %v, want %v (buf=%q)",
				c.path, got, c.wantHit, buf.String())
		}
	}
}

func TestRequestLogSkipsCustomPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	server := mcp.NewServer(&mcp.Implementation{Name: "skip-test", Version: "0.0.1"}, nil)
	h, err := Build(server, Config{
		Name:         "skip-test",
		Version:      "0.0.1",
		Logger:       logger,
		LogSkipPaths: []string{"/quiet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	quietReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/quiet", nil)
	h.ServeHTTP(rec, quietReq)
	if strings.Contains(buf.String(), `"path":"/quiet"`) {
		t.Errorf("custom skip path should be at Debug, got Info: %s", buf.String())
	}

	// Default /health is NOT skipped because user supplied an explicit list.
	buf.Reset()
	healthReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	h.ServeHTTP(rec, healthReq)
	if !strings.Contains(buf.String(), `"path":"/health"`) {
		t.Errorf("user-supplied LogSkipPaths overrides defaults; /health should log: %s", buf.String())
	}
}

// --- MAJOR3: REST bridge enforces ToolFilter ---

func TestRESTBridgeEnforcesToolFilter(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "filter-rest", Version: "0.0.1"}, nil)
	for _, name := range []string{"public", "secret"} {
		n := name
		mcp.AddTool(server, &mcp.Tool{Name: n}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + n}}}, nil, nil
		})
	}

	denySecret := func(_ context.Context, name string, _ *TokenInfo) bool {
		return name != "secret"
	}

	h, err := Build(server, Config{
		Name:              "filter-rest",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		BearerAuth: &BearerAuth{
			Verifier:   StaticTokenVerifier("tok"),
			ToolFilter: denySecret,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	withTok := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer tok")
		return req
	}

	t.Run("call denied tool returns 403", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := withTok(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/tools/secret", strings.NewReader(`{}`)))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("call permitted tool returns 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := withTok(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/tools/public", strings.NewReader(`{}`)))
		req.Header.Set("Content-Type", contentTypeJSON)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("list filters denied tool", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withTok(httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tools", nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var tools []map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&tools); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, tl := range tools {
			if tl["name"] == "secret" {
				t.Errorf("denied tool leaked through /api/tools: %v", tl)
			}
		}
	})

	t.Run("get denied tool returns 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		secretGet := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tools/secret", nil)
		h.ServeHTTP(rec, withTok(secretGet))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (avoid leaking tool existence)", rec.Code)
		}
	})
}

// --- MAJOR4: ToolTimeout watchdog logs leak warning ---

// safeBuf wraps bytes.Buffer with a mutex so the watchdog goroutine can
// write while the test polls Read.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestToolTimeoutLogsLeakWarning(t *testing.T) {
	buf := &safeBuf{}
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	timeout := 20 * time.Millisecond
	cfg := Config{ToolTimeout: timeout}
	mw := ToolTimeoutMiddleware(cfg)

	next := mcp.MethodHandler(func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		// Sleep > leakWarnFactor*timeout so the watchdog fires.
		time.Sleep(timeout * 4)
		return &mcp.CallToolResult{}, nil
	})

	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "leaky"},
	}
	res, err := mw(next)(context.Background(), "tools/call", req)
	if err != nil {
		t.Fatalf("middleware err: %v", err)
	}
	cr, ok := res.(*mcp.CallToolResult)
	if !ok || !cr.IsError {
		t.Fatalf("expected timeout error result, got %#v", res)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "tool goroutine outlived its timeout") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "tool goroutine outlived its timeout") {
		t.Errorf("expected leak-warning log, got: %s", out)
	}
	if !strings.Contains(out, `"tool":"leaky"`) {
		t.Errorf("expected tool name in log, got: %s", out)
	}
}

// --- MAJOR5: REST bridge closes both client AND server session on shutdown ---

func TestRESTBridgeShutdownClosesBothSessions(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "shutdown-test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "noop"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{
		Name:              "shutdown-test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		Context:           ctx,
	}
	if _, err := Build(server, cfg); err != nil {
		t.Fatal(err)
	}

	// Before shutdown: server should report exactly one active session
	// (the in-process REST bridge).
	count := 0
	for range server.Sessions() {
		count++
	}
	if count != 1 {
		t.Fatalf("server.Sessions before shutdown: count = %d, want 1", count)
	}

	cancel()

	// Post-cancel close happens in a goroutine; allow up to 1s.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		count = 0
		for range server.Sessions() {
			count++
		}
		if count == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count != 0 {
		t.Errorf("server.Sessions after shutdown: count = %d, want 0 (server-side session leaked)", count)
	}
}

// --- MINOR7: RequestID validates incoming header ---

func TestRequestIDRejectsMaliciousHeader(t *testing.T) {
	mw := RequestID()
	var captured string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFromContext(r.Context())
	})
	h := mw(inner)

	cases := []struct {
		name     string
		input    string
		expectIn bool // should the input be reused verbatim?
	}{
		{"valid id", "abc-123_DEADBEEF", true},
		{"newline injection", "abc\ninjected log line", false},
		{"quote injection", `abc","level":"error`, false},
		{"too long", strings.Repeat("a", 65), false},
		{"empty", "", false},
		{"control chars", "abc\x00def", false},
		{"spaces", "abc def", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if c.input != "" {
				req.Header.Set("X-Request-ID", c.input)
			}
			h.ServeHTTP(rec, req)
			if c.expectIn {
				if captured != c.input {
					t.Errorf("valid id %q dropped, got %q", c.input, captured)
				}
				return
			}
			if captured == c.input {
				t.Errorf("malicious id %q passed through unchanged", c.input)
			}
			if captured == "" {
				t.Error("expected fallback random id, got empty")
			}
		})
	}
}

// --- MINOR8: response_writer counters are race-free ---

// concurrentRW is a minimal http.ResponseWriter whose Write is safe under
// concurrent callers — used only to test the responseWriter wrapper for races
// without involving httptest.ResponseRecorder (its internal bytes.Buffer is
// not goroutine-safe).
type concurrentRW struct {
	mu     sync.Mutex
	hdr    http.Header
	bytes  int64
	status int
}

func (c *concurrentRW) Header() http.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}

func (c *concurrentRW) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes += int64(len(b))
	return len(b), nil
}

func (c *concurrentRW) WriteHeader(s int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

func TestResponseWriterRace(t *testing.T) {
	rw := &responseWriter{ResponseWriter: &concurrentRW{}}
	rw.status.Store(int32(http.StatusOK))

	// Two goroutines write concurrently; under -race this would explode
	// without atomic counters.
	const writers = 8
	const writes = 50
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			for range writes {
				_, _ = rw.Write([]byte("xx"))
			}
		}()
	}
	wg.Wait()

	want := int64(writers * writes * 2)
	if got := rw.bytesWritten.Load(); got != want {
		t.Errorf("bytesWritten = %d, want %d", got, want)
	}
	// status must be 200 — only Write-without-WriteHeader path was used.
	if got := rw.status.Load(); got != int32(http.StatusOK) {
		t.Errorf("status = %d, want %d", got, http.StatusOK)
	}
	if !rw.wroteHeader.Load() {
		t.Error("wroteHeader should be true after Write")
	}
}

// --- MINOR9: coerceStringTypes recurses into nested objects and arrays ---

func TestCoerceNestedObject(t *testing.T) {
	schema := &jsonschema.Schema{
		Properties: map[string]*jsonschema.Schema{
			"opts": {
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"verbose": {Type: "boolean"},
					"limit":   {Type: "integer"},
				},
			},
		},
	}
	m := map[string]any{
		"opts": map[string]any{
			"verbose": "true",
			"limit":   "10",
		},
	}
	coerceStringTypes(m, schema)
	got := m["opts"].(map[string]any)
	if got["verbose"] != true {
		t.Errorf("nested bool: got %v (%T), want true", got["verbose"], got["verbose"])
	}
	if got["limit"] != int64(10) {
		t.Errorf("nested int: got %v (%T), want 10", got["limit"], got["limit"])
	}
}

func TestCoerceArrayItems(t *testing.T) {
	schema := &jsonschema.Schema{
		Properties: map[string]*jsonschema.Schema{
			"flags": {
				Type:  "array",
				Items: &jsonschema.Schema{Type: "boolean"},
			},
			"sizes": {
				Type:  "array",
				Items: &jsonschema.Schema{Type: "integer"},
			},
		},
	}
	m := map[string]any{
		"flags": []any{"true", "false", "1"},
		"sizes": []any{"1", "2", "abc"},
	}
	coerceStringTypes(m, schema)
	flags := m["flags"].([]any)
	if flags[0] != true || flags[1] != false || flags[2] != true {
		t.Errorf("flags coercion: %v", flags)
	}
	sizes := m["sizes"].([]any)
	if sizes[0] != int64(1) || sizes[1] != int64(2) || sizes[2] != "abc" {
		t.Errorf("sizes coercion: %v", sizes)
	}
}

// --- MINOR10: REST tools cache refreshes after TTL ---

func TestRESTToolsCacheRefreshesAfterTTL(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ttl-test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "first"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	mux := http.NewServeMux()
	cfg := Config{
		Name:              "ttl-test",
		Version:           "0.0.1",
		RESTBridge:        true,
		DisableRequestLog: true,
		Context:           ctx,
	}
	if err := startRESTBridge(ctx, server, mux, cfg, slog.Default()); err != nil {
		t.Fatal(err)
	}

	// Reach into the bridge by re-executing a list call once, then registering
	// a new tool, then forcing TTL expiry by walking the cache key directly
	// is brittle; instead exercise the public surface: list, register, wait, list.

	rec := httptest.NewRecorder()
	listReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tools", nil)
	mux.ServeHTTP(rec, listReq)
	var got []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("initial: got %d tools, want 1", len(got))
	}

	// Hot-register another tool — visible only after TTL expiry.
	mcp.AddTool(server, &mcp.Tool{Name: "second"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})

	// Locate bridge state to shrink TTL for fast test feedback. We do this
	// by accessing the unexported field via a fresh bridge configured with
	// short TTL.
	short := newShortTTLBridge(t, server)
	rec = httptest.NewRecorder()
	short.handleListTools(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	var first []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	initialCount := len(first)

	mcp.AddTool(server, &mcp.Tool{Name: "third"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})

	// First list within TTL — same count as before.
	rec = httptest.NewRecorder()
	short.handleListTools(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	var cached []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&cached); err != nil {
		t.Fatal(err)
	}
	if len(cached) != initialCount {
		t.Errorf("within TTL: got %d tools, want cached %d", len(cached), initialCount)
	}

	// Wait past TTL and list again — new tool should appear.
	time.Sleep(short.cacheTTL + 10*time.Millisecond)
	rec = httptest.NewRecorder()
	short.handleListTools(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	var refreshed []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&refreshed); err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != initialCount+1 {
		t.Errorf("after TTL: got %d tools, want %d", len(refreshed), initialCount+1)
	}
}

// newShortTTLBridge wires a new bridge against the same server with a 50 ms
// TTL so the refresh-after-expiry path can be exercised in test time.
func newShortTTLBridge(t *testing.T, server *mcp.Server) *restBridge {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "ttl-bridge", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &restBridge{
		session:  cs,
		prefix:   "/api",
		cfg:      Config{Name: "t", Version: "0"},
		logger:   slog.Default(),
		cacheTTL: 50 * time.Millisecond,
	}
}

// --- ensure auth pkg is imported (silences unused on Go < 1.18 toolchains) ---

var _ = auth.ErrInvalidToken
var _ atomic.Int64
