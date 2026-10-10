package mcpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const warnMarker = "SupportedProtocolVersions"

// syncBuf is a goroutine-safe log sink (Run logs from several goroutines).
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestWithoutServerOpts_ClearsSupportedProtocolVersions(t *testing.T) {
	cfg := Config{Name: "x", Version: "0.0.1", SupportedProtocolVersions: []string{protoOld}}
	stripped := cfg.withoutServerOpts()
	if stripped.SupportedProtocolVersions != nil {
		t.Errorf("SupportedProtocolVersions = %v after withoutServerOpts, want nil", stripped.SupportedProtocolVersions)
	}
	if len(cfg.SupportedProtocolVersions) != 1 {
		t.Error("withoutServerOpts mutated the receiver")
	}
}

func TestWarnIgnoredServerOpts_SupportedProtocolVersions(t *testing.T) {
	cfg := Config{Name: "w", Version: "0.0.1", DisableRequestLog: true, SupportedProtocolVersions: []string{protoOld}}

	t.Run("Build warns naming the option", func(t *testing.T) {
		buf := &syncBuf{}
		cfg := cfg
		cfg.Logger = slog.New(slog.NewTextHandler(buf, nil))
		if _, err := Build(mcp.NewServer(&mcp.Implementation{Name: "w", Version: "1"}, nil), cfg); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), warnMarker) {
			t.Errorf("Build did not warn about %s; log: %q", warnMarker, buf.String())
		}
	})

	t.Run("Run warns naming the option", func(t *testing.T) {
		got := runBriefly(t, func(cfg Config) error {
			return Run(mcp.NewServer(&mcp.Implementation{Name: "w", Version: "1"}, nil), cfg)
		}, cfg)
		if !strings.Contains(got, warnMarker) {
			t.Errorf("Run did not warn about %s; log: %q", warnMarker, got)
		}
	})

	t.Run("Serve does not warn", func(t *testing.T) {
		got := runBriefly(t, func(cfg Config) error {
			return Serve(&mcp.Implementation{Name: "w", Version: "1"}, cfg, nil)
		}, cfg)
		if strings.Contains(got, warnMarker) {
			t.Errorf("Serve must not emit the ignored-option warning; log: %q", got)
		}
	})

	t.Run("NewServer does not warn", func(t *testing.T) {
		buf := &syncBuf{}
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
		defer slog.SetDefault(old)
		_ = NewServer(&mcp.Implementation{Name: "w", Version: "1"}, cfg)
		if strings.Contains(buf.String(), warnMarker) {
			t.Errorf("NewServer must not emit the warning; log: %q", buf.String())
		}
	})
}

// runBriefly runs fn (Run/Serve) on port 0 with a captured logger, cancels
// after the server is up, and returns everything it logged.
func runBriefly(t *testing.T, fn func(Config) error, cfg Config) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buf := &syncBuf{}
	old := slog.Default()
	defer slog.SetDefault(old)
	cfg.Logger = slog.New(slog.NewTextHandler(buf, nil))
	cfg.Port, cfg.Context = "0", ctx
	errCh := make(chan error, 1)
	go func() { errCh <- fn(cfg) }()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("server returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop within 5s")
	}
	return buf.String()
}

func TestNewServer_UnknownProtocolVersionPanicsClearly(t *testing.T) {
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if !strings.Contains(msg, `"1999-01-01"`) || !strings.Contains(msg, protoNew) || !strings.Contains(msg, "SupportedProtocolVersions") {
			t.Fatalf("panic = %v; want message naming the bad value and the valid set", r)
		}
	}()
	NewServer(&mcp.Implementation{Name: "p", Version: "1"}, Config{Name: "p", Version: "1", SupportedProtocolVersions: []string{protoOld, "1999-01-01"}})
	t.Fatal("NewServer did not panic on an unknown protocol version")
}

func TestCORS_DefaultHeadersCoverMCPTransport(t *testing.T) {
	h, err := Build(mcp.NewServer(&mcp.Implementation{Name: "c", Version: "1"}, nil),
		Config{Name: "c", Version: "1", DisableRequestLog: true, CORSOrigins: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	allow := rec.Header().Get("Access-Control-Allow-Headers")
	for _, name := range []string{"Mcp-Protocol-Version", "Mcp-Session-Id", "Last-Event-ID", "Mcp-Method", "Mcp-Name", "Content-Type", "Authorization"} {
		if !strings.Contains(allow, name) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %s", allow, name)
		}
	}
	if got := rec.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Mcp-Session-Id") {
		t.Errorf("Access-Control-Expose-Headers = %q, want Mcp-Session-Id", got)
	}
}

// --- request body cap ---

func newBodyCapServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.Name, cfg.Version, cfg.DisableRequestLog = "cap", "0.0.1", true
	server := NewServer(&mcp.Implementation{Name: "cap", Version: "0.0.1"}, cfg)
	type lenArgs struct {
		S string `json:"s"`
	}
	AddTool(server, &mcp.Tool{Name: "len"},
		func(_ context.Context, _ *mcp.CallToolRequest, a lenArgs) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: jsonInt(len(a.S))}}}, nil
		})
	return NewTestServer(t, server, cfg)
}

func callLen(t *testing.T, ts *httptest.Server, size int) (string, error) {
	t.Helper()
	sess := connectProto(t, ts, "", nil)
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "len", Arguments: map[string]any{"s": strings.Repeat("a", size)},
	})
	if err != nil {
		return "", err
	}
	return res.Content[0].(*mcp.TextContent).Text, nil
}

// postPadded POSTs a valid 2026-07-28 tools/list whose params carry `pad`
// bytes of padding and returns the HTTP status. It exercises the body cap
// without paying for client-side marshalling and schema validation of a huge
// tool argument (slow under -race).
func postPadded(t *testing.T, ts *httptest.Server, pad int) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"pad":"` + strings.Repeat("a", pad) +
		`","_meta":{"io.modelcontextprotocol/protocolVersion":"` + protoNew +
		`","io.modelcontextprotocol/clientInfo":{"name":"x","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", protoNew)
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

const miB = 1 << 20

// Default cap is go-mcpserver's own 16 MiB, not the SDK's 4 MiB.
func TestMaxRequestBodyBytes_DefaultIs16MiB(t *testing.T) {
	ts := newBodyCapServer(t, Config{})

	for _, tc := range []struct {
		name string
		pad  int
		want int
	}{
		{"just under 16 MiB accepted", 16*miB - 4096, http.StatusOK},
		{"just over 16 MiB rejected", 16*miB + 4096, http.StatusRequestEntityTooLarge},
	} {
		if got := postPadded(t, ts, tc.pad); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}

	// End to end through a real client: a 5 MiB tool argument (above the
	// go-sdk v1.8.0 4 MiB default, fine on v1.6.1) works.
	if got, err := callLen(t, ts, 5*miB); err != nil || got != "5242880" {
		t.Fatalf("5 MiB tool argument: got %q, err %v; want 5242880, nil", got, err)
	}
}

func TestMaxRequestBodyBytes_ExplicitAndUnlimited(t *testing.T) {
	small := newBodyCapServer(t, Config{MaxRequestBodyBytes: miB})
	if got := postPadded(t, small, 2*miB); got != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB with 1 MiB cap: status = %d, want 413", got)
	}
	if got := postPadded(t, small, miB/2); got != http.StatusOK {
		t.Errorf("0.5 MiB with 1 MiB cap: status = %d, want 200", got)
	}

	if testing.Short() {
		t.Skip("17 MiB request is slow under -race; skipped in -short mode")
	}
	unlimited := newBodyCapServer(t, Config{MaxRequestBodyBytes: -1})
	if got := postPadded(t, unlimited, 17*miB); got != http.StatusOK {
		t.Errorf("negative cap should disable the limit: status = %d, want 200", got)
	}
}
