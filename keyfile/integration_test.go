package keyfile

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpserver "github.com/anatolykoptev/go-mcpserver"
)

func TestKeyfile_ToolsListFiltered(t *testing.T) {
	aTok, aSha := mustMint(t)
	bTok, bSha := mustMint(t)
	exp := futureExpiry()
	s, _, _ := openStore(t, fileOf(
		entry("alice", aSha, exp, "ro_health", "ro_metrics"),
		entry("bob", bSha, exp, "ro_services"),
	))
	ts := serve(t, s, false)

	got := toolNames(t, connect(t, ts, aTok))
	slices.Sort(got)
	if want := []string{"ro_health", "ro_metrics"}; !slices.Equal(got, want) {
		t.Errorf("alice tools/list = %v, want %v", got, want)
	}
	if got := toolNames(t, connect(t, ts, bTok)); !slices.Equal(got, []string{"ro_services"}) {
		t.Errorf("bob tools/list = %v, want [ro_services]", got)
	}
}

func TestKeyfile_DeniedToolCall(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	sess := connect(t, serve(t, s, false), aTok)

	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "server_exec"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("tools/call of an unlisted tool must be denied, got %#v", res.Content)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; strings.Contains(txt, "ok:") {
		t.Fatalf("denied tool executed: %q", txt)
	}
	res, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ro_health"})
	if err != nil || res.IsError {
		t.Fatalf("granted tool must run: err=%v res=%#v", err, res)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; txt != "ok:ro_health" {
		t.Errorf("granted tool result = %q", txt)
	}
}

func post(t *testing.T, ts *httptest.Server, token string) int {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestKeyfile_MalformedToken401NotServerError(t *testing.T) {
	aTok, aSha := mustMint(t)
	unknown, _ := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	ts := serve(t, s, false)
	for name, tok := range map[string]string{
		"garbage":   "not-a-token",
		"bad crc":   aTok[:len(aTok)-1] + string(rune(aTok[len(aTok)-1]^0x01)),
		"truncated": aTok[:10],
		"unknown":   unknown,
		"none":      "",
	} {
		if code := post(t, ts, tok); code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, code)
		}
	}
}

func TestKeyfile_ExpiredKeyHTTP401(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	ts := serve(t, s, false)
	if code := post(t, ts, aTok); code == http.StatusUnauthorized {
		t.Fatalf("valid key got 401")
	}
	s.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if code := post(t, ts, aTok); code != http.StatusUnauthorized {
		t.Fatalf("expired key status = %d, want 401", code)
	}
}

func TestKeyfile_RevokeByDeleteHTTP(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	ts := serve(t, s, false)
	if code := post(t, ts, aTok); code == http.StatusUnauthorized {
		t.Fatal("valid key got 401")
	}
	writeAtomic(t, path, emptyFile)
	if code := post(t, ts, aTok); code != http.StatusUnauthorized {
		t.Fatalf("revoked key status = %d, want 401", code)
	}
}

// ToolFilter must not trust anything but the verified snapshot.
func TestKeyfile_NilTokenInfoDenied(t *testing.T) {
	s, _, _ := openStore(t, "")
	f := s.ToolFilter()
	ctx := context.Background()
	for name, info := range map[string]*mcpserver.TokenInfo{
		"nil":         nil,
		"no extra":    {UserID: "alice"},
		"wrong type":  {Extra: map[string]any{extraToolsKey: "ro_health"}},
		"wrong any":   {Extra: map[string]any{extraToolsKey: []any{"ro_health"}}},
		"missing key": {Extra: map[string]any{"other": []string{"ro_health"}}},
	} {
		if f(ctx, "ro_health", info) {
			t.Errorf("%s: filter allowed a call without a valid snapshot", name)
		}
	}
}

func TestKeyfile_RESTBridgeFailsClosed(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	ts := serve(t, s, true)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/api/tools/ro_health", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+aTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK || strings.Contains(string(body), "ok:ro_health") {
		t.Fatalf("REST bridge executed a keyfile-granted tool (status %d, body %s); bridge must fail closed", resp.StatusCode, body)
	}
}

func TestKeyfile_FilterUsesVerifiedSnapshot(t *testing.T) {
	aTok, aSha := mustMint(t)
	exp := futureExpiry()
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, exp, "ro_health")))
	info, err := verify(s, aTok)
	if err != nil {
		t.Fatal(err)
	}
	// The file changes between verify and filter: ro_health is taken away.
	writeAtomic(t, path, fileOf(entry("alice", aSha, exp, "ro_metrics")))
	info2, err := verify(s, aTok) // forces the reload to land
	if err != nil {
		t.Fatal(err)
	}
	f := s.ToolFilter()
	ctx := context.Background()
	if !f(ctx, "ro_health", info) {
		t.Error("decision must follow the snapshot taken at verify time (ro_health was granted then)")
	}
	if f(ctx, "ro_metrics", info) {
		t.Error("snapshot never granted ro_metrics")
	}
	if f(ctx, "ro_health", info2) || !f(ctx, "ro_metrics", info2) {
		t.Error("the next request must see the reloaded grant set")
	}
}

func TestKeyfile_InstallRefusesExistingBearerAuth(t *testing.T) {
	s, _, _ := openStore(t, "")
	existing := &mcpserver.BearerAuth{Verifier: mcpserver.StaticTokenVerifier("x")}
	cfg := mcpserver.Config{BearerAuth: existing}
	if err := s.Install(&cfg); err == nil {
		t.Fatal("Install must refuse to overwrite an existing BearerAuth")
	}
	if cfg.BearerAuth != existing {
		t.Fatal("Install replaced the existing BearerAuth")
	}
	cfg = mcpserver.Config{}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	ba := cfg.BearerAuth
	if ba == nil || ba.Verifier == nil || ba.ToolFilter == nil || ba.LoopbackBypass || ba.LoopbackIdentity != nil {
		t.Fatalf("Install produced %#v", ba)
	}
}

// --- logging ----------------------------------------------------------------

func TestKeyfile_NoTokenMaterialInLogs(t *testing.T) {
	unknownTok, unknownSha := mustMint(t)
	expiredTok, expiredSha := mustMint(t)
	goodTok, goodSha := mustMint(t)
	s, path, buf := openStore(t, fileOf(
		entry("alice", expiredSha, futureExpiry(), "ro_health"),
		entry("bob", goodSha, futureExpiry(), "ro_health"),
	))
	s.now = time.Now
	malformed := goodTok[:len(goodTok)-1] + string(rune(goodTok[len(goodTok)-1]^0x01))

	_, _ = verify(s, unknownTok)
	_, _ = verify(s, malformed)
	s.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	_, _ = verify(s, expiredTok)
	s.now = time.Now
	// a bad entry and a torn file produce their own log lines
	writeAtomic(t, path, fileOf(entry("carol", unknownSha, futureExpiry(), "nope")))
	_, _ = verify(s, goodTok)
	writeAtomic(t, path, "version: 1\nkeys: [[[\nend: true\n")
	_, _ = verify(s, goodTok)
	waitIdle(t, s)

	logs := buf.String()
	if !strings.Contains(logs, "malformed") || !strings.Contains(logs, "unknown") || !strings.Contains(logs, "expired") {
		t.Fatalf("expected a log line for each reject reason, got:\n%s", logs)
	}
	for _, tok := range []string{unknownTok, expiredTok, goodTok, malformed} {
		for i := 0; i+6 <= len(tok); i++ {
			for j := i + 6; j <= len(tok); j++ {
				if strings.Contains(logs, tok[i:j]) {
					t.Fatalf("log contains token material %q:\n%s", tok[i:j], logs)
				}
			}
		}
	}
	for _, sha := range []string{unknownSha, expiredSha, goodSha, hashOf(malformed)} {
		for n := 8; n <= len(sha); n += 8 {
			if strings.Contains(logs, sha[:n]) {
				t.Fatalf("log contains a sha256 prefix %q:\n%s", sha[:n], logs)
			}
		}
	}
}

func TestKeyfile_RejectLogRateLimited(t *testing.T) {
	s, _, buf := openStore(t, "")
	for range 20 {
		tok, _ := mustMint(t)
		_, _ = verify(s, tok)
	}
	if n := strings.Count(buf.String(), "reason=unknown"); n != 1 {
		t.Errorf("reject log lines = %d, want 1 per minute", n)
	}
	if got := s.Status().Rejected.Unknown; got != 20 {
		t.Errorf("unknown reject counter = %d, want 20", got)
	}
}

func TestKeyfile_StatusFileSHA256(t *testing.T) {
	content := fileOf(entry("alice", hashOf("x"), futureExpiry(), "ro_health"))
	s, _, _ := openStore(t, content)
	if got, want := s.Status().FileSHA256, hashOf(content); got != want {
		t.Errorf("FileSHA256 = %q, want %q", got, want)
	}
}
