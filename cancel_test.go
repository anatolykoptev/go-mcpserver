package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cancelProbe returns a server with a "hang" tool that records ctx cancellation.
type cancelProbe struct {
	started   chan struct{}
	cancelled chan struct{}
	finished  atomic.Bool
}

func newCancelServer(t *testing.T, cfg Config) (*httptest.Server, *cancelProbe) {
	t.Helper()
	pr := &cancelProbe{started: make(chan struct{}, 1), cancelled: make(chan struct{}, 1)}
	cfg.Name, cfg.Version, cfg.DisableRequestLog = "cancel", "0.0.1", true
	server := NewServer(&mcp.Implementation{Name: "cancel", Version: "0.0.1"}, cfg)
	mcp.AddTool(server, &mcp.Tool{Name: "hang"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		pr.started <- struct{}{}
		select {
		case <-ctx.Done():
			pr.cancelled <- struct{}{}
			return nil, nil, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
			pr.finished.Store(true)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
		}
	})
	return NewTestServer(t, server, cfg), pr
}

// postHang POSTs a tools/call for "hang" at the given protocol and returns a
// cancel func that aborts the HTTP request (closing the connection).
func postHang(t *testing.T, ts *httptest.Server, proto string) (abort func(), done <-chan struct{}) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hang","arguments":{}`
	if proto == protoNew {
		body += `,"_meta":{"io.modelcontextprotocol/protocolVersion":"` + protoNew +
			`","io.modelcontextprotocol/clientInfo":{"name":"x","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`
	}
	body += `}}`
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", proto)
	if proto == protoNew {
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "hang")
	}
	fin := make(chan struct{})
	go func() {
		defer close(fin)
		resp, err := ts.Client().Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	return cancel, fin
}

func waitCh(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// T1: default config cancels the tool ctx promptly after the client aborts.
func TestCancelOnDisconnect_T1_DefaultCancelsTool(t *testing.T) {
	for _, proto := range []string{protoNew, protoOld} {
		t.Run(proto, func(t *testing.T) {
			ts, pr := newCancelServer(t, Config{})
			abort, done := postHang(t, ts, proto)
			if !waitCh(pr.started, 3*time.Second) {
				t.Fatal("tool never started")
			}
			start := time.Now()
			abort()
			if !waitCh(pr.cancelled, 500*time.Millisecond) {
				t.Fatalf("tool ctx not cancelled within 500ms of the client aborting (waited %s)", time.Since(start))
			}
			<-done
		})
	}
}

// T2: with CancelOnClientDisconnect=false the tool is not cancelled by the abort.
func TestCancelOnDisconnect_T2_DisabledKeepsRunning(t *testing.T) {
	off := false
	for _, proto := range []string{protoNew, protoOld} {
		t.Run(proto, func(t *testing.T) {
			ts, pr := newCancelServer(t, Config{CancelOnClientDisconnect: &off})
			abort, done := postHang(t, ts, proto)
			if !waitCh(pr.started, 3*time.Second) {
				t.Fatal("tool never started")
			}
			abort()
			if waitCh(pr.cancelled, 500*time.Millisecond) {
				t.Fatal("tool ctx was cancelled by the client abort despite CancelOnClientDisconnect=false")
			}
			<-done
			deadline := time.Now().Add(4 * time.Second)
			for !pr.finished.Load() && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if !pr.finished.Load() {
				t.Fatal("tool did not run to completion")
			}
		})
	}
}

// T3: after the cancellation, middleware, keepalive and tool goroutines all
// exit - the goroutine count returns to its pre-request baseline (bounded
// settle loop, no fixed slack).
func TestCancelOnDisconnect_T3_NoGoroutineLeak(t *testing.T) {
	for _, proto := range []string{protoNew, protoOld} {
		t.Run(proto, func(t *testing.T) {
			ts, pr := newCancelServer(t, Config{ToolKeepaliveInterval: 10 * time.Millisecond})
			// Warm up one full cycle so one-time goroutines exist in the baseline.
			abort, done := postHang(t, ts, proto)
			<-pr.started
			abort()
			<-pr.cancelled
			<-done
			// settle polls until the count is stable for several reads (or, when
			// target > 0, drops to target), bounded at 5s.
			settle := func(target int) int {
				deadline := time.Now().Add(5 * time.Second)
				n, stable := runtime.NumGoroutine(), 0
				for time.Now().Before(deadline) {
					ts.Client().CloseIdleConnections()
					time.Sleep(25 * time.Millisecond)
					m := runtime.NumGoroutine()
					if target > 0 && m <= target {
						return m
					}
					if m == n {
						stable++
					} else {
						stable = 0
					}
					n = m
					if target == 0 && stable >= 6 {
						return n
					}
				}
				return n
			}
			base := settle(0)
			for range 5 {
				abort, done := postHang(t, ts, proto)
				<-pr.started
				abort()
				<-pr.cancelled
				<-done
			}
			if after := settle(base); after > base {
				t.Fatalf("goroutine leak: baseline %d, after 5 cancelled calls %d", base, after)
			}
		})
	}
}

// T4: a stateless handler answers GET and DELETE with 405 + Allow: POST (spec:
// a server offering no standalone SSE stream MUST 405 a GET). The SDK does this
// today; this locks it against a future SDK change.
func TestStatelessGetDelete_405AllowPost(t *testing.T) {
	ts := NewTestServer(t, mcp.NewServer(&mcp.Implementation{Name: "m", Version: "1"}, nil),
		Config{Name: "m", Version: "1", DisableRequestLog: true})
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(context.Background(), method, ts.URL+"/mcp", nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s /mcp = %d Allow=%q; want 405 Allow=POST", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
}

// An unauthenticated GET under BearerAuth is rejected by auth (401), not
// answered with the stateless 405: auth wraps the MCP handler.
func TestStatelessGet_Unauthenticated401UnderBearerAuth(t *testing.T) {
	h, err := Build(mcp.NewServer(&mcp.Implementation{Name: "m", Version: "1"}, nil), Config{
		Name: "m", Version: "1", DisableRequestLog: true,
		BearerAuth: &BearerAuth{Verifier: validVerifier},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{"", http.StatusUnauthorized}, {"valid-token", http.StatusMethodNotAllowed}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/mcp", nil)
		req.Header.Set("Accept", "text/event-stream")
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET /mcp token=%q: status = %d, want %d", tc.token, rec.Code, tc.want)
		}
	}
}
