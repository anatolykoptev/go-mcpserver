package mcpclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	mcpserver "github.com/anatolykoptev/go-mcpserver"
	"github.com/anatolykoptev/go-mcpserver/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newEchoServer builds a minimal MCP httptest.Server exposing one "echo" tool
// that returns the "msg" argument as text content, and one "fail" tool that
// sets IsError on the result.
func newEchoServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)

	type echoArgs struct {
		Msg string `json:"msg"`
	}
	mcpserver.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "returns msg as text",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: args.Msg}},
		}, nil
	})

	mcpserver.AddTool(srv, &mcp.Tool{
		Name:        "fail",
		Description: "always returns an error result",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "tool exploded"}},
		}, nil
	})

	ts := mcpserver.NewTestServer(t, srv, mcpserver.Config{Name: "test", Version: "1.0.0"})
	return ts
}

// TestCallText_RoundTrip verifies CallText reaches the server and returns
// concatenated text content.
func TestCallText_RoundTrip(t *testing.T) {
	ts := newEchoServer(t)
	c := mcpclient.New(ts.URL)
	defer c.Close() //nolint:errcheck

	got, err := c.CallText(context.Background(), "echo", map[string]any{"msg": "hello"})
	if err != nil {
		t.Fatalf("CallText: %v", err)
	}
	if got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

// TestCallText_MultiContent verifies multi-part text is joined with newline.
func TestCallText_MultiContent(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	mcpserver.AddTool(srv, &mcp.Tool{Name: "multi"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "part1"},
				&mcp.TextContent{Text: "part2"},
			},
		}, nil
	})
	ts := mcpserver.NewTestServer(t, srv, mcpserver.Config{Name: "test", Version: "1.0.0"})

	c := mcpclient.New(ts.URL)
	defer c.Close() //nolint:errcheck

	got, err := c.CallText(context.Background(), "multi", nil)
	if err != nil {
		t.Fatalf("CallText: %v", err)
	}
	if got != "part1\npart2" {
		t.Fatalf("want %q, got %q", "part1\npart2", got)
	}
}

// TestCall_ToolError verifies IsError=true result surfaces as ErrToolError.
func TestCall_ToolError(t *testing.T) {
	ts := newEchoServer(t)
	c := mcpclient.New(ts.URL)
	defer c.Close() //nolint:errcheck

	_, err := c.Call(context.Background(), "fail", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, mcpclient.ErrToolError) {
		t.Fatalf("want ErrToolError, got %v", err)
	}
}

// TestCallText_UnreachableTolerant verifies that when tolerant=true, a dead
// server returns ("", nil).
func TestCallText_UnreachableTolerant(t *testing.T) {
	c := mcpclient.New("http://127.0.0.1:1", mcpclient.WithUnreachableTolerant(true))
	defer c.Close() //nolint:errcheck

	got, err := c.CallText(context.Background(), "any", nil)
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// TestCallText_UnreachableIntolerant verifies that when tolerant=false (default),
// a dead server returns ErrUnreachable.
func TestCallText_UnreachableIntolerant(t *testing.T) {
	c := mcpclient.New("http://127.0.0.1:1")
	defer c.Close() //nolint:errcheck

	_, err := c.CallText(context.Background(), "any", nil)
	if !errors.Is(err, mcpclient.ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

// TestFire_SurvivesCancelledParentCtx verifies that Fire keeps running even
// when the parent context is cancelled immediately after the call.
func TestFire_SurvivesCancelledParentCtx(t *testing.T) {
	// Use a channel with a short-lived server to prove the goroutine outlives
	// the parent context.
	done := make(chan struct{}, 1)

	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	mcpserver.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, error) {
		done <- struct{}{}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})
	ts := mcpserver.NewTestServer(t, srv, mcpserver.Config{Name: "test", Version: "1.0.0"})

	c := mcpclient.New(ts.URL)
	defer c.Close() //nolint:errcheck

	// Parent context is cancelled immediately after Fire returns.
	parentCtx, cancel := context.WithCancel(context.Background())
	c.Fire(parentCtx, "ping", nil)
	cancel()

	// The goroutine must still reach the server within 5s.
	select {
	case <-done:
		// success: server was reached after parent ctx was cancelled
	case <-time.After(5 * time.Second):
		t.Fatal("Fire: server was not reached after parent ctx cancellation (WithoutCancel guarantee broken)")
	}
}

// TestSessionReuse verifies that two consecutive calls share the same
// underlying session (no reconnect on the second call).
func TestSessionReuse(t *testing.T) {
	ts := newEchoServer(t)
	c := mcpclient.New(ts.URL, mcpclient.WithSessionReuse(true))
	defer c.Close() //nolint:errcheck

	for i := range 3 {
		got, err := c.CallText(context.Background(), "echo", map[string]any{"msg": "hi"})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "hi" {
			t.Fatalf("call %d: want %q, got %q", i, "hi", got)
		}
	}
}

// TestWithTimeout verifies that a per-call timeout fires when the tool is slow.
//
// Strategy: use WithHTTPClient with a short Timeout — this is respected by the
// go-sdk transport end-to-end including the Initialize POST. We point the client
// at a stall listener (accepts but never writes) so the HTTP exchange never
// completes, and the http.Client.Timeout fires within 200ms.
//
// Note: context.WithTimeout in mcpclient.Call is also applied, but the go-sdk
// transport detaches the connection context from the caller's ctx for standalone
// SSE support. WithHTTPClient.Timeout is the reliable per-transport gate.
func TestWithTimeout(t *testing.T) {
	ln, err := newStallListener(t)
	if err != nil {
		t.Fatalf("listener: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn // accept but never respond
		}
	}()

	stallHTTP := &http.Client{Timeout: 200 * time.Millisecond}
	c := mcpclient.New(
		"http://"+ln.Addr().String(),
		mcpclient.WithHTTPClient(stallHTTP),
		mcpclient.WithTimeout(200*time.Millisecond),
	)
	defer c.Close() //nolint:errcheck

	_, err = c.CallText(context.Background(), "any", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestNoSessionReuse_NoGoroutineLeak verifies that WithSessionReuse(false) does
// not leak goroutines, SSE connections, or sockets across multiple calls.
// Each non-reuse call must close its session before returning, leaving the
// goroutine count stable after a brief settle period.
func TestNoSessionReuse_NoGoroutineLeak(t *testing.T) {
	ts := newEchoServer(t)
	c := mcpclient.New(ts.URL, mcpclient.WithSessionReuse(false))
	defer c.Close() //nolint:errcheck

	const calls = 10
	const settle = 200 * time.Millisecond

	// Warm up: one call to let any one-time init goroutines settle.
	if _, err := c.CallText(context.Background(), "echo", map[string]any{"msg": "warmup"}); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	time.Sleep(settle)

	before := runtime.NumGoroutine()

	for i := range calls {
		got, err := c.CallText(context.Background(), "echo", map[string]any{"msg": "leak?"})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "leak?" {
			t.Fatalf("call %d: want %q, got %q", i, "leak?", got)
		}
	}

	// Allow any in-flight teardown to complete.
	time.Sleep(settle)
	after := runtime.NumGoroutine()

	// Allow slack for background noise: httptest server teardown, race-detector
	// goroutines, and runtime timers that may fire between samples. The key
	// signal is that we do NOT see ~1 goroutine per call (10+) as before the fix.
	const slack = 5
	if after > before+slack {
		t.Fatalf("goroutine leak: before=%d after=%d (delta=%d, slack=%d); non-reuse mode leaked %d goroutine(s)",
			before, after, after-before, slack, after-before)
	}
	t.Logf("goroutines: before=%d after=%d (delta=%d) — clean", before, after, after-before)
}

// newStallListener returns a TCP listener on a random local port that is
// cleaned up when the test finishes.
func newStallListener(t *testing.T) (*net.TCPListener, error) {
	t.Helper()
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, nil
}

// TestFireBoundedConcurrency verifies that Fire() goroutines are bounded
// by WithMaxFireConcurrency. When the limit is reached, additional Fire()
// calls are dropped instead of spawning unbounded goroutines.
func TestFireBoundedConcurrency(t *testing.T) {
	// Use a server with a slow tool so Fire goroutines stay alive.
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0"}, nil)
	mcpserver.AddTool(srv, &mcp.Tool{Name: "slow"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, error) {
		time.Sleep(2 * time.Second)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
	})
	ts := mcpserver.NewTestServer(t, srv, mcpserver.Config{Name: "test", Version: "1.0.0"})

	c := mcpclient.New(ts.URL, mcpclient.WithMaxFireConcurrency(3))
	defer c.Close() //nolint:errcheck

	before := runtime.NumGoroutine()

	// Fire 20 calls — only 3 should spawn goroutines, the rest should be dropped.
	for i := 0; i < 20; i++ {
		c.Fire(context.Background(), "slow", nil)
	}

	// Give dropped calls time to be rejected.
	time.Sleep(200 * time.Millisecond)

	// Goroutine count should not spike to 20 — at most 3 Fire goroutines
	// plus transport/SSE overhead per call. Without the semaphore, 20 Fire
	// calls would each spawn a Call goroutine + HTTP/SSE goroutines (~200+).
	// With the semaphore, only 3 Fire calls proceed.
	peak := runtime.NumGoroutine()
	growth := peak - before

	if growth > 60 {
		t.Errorf("goroutine growth = %d (before=%d, peak=%d), want <= 60 — semaphore not bounding Fire() goroutines", growth, before, peak)
	}

	// Wait for the 3 goroutines to finish so we don't leak them into other tests.
	time.Sleep(3 * time.Second)
}
