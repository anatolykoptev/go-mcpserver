package keyfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpserver "github.com/anatolykoptev/go-mcpserver"
)

// --- reload cannot wedge ------------------------------------------------------

func TestKeyfile_ValidatorPanicDropsOnlyThatEntry(t *testing.T) {
	aTok, aSha := mustMint(t)
	bTok, bSha := mustMint(t)
	exp := futureExpiry()
	content := fileOf(
		entryAttrs("alice", aSha, "      role: boom\n", "ro_health"),
		entryAttrs("bob", bSha, "      role: ok\n", "ro_health"),
	)
	s, path, buf := openWith(t, content, func(o *Options) {
		o.ValidateAttrs = func(_ string, attrs map[string]string) error {
			if attrs["role"] == "boom" {
				panic("consumer bug with secret-looking text " + aSha)
			}
			return nil
		}
	})
	if _, err := verify(s, aTok); err == nil {
		t.Fatal("entry whose validator panicked must be dropped")
	}
	if _, err := verify(s, bTok); err != nil {
		t.Fatalf("sibling entry must survive a validator panic: %v", err)
	}
	if !strings.Contains(buf.String(), "validator panic") {
		t.Errorf("drop reason not logged: %s", buf.String())
	}
	if strings.Contains(buf.String(), aSha) {
		t.Errorf("panic value leaked into the log")
	}
	// A later revocation still takes effect: the store is not wedged.
	writeAtomic(t, path, fileOf(
		entryAttrs("alice", aSha, "      role: boom\n", "ro_health"),
		entry("carol", hashOf("x"), exp, "ro_health"),
	))
	if _, err := verify(s, bTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("revoked key still accepted after a validator panic: %v", err)
	}
	if s.reloading.Load() {
		t.Fatal("reloading flag stuck after a validator panic")
	}
}

// A panic outside the validator (here: the file read) must neither wedge the
// reload flag nor keep a revoked key alive.
func TestKeyfile_ReloadPanicDoesNotWedge(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatal(err)
	}
	base := s.readFile
	var panicking atomic.Bool
	panicking.Store(true)
	s.readFile = func(p string) ([]byte, error) {
		if panicking.Load() {
			panic("boom")
		}
		return base(p)
	}
	writeAtomic(t, path, emptyFile) // revoke alice
	_, _ = verifyWithin(t, s, aTok, 5*time.Second)
	waitIdle(t, s)
	if _, err := verify(s, aTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("after a failed reload the store must deny all, got %v", err)
	}
	waitIdle(t, s) // a wedged flag would never clear
	// Recovery on the next pass once reads work again.
	panicking.Store(false)
	writeAtomic(t, path, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatalf("store did not recover after the panic cleared: %v", err)
	}
}

// Same on the retry path: the first read is torn, the retry panics.
func TestKeyfile_RetryPanicDoesNotKillOrWedge(t *testing.T) {
	aTok, aSha := mustMint(t)
	full := fileOf(entry("alice", aSha, futureExpiry(), "ro_health"))
	s, path, _ := openStore(t, full)
	release := gateRetry(t, s)
	base := s.readFile
	var calls atomic.Int32
	s.readFile = func(p string) ([]byte, error) {
		b, err := base(p)
		switch calls.Add(1) {
		case 1:
			return b[:len(b)-4], err // torn
		default:
			panic("boom in retry")
		}
	}
	writeAtomic(t, path, "# changed\n"+full)
	if _, err := verifyWithin(t, s, aTok, 5*time.Second); err != nil {
		t.Fatalf("old set serves during the window: %v", err)
	}
	release()
	waitIdle(t, s) // the process is still alive and the flag cleared
	if _, err := verify(s, aTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a panicking retry must end in deny-all, got %v", err)
	}
	if st := s.Status(); st.LastReloadOK {
		t.Error("LastReloadOK must be false")
	}
}

// --- API shape ------------------------------------------------------------------

func TestKeyfile_BearerAuthCarriesVerifierAndFilter(t *testing.T) {
	s, _, _ := openStore(t, emptyFile)
	ba := s.BearerAuth()
	if ba == nil || ba.Verifier == nil || ba.ToolFilter == nil {
		t.Fatalf("BearerAuth() must always carry both halves: %#v", ba)
	}
	if ba.LoopbackBypass || ba.LoopbackIdentity != nil {
		t.Fatal("BearerAuth() must not enable a loopback bypass")
	}
	if ba.ToolFilter(context.Background(), "ro_health", nil) {
		t.Fatal("the paired filter must deny a missing identity")
	}
}

func TestKeyfile_InstallRefusesRESTBridge(t *testing.T) {
	s, _, _ := openStore(t, emptyFile)
	cfg := mcpserver.Config{RESTBridge: true}
	if err := s.Install(&cfg); err == nil {
		t.Fatal("Install must refuse a Config with RESTBridge set")
	}
	if cfg.BearerAuth != nil {
		t.Fatal("Install left a BearerAuth behind on refusal")
	}
}

func TestKeyfile_StatusFileMissing(t *testing.T) {
	s, _, _ := openStore(t, "")
	if st := s.Status(); !st.FileMissing || !st.LastReloadOK || st.Keys != 0 {
		t.Errorf("Status = %+v, want FileMissing=true LastReloadOK=true Keys=0", st)
	}
	s2, _, _ := openStore(t, emptyFile)
	if s2.Status().FileMissing {
		t.Error("FileMissing set for an existing file")
	}
}

// --- per-request identity in handlers --------------------------------------------

// In stateful mode the SDK keeps the connection context of the request that
// opened the session; Attrs must still reflect the current request.
func TestKeyfile_AttrsFreshInStatefulSession(t *testing.T) {
	tok, sha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entryAttrs("alice", sha, "      scope: wide\n", "ro_health")))

	server := mcp.NewServer(&mcp.Implementation{Name: "stateful", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ro_health"}, func(ctx context.Context, req *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		out := map[string]string{
			"ctx": Attrs(ctx)["scope"],
			"req": AttrsOf(req.Extra.TokenInfo)["scope"],
		}
		b, _ := json.Marshal(out)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
	})
	stateful := false
	cfg := mcpserver.Config{
		Name: "stateful", Version: "0.0.1", DisableRequestLog: true,
		Stateless: &stateful, SessionTimeout: time.Minute,
	}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	ts := mcpserver.NewTestServer(t, server, cfg)
	sess := connect(t, ts, tok)

	call := func() map[string]string {
		t.Helper()
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "ro_health"})
		if err != nil || res.IsError {
			t.Fatalf("call: err=%v res=%#v", err, res)
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := call(); got["ctx"] != "wide" || got["req"] != "wide" {
		t.Fatalf("first call = %v, want wide", got)
	}
	// Narrow the same key (same name, same hash) while the session stays open.
	writeAtomic(t, path, fileOf(entryAttrs("alice", sha, "      scope: narrow\n", "ro_health")))
	if got := call(); got["ctx"] != "narrow" || got["req"] != "narrow" {
		t.Fatalf("after narrowing, handler saw %v; Attrs(ctx) and req.Extra must both report narrow", got)
	}
}

// --- non-tool methods ---------------------------------------------------------------

func resourceServer() *mcp.Server {
	server := newTestMCPServer()
	server.AddResource(&mcp.Resource{Name: "secret", URI: "test://secret", MIMEType: "text/plain"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "test://secret", Text: "classified"}}}, nil
		})
	server.AddPrompt(&mcp.Prompt{Name: "secret_prompt"},
		func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "classified"}}}}, nil
		})
	return server
}

func TestKeyfile_ScopedKeyCannotReachResourcesOrPrompts(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	cfg := mcpserver.Config{Name: "kf-test", Version: "0.0.1", DisableRequestLog: true}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	ts := mcpserver.NewTestServer(t, resourceServer(), cfg)
	sess := connect(t, ts, tok)
	ctx := context.Background()

	if res, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: "test://secret"}); err == nil {
		t.Errorf("resources/read must be denied for a tool-scoped key, got %#v", res)
	}
	if res, err := sess.ListResources(ctx, nil); err == nil {
		t.Errorf("resources/list must be denied, got %#v", res)
	}
	if res, err := sess.GetPrompt(ctx, &mcp.GetPromptParams{Name: "secret_prompt"}); err == nil {
		t.Errorf("prompts/get must be denied for a tool-scoped key, got %#v", res)
	}
	if res, err := sess.ListPrompts(ctx, nil); err == nil {
		t.Errorf("prompts/list must be denied, got %#v", res)
	}
	// Tools still work.
	if names := toolNames(t, sess); len(names) != 1 || names[0] != "ro_health" {
		t.Errorf("tools/list = %v", names)
	}
	if res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ro_health"}); err != nil || res.IsError {
		t.Errorf("granted tool call failed: %v %#v", err, res)
	}
}

func TestKeyfile_AllowNonToolMethodsOptOut(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openWith(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health")),
		func(o *Options) { o.AllowNonToolMethods = true })
	cfg := mcpserver.Config{Name: "kf-test", Version: "0.0.1", DisableRequestLog: true}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	ts := mcpserver.NewTestServer(t, resourceServer(), cfg)
	sess := connect(t, ts, tok)
	res, err := sess.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "test://secret"})
	if err != nil || res.Contents[0].Text != "classified" {
		t.Fatalf("opt-out must allow resources/read: %v %#v", err, res)
	}
}

// --- file shape ------------------------------------------------------------------------

// Every byte-prefix of a valid file is either invalid (deny-all) or means
// exactly what the full file means.
func TestKeyfile_EveryTruncationIsInvalidOrEquivalent(t *testing.T) {
	_, aSha := mustMint(t)
	_, bSha := mustMint(t)
	exp := futureExpiry()
	full := fileOf(
		entryAttrs("alice", aSha, "      role: reader\n", "ro_health", "ro_metrics"),
		entry("bob", bSha, exp, "ro_services", "server_exec"),
	)
	s, _, _ := openStore(t, full)
	want, err := s.parse([]byte(full))
	if err != nil || len(want) != 2 {
		t.Fatalf("control: %d entries, err=%v", len(want), err)
	}
	for i := range len(full) {
		got, err := s.parse([]byte(full[:i]))
		if err != nil {
			continue
		}
		if !sameEntries(got, want) {
			t.Errorf("prefix of %d/%d bytes parsed to a different key set (%d entries); last line %q",
				i, len(full), len(got), lastNonBlankLine([]byte(full[:i])))
		}
	}
}

func sameEntries(a, b []*keyEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].name != b[i].name || a[i].digest != b[i].digest ||
			fmt.Sprint(a[i].tools) != fmt.Sprint(b[i].tools) || fmt.Sprint(a[i].attrs) != fmt.Sprint(b[i].attrs) {
			return false
		}
	}
	return true
}

func TestKeyfile_DuplicateNameDropsEveryCopy(t *testing.T) {
	aTok, aSha := mustMint(t)
	bTok, bSha := mustMint(t)
	cTok, cSha := mustMint(t)
	exp := futureExpiry()
	s, _, buf := openStore(t, fileOf(
		entry("alice", aSha, exp, "ro_health"),
		entry("alice", bSha, exp, "ro_health"),
		entry("carol", cSha, exp, "ro_health"),
	))
	for _, tok := range []string{aTok, bTok} {
		if _, err := verify(s, tok); err == nil {
			t.Error("a key sharing a duplicated name must be dropped")
		}
	}
	if _, err := verify(s, cTok); err != nil {
		t.Errorf("unrelated entry must survive: %v", err)
	}
	if !s.Status().LastReloadOK || !strings.Contains(buf.String(), "duplicate name") {
		t.Errorf("want a healthy store and an ERROR naming the duplicate; log=%s", buf.String())
	}
}

func TestKeyfile_DuplicateHashDropsEveryCopy(t *testing.T) {
	aTok, aSha := mustMint(t)
	cTok, cSha := mustMint(t)
	exp := futureExpiry()
	s, _, buf := openStore(t, fileOf(
		entry("alice", aSha, exp, "ro_health"),
		entry("bobby", aSha, exp, "ro_metrics"),
		entry("carol", cSha, exp, "ro_health"),
	))
	if _, err := verify(s, aTok); err == nil {
		t.Error("a hash claimed by two entries must be dropped for both")
	}
	if _, err := verify(s, cTok); err != nil {
		t.Errorf("unrelated entry must survive: %v", err)
	}
	if !strings.Contains(buf.String(), "duplicate sha256") {
		t.Errorf("log=%s", buf.String())
	}
}

// --- reload edge cases --------------------------------------------------------------------

// A transient read error must not pin the failure: the same file is re-read.
func TestKeyfile_TransientReadErrorIsRetriedWithoutFileChange(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	base := s.readFile
	var failing atomic.Bool
	failing.Store(true)
	s.readFile = func(p string) ([]byte, error) {
		if failing.Load() {
			return nil, errors.New("simulated EIO")
		}
		return base(p)
	}
	writeAtomic(t, path, fileOf(entry("alice", aSha, futureExpiry(), "ro_health", "ro_metrics")))
	_, _ = verifyWithin(t, s, aTok, 5*time.Second)
	waitIdle(t, s)
	if _, err := verify(s, aTok); err == nil {
		t.Fatal("while reads fail the store must deny all")
	}
	waitIdle(t, s)
	failing.Store(false) // no file change at all
	if _, err := verify(s, aTok); err != nil {
		t.Fatalf("the unchanged file must be re-read after a transient error: %v", err)
	}
}

func TestKeyfile_ExpiryBoundary(t *testing.T) {
	tok, sha := mustMint(t)
	const exp = "2099-01-01T00:00:00Z"
	expires, _ := time.Parse(time.RFC3339, exp)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, exp, "ro_health")))
	s.now = func() time.Time { return expires.Add(-time.Nanosecond) }
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("one tick before expires must be accepted: %v", err)
	}
	s.now = func() time.Time { return expires }
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a token is rejected at exactly expires, got %v", err)
	}
}

// --- size cap ----------------------------------------------------------------------------

func paddedToSize(body string, size int) string {
	return "#" + strings.Repeat("x", size-len(body)-2) + "\n" + body
}

func TestKeyfile_FileSizeCapPinned(t *testing.T) {
	tok, sha := mustMint(t)
	body := fileOf(entry("alice", sha, futureExpiry(), "ro_health"))

	atCap := paddedToSize(body, maxFileSize)
	s, path, _ := openStore(t, atCap)
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("a file of exactly %d bytes must be accepted: %v", maxFileSize, err)
	}
	if data, err := s.readChecked(path); err != nil || len(data) != maxFileSize {
		t.Fatalf("readChecked at cap: len=%d err=%v", len(data), err)
	}

	over := paddedToSize(body, maxFileSize+1)
	s2, path2, _ := openStore(t, over)
	if _, err := verify(s2, tok); err == nil {
		t.Fatalf("a file of %d bytes must be refused", maxFileSize+1)
	}
	if s2.Status().LastReloadOK {
		t.Error("an oversized file must leave the store in deny-all")
	}
	if _, err := s2.readChecked(path2); err == nil || err.Error() != "file too large" {
		t.Errorf("the size must be checked before reading the file, got %v", err)
	}
	if _, err := s2.parse([]byte(over)); err == nil || err.Error() != "file too large" {
		t.Errorf("parse must refuse oversized input, got %v", err)
	}
}

// --- follow-ups from #49 ------------------------------------------------------------------

// middlewareAllows reports whether the middleware lets method through to next.
func middlewareAllows(t *testing.T, s *Store, method string) bool {
	t.Helper()
	called := false
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		called = true
		return nil, nil
	}
	_, _ = s.Middleware()(next)(context.Background(), method, &mcp.ServerRequest[*mcp.PingParams]{})
	return called
}

func TestKeyfile_ScopedMethodAllowlist(t *testing.T) {
	s, _, _ := openStore(t, emptyFile)
	for _, m := range []string{
		"initialize", "ping", "server/discover", "tools/list", "tools/call",
		"notifications/initialized", "notifications/cancelled",
	} {
		if !middlewareAllows(t, s, m) {
			t.Errorf("%s must pass for a scoped key", m)
		}
	}
	for _, m := range []string{
		"tools/foo", "tools/", "tools/list/extra", "tools", "server/other", "server/",
		"resources/read", "prompts/get", "completion/complete", "logging/setLevel", "",
	} {
		if middlewareAllows(t, s, m) {
			t.Errorf("%q must be default-denied for a scoped key", m)
		}
	}
}

func TestKeyfile_ServerDiscoverOptOutStillAllowsEverything(t *testing.T) {
	s, _, _ := openWith(t, emptyFile, func(o *Options) { o.AllowNonToolMethods = true })
	if !middlewareAllows(t, s, "tools/foo") {
		t.Error("AllowNonToolMethods must let every method through")
	}
}

// A backward wall-clock step must not freeze the reload gate: the revoked key
// has to stop authenticating on the next verification after the file changed.
func TestKeyfile_BackwardClockStepDoesNotFreezeReload(t *testing.T) {
	tok, sha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entry("alice", sha, "2099-01-01T00:00:00Z", "ro_health")))
	s.statTTL = time.Second
	base := time.Now()
	s.now = func() time.Time { return base }
	if _, err := verify(s, tok); err != nil {
		t.Fatal(err)
	}
	// Step the clock back an hour, then revoke the key.
	s.now = func() time.Time { return base.Add(-time.Hour) }
	writeAtomic(t, path, emptyFile)
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("revoked key must stop authenticating after a backward clock step, got %v", err)
	}
}

// The reject/warn rate limiter shares the same clock hazard.
func TestKeyfile_BackwardClockStepDoesNotSilenceWarnings(t *testing.T) {
	s, _, _ := openStore(t, emptyFile)
	base := time.Now()
	var last atomic.Int64
	s.now = func() time.Time { return base }
	if !s.warnDue(&last) {
		t.Fatal("first warning must be due")
	}
	s.now = func() time.Time { return base.Add(-time.Hour) }
	if !s.warnDue(&last) {
		t.Error("a backward clock step must not suppress warnings until the clock catches up")
	}
}
