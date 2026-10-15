package keyfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpserver "github.com/anatolykoptev/go-mcpserver"
)

var testTools = []string{"ro_health", "ro_services", "ro_metrics", "server_exec"}

const testPrefix = "dzk"

// syncBuf is a goroutine-safe log sink.
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

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func futureExpiry() string { return time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339) }

// entry renders one key-file entry; an empty expires omits the field.
func entry(name, sha, expires string, tools ...string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  - name: %s\n    sha256: %s\n", name, sha)
	if expires != "" {
		fmt.Fprintf(&sb, "    expires: %s\n", expires)
	}
	fmt.Fprintf(&sb, "    tools: [%s]\n", strings.Join(tools, ", "))
	return sb.String()
}

func fileOf(entries ...string) string {
	return "version: 1\nkeys:\n" + strings.Join(entries, "")
}

// writeAtomic replaces path via write-tmp-then-rename (new inode).
func writeAtomic(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func mustMint(t *testing.T) (token, sha string) {
	t.Helper()
	token, sha, err := Mint(testPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return token, sha
}

func baseOpts(path string, logger *slog.Logger) Options {
	return Options{
		Path:             path,
		Prefix:           testPrefix,
		Tools:            testTools,
		ReservedPrefixes: []string{"env-", "system-"},
		Logger:           logger,
	}
}

func debugLogger() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// openStore writes content to a fresh key file and opens a store on it with the
// stat cache disabled so every verify re-checks the file.
func openStore(t *testing.T, content string) (*Store, string, *syncBuf) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.yaml")
	if content != "" {
		writeAtomic(t, path, content)
	}
	logger, buf := debugLogger()
	s, err := Open(baseOpts(path, logger))
	if err != nil {
		t.Fatal(err)
	}
	s.statTTL = 0
	return s, path, buf
}

func verify(s *Store, token string) (*mcpserver.TokenInfo, error) {
	return s.Verifier()(context.Background(), token, nil)
}

// bearerRT injects an Authorization header into every request.
type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (rt *bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(c)
}

func newTestMCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "kf-test", Version: "0.0.1"}, nil)
	for _, name := range testTools {
		n := name
		mcp.AddTool(server, &mcp.Tool{Name: n}, func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok:" + n}}}, nil, nil
		})
	}
	return server
}

// serve drives the real Install + Build path.
func serve(t *testing.T, s *Store, rest bool) *httptest.Server {
	t.Helper()
	cfg := mcpserver.Config{Name: "kf-test", Version: "0.0.1", DisableRequestLog: true, RESTBridge: rest}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	return mcpserver.NewTestServer(t, newTestMCPServer(), cfg)
}

func connect(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	tr := &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: &bearerRT{token: token, base: ts.Client().Transport}},
		DisableStandaloneSSE: true,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.0.1"}, nil)
	sess, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func toolNames(t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}
