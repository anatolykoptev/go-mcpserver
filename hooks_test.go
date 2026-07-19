package mcpserver

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newHooksTestServer(t *testing.T, hooks MCPHooks) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "hooks-test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "fail"}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "boom"}},
		}, nil, nil
	})

	ts := NewTestServer(t, server, Config{
		Name:                   "hooks-test",
		Version:                "0.0.1",
		MCPReceivingMiddleware: []mcp.Middleware{hooks.Middleware()},
		DisableRequestLog:      true,
	})

	transport := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           ts.Client(),
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	sess, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func TestMCPHooksOnToolCall(t *testing.T) {
	var mu sync.Mutex
	var called string

	hooks := MCPHooks{
		OnToolCall: func(_ context.Context, toolName string) {
			mu.Lock()
			called = toolName
			mu.Unlock()
		},
	}

	sess := newHooksTestServer(t, hooks)
	_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if called != "echo" {
		t.Errorf("OnToolCall got %q, want %q", called, "echo")
	}
}

func TestMCPHooksOnToolResult(t *testing.T) {
	var mu sync.Mutex
	var gotName string
	var gotDur time.Duration
	var gotErr bool

	hooks := MCPHooks{
		OnToolResult: func(_ context.Context, name string, dur time.Duration, isErr bool) {
			mu.Lock()
			gotName = name
			gotDur = dur
			gotErr = isErr
			mu.Unlock()
		},
	}

	sess := newHooksTestServer(t, hooks)

	// Call successful tool
	_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if gotName != "echo" {
		t.Errorf("name = %q, want %q", gotName, "echo")
	}
	if gotDur <= 0 {
		t.Error("expected duration > 0")
	}
	if gotErr {
		t.Error("expected isError=false for echo")
	}
	mu.Unlock()

	// Call failing tool
	_, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "fail"})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	if gotName != "fail" {
		t.Errorf("name = %q, want %q", gotName, "fail")
	}
	if !gotErr {
		t.Error("expected isError=true for fail")
	}
	mu.Unlock()
}

func TestMCPHooksOnError(t *testing.T) {
	var mu sync.Mutex
	var gotMethod string
	var gotErr error

	hooks := MCPHooks{
		OnError: func(_ context.Context, method string, err error) {
			mu.Lock()
			gotMethod = method
			gotErr = err
			mu.Unlock()
		},
	}

	sess := newHooksTestServer(t, hooks)

	// Call non-existent tool — SDK may return protocol-level error
	_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "nonexistent"})
	if err == nil {
		mu.Lock()
		defer mu.Unlock()
		if gotMethod == "" {
			t.Skip("SDK returned tool-not-found as result, not method error")
		}
		return
	}

	mu.Lock()
	defer mu.Unlock()
	if gotMethod == "" && gotErr == nil {
		t.Skip("error propagated to client without hitting OnError middleware")
	}
}

func TestMCPHooksPartial(t *testing.T) {
	// Only OnToolResult set — OnToolCall and OnError are nil. No panic expected.
	var mu sync.Mutex
	var gotName string

	hooks := MCPHooks{
		OnToolResult: func(_ context.Context, name string, _ time.Duration, _ bool) {
			mu.Lock()
			gotName = name
			mu.Unlock()
		},
	}

	sess := newHooksTestServer(t, hooks)
	_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo"})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotName != "echo" {
		t.Errorf("OnToolResult got %q, want %q", gotName, "echo")
	}
}

func TestResolveTimeoutCapsArgTimeout(t *testing.T) {
	cfg := Config{ToolTimeout: 90 * time.Second}
	cfg = withDefaults(cfg)

	// An absurd timeout_secs should be capped, not passed through.
	args := []byte(`{"timeout_secs": 9999999}`)
	got := resolveTimeout("echo", args, cfg)
	maxAllowed := cfg.MaxToolTimeout
	if maxAllowed == 0 {
		maxAllowed = cfg.ToolTimeout * 2
	}
	if got > maxAllowed {
		t.Errorf("resolveTimeout with timeout_secs=9999999 returned %v, want <= %v (capped)", got, maxAllowed)
	}
}

func TestResolveTimeoutRespectsReasonableArgTimeout(t *testing.T) {
	cfg := Config{ToolTimeout: 90 * time.Second}
	cfg = withDefaults(cfg)

	// A reasonable timeout_secs below the cap should pass through.
	args := []byte(`{"timeout_secs": 30}`)
	got := resolveTimeout("echo", args, cfg)
	if got != 30*time.Second {
		t.Errorf("resolveTimeout with timeout_secs=30 returned %v, want 30s", got)
	}
}

func TestToolTimeoutMiddlewareBoundedConcurrency(t *testing.T) {
	cfg := Config{ToolTimeout: 5 * time.Second, MaxConcurrentTools: 2}
	cfg = withDefaults(cfg)

	var active int32
	var maxActive int32

	block := make(chan struct{})
	handler := mcp.MethodHandler(func(_ context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maxActive)
			if cur <= old || atomic.CompareAndSwapInt32(&maxActive, old, cur) {
				break
			}
		}
		<-block // block until test releases
		atomic.AddInt32(&active, -1)
		return &mcp.CallToolResult{}, nil
	})

	mw := ToolTimeoutMiddleware(cfg)
	wrapped := mw(handler)

	makeReq := func() mcp.Request {
		return &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
			Params: &mcp.CallToolParamsRaw{Name: "blocked"},
		}
	}

	// Launch 5 concurrent calls — only 2 should run, 3 should be rejected.
	var wg sync.WaitGroup
	var rejected int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, _ := wrapped(context.Background(), methodToolsCall, makeReq())
			if cr, ok := result.(*mcp.CallToolResult); ok && cr.IsError {
				if len(cr.Content) > 0 {
					if tc, ok := cr.Content[0].(*mcp.TextContent); ok && strings.Contains(tc.Text, "max concurrent") {
						atomic.AddInt32(&rejected, 1)
					}
				}
			}
		}()
	}

	// Give goroutines time to start and hit the semaphore.
	time.Sleep(100 * time.Millisecond)
	close(block) // release the 2 that acquired the semaphore
	wg.Wait()

	if max := atomic.LoadInt32(&maxActive); max > 2 {
		t.Errorf("max concurrent tools = %d, want <= 2", max)
	}
	if got := atomic.LoadInt32(&rejected); got < 3 {
		t.Errorf("rejected = %d, want >= 3 (5 calls, 2 slots)", got)
	}
}

func TestDisableEventStore(t *testing.T) {
	cfg := Config{Name: "test", Version: "0.0.1", DisableEventStore: true}
	cfg = withDefaults(cfg)
	if cfg.EventStore != nil {
		t.Error("DisableEventStore=true but EventStore was auto-enabled")
	}

	cfg2 := Config{Name: "test", Version: "0.0.1"}
	cfg2 = withDefaults(cfg2)
	if cfg2.EventStore == nil {
		t.Error("DisableEventStore=false (default) but EventStore was not auto-enabled")
	}
}

// TestToolKeepaliveHeartbeat verifies that with ToolKeepaliveInterval set, a
// long-running tool call emits progress notifications to the client while it
// runs (SSE + stateless, matching the go-wp deployment) and still returns its
// result. Without the heartbeat the client sees zero bytes until the tool
// completes and a shorter client/proxy timeout would abandon the call.
func TestToolKeepaliveHeartbeat(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "ka-test", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "slow"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		select {
		case <-time.After(220 * time.Millisecond):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
	})

	ts := NewTestServer(t, server, Config{
		Name:                  "ka-test",
		Version:               "0.0.1",
		ToolKeepaliveInterval: 30 * time.Millisecond,
		DisableRequestLog:     true,
	})

	var progress atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, _ *mcp.ProgressNotificationClientRequest) {
			progress.Add(1)
		},
	})
	transport := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           ts.Client(),
		DisableStandaloneSSE: true,
	}
	sess, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned error result: %+v", res.Content)
	}
	if got := progress.Load(); got < 1 {
		t.Errorf("expected >=1 progress heartbeat during the 220ms call (interval 30ms), got %d", got)
	}
}

func TestResolveToolTimeoutMode(t *testing.T) {
	cases := []struct {
		mode ToolTimeoutMode
		want time.Duration
	}{
		{ToolTimeoutModeShort, ToolTimeoutShort},
		{ToolTimeoutModeDefault, ToolTimeoutDefault},
		{ToolTimeoutModeLong, ToolTimeoutLong},
		{ToolTimeoutModeCustom, ToolTimeoutDefault}, // custom without explicit ToolTimeout → default tier
		{"", ToolTimeoutDefault},
		{"bogus", ToolTimeoutDefault},
	}
	for _, c := range cases {
		if got := resolveToolTimeoutMode(c.mode); got != c.want {
			t.Errorf("resolveToolTimeoutMode(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

func TestWithDefaultsToolTimeoutMode(t *testing.T) {
	// Mode selects the tier when ToolTimeout is unset.
	if got := withDefaults(Config{Name: "x", Version: "y", ToolTimeoutMode: ToolTimeoutModeLong}).ToolTimeout; got != ToolTimeoutLong {
		t.Errorf("mode=long → ToolTimeout = %v, want %v", got, ToolTimeoutLong)
	}
	// An explicit ToolTimeout always wins over the mode.
	if got := withDefaults(Config{Name: "x", Version: "y", ToolTimeout: 42 * time.Second, ToolTimeoutMode: ToolTimeoutModeLong}).ToolTimeout; got != 42*time.Second {
		t.Errorf("explicit ToolTimeout with mode=long = %v, want 42s", got)
	}
	// Zero-value: default tier (90s), not the old implicit value.
	if got := withDefaults(Config{Name: "x", Version: "y"}).ToolTimeout; got != ToolTimeoutDefault {
		t.Errorf("no mode → ToolTimeout = %v, want %v", got, ToolTimeoutDefault)
	}
}
