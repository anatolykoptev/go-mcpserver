package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// forgedCallReq carries a params type that does not match its method, which is
// what a miswired or future SDK dispatch could hand a receiving middleware.
func forgedCallReq() mcp.Request {
	return &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}}
}

func allowAll(context.Context, string, *TokenInfo) bool { return true }

func TestCommaOk_FilterToolCallDeniesOnParamsMismatch(t *testing.T) {
	called := false
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	h := toolFilterMiddleware(allowAll)(next)
	res, err := h(context.Background(), "tools/call", forgedCallReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Fatal("comma-ok: next was reached on a params type mismatch; want deny")
	}
	cr, ok := res.(*mcp.CallToolResult)
	if !ok || !cr.IsError {
		t.Fatalf("comma-ok: want IsError result on mismatch, got %#v", res)
	}
}

func TestCommaOk_FilterToolListDeniesOnResultMismatch(t *testing.T) {
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.CallToolResult{}, nil // not a *ListToolsResult
	}
	h := toolFilterMiddleware(allowAll)(next)
	res, err := h(context.Background(), "tools/list", forgedCallReq())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lr, ok := res.(*mcp.ListToolsResult)
	if !ok || len(lr.Tools) != 0 {
		t.Fatalf("comma-ok: want empty ListToolsResult on mismatch, got %#v", res)
	}
}

func TestCommaOk_HooksPassThroughOnParamsMismatch(t *testing.T) {
	var observed bool
	hooks := MCPHooks{OnToolCall: func(context.Context, string) { observed = true }}
	nextCalled := false
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		nextCalled = true
		return &mcp.CallToolResult{}, nil
	}
	res, err := hooks.Middleware()(next)(context.Background(), "tools/call", forgedCallReq())
	if err != nil || res == nil {
		t.Fatalf("hooks must pass through unchanged, got res=%v err=%v", res, err)
	}
	if !nextCalled {
		t.Fatal("comma-ok: MCPHooks did not pass through to next on mismatch")
	}
	if observed {
		t.Fatal("MCPHooks observed a call it could not parse")
	}
}

func TestCommaOk_TimeoutMiddlewarePassThroughOnParamsMismatch(t *testing.T) {
	nextCalled := false
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		nextCalled = true
		return &mcp.CallToolResult{}, nil
	}
	h := ToolTimeoutMiddleware(Config{})(next)
	res, err := h(context.Background(), "tools/call", forgedCallReq())
	if err != nil || res == nil {
		t.Fatalf("timeout middleware must pass through unchanged, got res=%v err=%v", res, err)
	}
	if !nextCalled {
		t.Fatal("comma-ok: ToolTimeoutMiddleware did not pass through to next on mismatch")
	}
}
