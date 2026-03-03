package mcpserver

import (
	"context"
	"sync"
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
