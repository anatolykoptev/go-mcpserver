# Roadmap

## v0.6.0

- **MCP-layer tool scope filtering** — `MCPToolFilter func(ctx context.Context, toolName string) bool` in `BearerAuth`; filters `tools/list` responses per token scope (clients see only permitted tools, not flat 401). Inspired by FastMCP Python `AuthMiddleware.on_list_tools()`.
- **Testing convenience** — `NewTestServer(t, server, cfg) *httptest.Server` wrapping `Build()` + `httptest.NewServer()` + `t.Cleanup()`. Mirrors mark3labs/mcp-go `mcptest` and punkpeye/fastmcp `runWithTestServer()`.
- **MCPHooks convenience** — `type MCPHooks struct { OnToolCall, OnError, OnSuccess }` factory for `MCPReceivingMiddleware`; simplified typed hooks for metrics/tracing without full OTel setup. Inspired by mark3labs/mcp-go 24-hook system.

## v0.5.1 (done)

- ~~**StaticTokenVerifier**~~ — convenience helper for pre-shared token auth without full OAuth

## v0.5.0 (done)

- ~~**MCP-layer middleware**~~ — `Config.MCPReceivingMiddleware`/`MCPSendingMiddleware` pass-through to go-sdk `AddReceivingMiddleware`/`AddSendingMiddleware`
- ~~**StreamableHTTP options**~~ — `Config.SessionTimeout`, `EventStore`, `JSONResponse`, `MCPLogger` exposed
- ~~**OAuth 2.1 bearer auth**~~ — `Config.BearerAuth` wraps `/mcp` only; RFC 9728 metadata endpoint; `auth.go` types
- ~~**Connection lost handler**~~ — dropped (`ServerSessionOptions.onClose` unexported in SDK v1.4.0)

## v0.4.0 (done)

- ~~**Stateless/Stateful toggle**~~ — `Config.Stateless *bool`; nil defaults to true
- ~~**DisableMCP flag**~~ — `Config.DisableMCP bool`; skips `/mcp` route registration
- ~~**Flusher interface**~~ — `responseWriter` implements `http.Flusher`; extracted to `response_writer.go`
- ~~**os.Exit cleanup**~~ — done in v0.3.0 audit (error channel)
- ~~**Expose handler**~~ — `Build()` returns `(http.Handler, error)` for testing/embedding
- ~~**Consumer migrations**~~ — go-billing migrated; `WithRequestID()` context helper added
