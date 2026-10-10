package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	protoNew = "2026-07-28"
	protoOld = "2025-11-25"
)

type addArgs struct {
	N int `json:"n"`
}

// newProtocolServer builds an mcp.Server via NewServer (so Config-derived
// ServerOptions such as SupportedProtocolVersions apply) with an "add" tool
// registered through the lenient AddTool and a "slow" tool that blocks until
// its context is done.
func newProtocolServer(t *testing.T, cfg Config) *mcp.Server {
	t.Helper()
	cfg.Name, cfg.Version = "proto-test", "0.0.1"
	server := NewServer(&mcp.Implementation{Name: "proto-test", Version: "0.0.1"}, cfg)
	AddTool(server, &mcp.Tool{Name: "add", Description: "n+1"},
		func(_ context.Context, _ *mcp.CallToolRequest, a addArgs) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: jsonInt(a.N + 1)}}}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "slow"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			select {
			case <-time.After(5 * time.Second):
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil, nil
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		})
	return server
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// connectProto connects a go-sdk client to ts. pin == "" lets the client
// prefer its newest protocol; otherwise ClientSessionOptions.ProtocolVersion
// pins it.
func connectProto(t *testing.T, ts *httptest.Server, pin string, copts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "proto-client", Version: "0.0.1"}, copts)
	var so *mcp.ClientSessionOptions
	if pin != "" {
		so = &mcp.ClientSessionOptions{ProtocolVersion: pin}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           ts.Client(),
		DisableStandaloneSSE: true,
	}, so)
	if err != nil {
		t.Fatalf("connect (pin=%q): %v", pin, err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// assertListAndCall lists tools and calls "add" with a string-typed number to
// exercise lenient coercion; the answer must be correct.
func assertListAndCall(t *testing.T, sess *mcp.ClientSession) {
	t.Helper()
	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["add"] || !names["slow"] {
		t.Fatalf("ListTools = %v, want add and slow", names)
	}
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "add",
		Arguments: map[string]any{"n": "41"}, // string -> int via lenient coercion
	})
	if err != nil {
		t.Fatalf("CallTool add: %v", err)
	}
	if res.IsError || len(res.Content) != 1 {
		t.Fatalf("add result = %+v", res)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "42" {
		t.Fatalf("add(\"41\") = %q, want 42", got)
	}
}

func negotiated(sess *mcp.ClientSession) string {
	if r := sess.InitializeResult(); r != nil {
		return r.ProtocolVersion
	}
	return ""
}

// T1: default (stateless) server speaks 2026-07-28 end to end.
func TestProtocol_T1_Stateless_2026(t *testing.T) {
	ts := NewTestServer(t, newProtocolServer(t, Config{}), Config{Name: "proto-test", Version: "0.0.1", DisableRequestLog: true})
	sess := connectProto(t, ts, "", nil)
	if got := negotiated(sess); got != protoNew {
		t.Fatalf("negotiated = %q, want %q", got, protoNew)
	}
	assertListAndCall(t, sess)
}

// T2: a client pinned to 2025-11-25 still works against the same server.
func TestProtocol_T2_Pinned2025_StillWorks(t *testing.T) {
	ts := NewTestServer(t, newProtocolServer(t, Config{}), Config{Name: "proto-test", Version: "0.0.1", DisableRequestLog: true})
	sess := connectProto(t, ts, protoOld, nil)
	if got := negotiated(sess); got != protoOld {
		t.Fatalf("negotiated = %q, want %q", got, protoOld)
	}
	assertListAndCall(t, sess)
}

// T3: SupportedProtocolVersions narrows the server; a 2026-preferring client
// ends up on 2025-11-25.
func TestProtocol_T3_SupportedProtocolVersions_Narrows(t *testing.T) {
	cfg := Config{Name: "proto-test", Version: "0.0.1", DisableRequestLog: true, SupportedProtocolVersions: []string{protoOld}}
	ts := NewTestServer(t, newProtocolServer(t, cfg), cfg.withoutServerOpts())
	sess := connectProto(t, ts, "", nil) // client prefers 2026-07-28
	if got := negotiated(sess); got != protoOld {
		t.Fatalf("negotiated = %q, want %q (server pinned to %s)", got, protoOld, protoOld)
	}
	assertListAndCall(t, sess)
}

// Stateful mode: 2025-11-25 clients work; a 2026-07-28 request gets the SDK's
// JSON-RPC unsupported-version error (so clients renegotiate), and the
// default client falls back to 2025-11-25.
func TestProtocol_Stateful(t *testing.T) {
	f := false
	cfg := Config{Name: "proto-test", Version: "0.0.1", DisableRequestLog: true, Stateless: &f, SessionTimeout: time.Minute}
	ts := NewTestServer(t, newProtocolServer(t, cfg), cfg)

	t.Run("pinned 2025 works", func(t *testing.T) {
		sess := connectProto(t, ts, protoOld, nil)
		if got := negotiated(sess); got != protoOld {
			t.Fatalf("negotiated = %q, want %q", got, protoOld)
		}
		assertListAndCall(t, sess)
	})

	t.Run("default client renegotiates down", func(t *testing.T) {
		sess := connectProto(t, ts, "", nil)
		if got := negotiated(sess); got != protoOld {
			t.Fatalf("negotiated = %q, want %q", got, protoOld)
		}
		assertListAndCall(t, sess)
	})

	t.Run("raw 2026 request gets JSON-RPC error not plaintext", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"` + protoNew + `","io.modelcontextprotocol/clientInfo":{"name":"x","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", protoNew)
		req.Header.Set("Mcp-Method", "tools/list")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil || env.Error == nil {
			t.Fatalf("status=%d body=%q: want JSON-RPC error envelope (err=%v)", resp.StatusCode, raw, err)
		}
		if env.Error.Code != -32022 {
			t.Fatalf("error code = %d, want -32022 (UnsupportedProtocolVersion); body=%s", env.Error.Code, raw)
		}
	})
}

// T4: tool timeout and keepalive behave under 2026-07-28.
func TestProtocol_T4_TimeoutAndKeepalive_2026(t *testing.T) {
	cfg := Config{
		Name: "proto-test", Version: "0.0.1", DisableRequestLog: true,
		ToolTimeout:           150 * time.Millisecond,
		ToolKeepaliveInterval: 20 * time.Millisecond,
	}
	ts := NewTestServer(t, newProtocolServer(t, cfg), cfg)
	var progress atomic.Int32
	sess := connectProto(t, ts, "", &mcp.ClientOptions{
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { progress.Add(1) },
	})
	if got := negotiated(sess); got != protoNew {
		t.Fatalf("negotiated = %q, want %q", got, protoNew)
	}
	start := time.Now()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("slow tool ran %s; ToolTimeout=150ms did not cut it", elapsed)
	}
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("slow tool should fail after timeout, got res=%+v", res)
	}
	if progress.Load() < 1 {
		t.Errorf("no keepalive progress notification received before timeout (interval 20ms, timeout 150ms)")
	}
	// Session still usable after the timeout.
	assertListAndCall(t, sess)
}

// T5: REST bridge list + call still work when the server speaks 2026-07-28.
func TestProtocol_T5_RESTBridge(t *testing.T) {
	cfg := Config{Name: "proto-test", Version: "0.0.1", DisableRequestLog: true, RESTBridge: true}
	ts := NewTestServer(t, newProtocolServer(t, cfg), cfg)

	status, raw := doREST(t, ts, http.MethodGet, "/api/tools", "")
	if status != http.StatusOK || !strings.Contains(raw, `"add"`) {
		t.Fatalf("GET /api/tools = %d %s, want 200 listing add", status, raw)
	}

	status, raw = doREST(t, ts, http.MethodPost, "/api/tools/add", `{"n":"41"}`)
	if status != http.StatusOK || !strings.Contains(raw, "42") {
		t.Fatalf("POST /api/tools/add = %d %s, want 200 containing 42", status, raw)
	}
}

func doREST(t *testing.T, ts *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
