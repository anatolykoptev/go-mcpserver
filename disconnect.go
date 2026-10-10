package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// clientCtxHeader carries a per-request random ID from the HTTP layer to the
// tool-timeout middleware (via RequestExtra.Header), so the middleware can find
// the originating HTTP request's context. The wrapper always overwrites it, so
// a client cannot choose its own value.
const clientCtxHeader = "X-Mcpserver-Client-Ctx"

const clientCtxIDBytes = 16

// sdkPropagatesFrom is the first protocol version for which go-sdk's
// PropagateRequestCancellation cancels the handler context itself.
const sdkPropagatesFrom = "2026-07-28"

// clientCtxs maps clientCtxHeader values to the live HTTP request contexts.
var clientCtxs sync.Map // string -> context.Context

// trackClientDisconnect wraps the MCP handler so tool calls can observe the
// HTTP request context. The go-sdk's own PropagateRequestCancellation only
// covers 2026-07-28 requests; this covers 2025-11-25 and older stateless POSTs
// (2026-07-28 requests are left to the SDK).
func trackClientDisconnect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 2026-07-28 requests are handled by the SDK's own option.
		if r.Method != http.MethodPost || r.Header.Get("Mcp-Protocol-Version") >= sdkPropagatesFrom {
			next.ServeHTTP(w, r)
			return
		}
		b := make([]byte, clientCtxIDBytes)
		if _, err := rand.Read(b); err != nil {
			next.ServeHTTP(w, r) // fail open: no disconnect cancellation for this request
			return
		}
		id := hex.EncodeToString(b)
		clientCtxs.Store(id, r.Context())
		defer clientCtxs.Delete(id)
		r.Header.Set(clientCtxHeader, id)
		next.ServeHTTP(w, r)
	})
}

// clientRequestCtx returns the HTTP request context behind req, or nil when
// the request did not come through trackClientDisconnect.
func clientRequestCtx(req mcp.Request) context.Context {
	extra := req.GetExtra()
	if extra == nil || extra.Header == nil {
		return nil
	}
	id := extra.Header.Get(clientCtxHeader)
	if id == "" {
		return nil
	}
	if v, ok := clientCtxs.Load(id); ok {
		if c, ok := v.(context.Context); ok {
			return c
		}
	}
	return nil
}
