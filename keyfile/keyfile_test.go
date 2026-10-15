package keyfile

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// --- token format -----------------------------------------------------------

// Independent oracle: both vectors were produced by a separate implementation
// (python3: zlib.crc32 + big-int base62), not by this package.
func TestToken_KnownVectors(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	if got, want := encodeToken("dzk", raw), "dzk1_003aUlTJC7tjlCTQj2uNU3MFagCXG9LRKRcwGkBIDlf1AEavM"; got != want {
		t.Errorf("encodeToken(0..31) = %q, want %q", got, want)
	}
	var zero [32]byte
	if got, want := encodeToken("dzk", zero), "dzk1_"+strings.Repeat("0", 43)+"0L61OG"; got != want {
		t.Errorf("encodeToken(zero) = %q, want %q", got, want)
	}
	// CRC-32/IEEE check value from the CRC catalogue.
	if got := checksum([]byte("123456789")); got != 0xCBF43926 {
		t.Errorf("checksum(123456789) = %#x, want 0xCBF43926", got)
	}
}

func TestMint_Shape(t *testing.T) {
	tok, sha, err := Mint("dzk")
	if err != nil {
		t.Fatal(err)
	}
	if want := len("dzk") + 2 + bodyLen + crcLen; len(tok) != want {
		t.Errorf("token length = %d, want %d", len(tok), want)
	}
	if !strings.HasPrefix(tok, "dzk1_") {
		t.Errorf("token has wrong prefix")
	}
	if sha != hashOf(tok) {
		t.Errorf("returned sha256 does not match the token")
	}
	if !wellFormed("dzk", tok) {
		t.Errorf("minted token fails its own format check")
	}
	tok2, _, _ := Mint("dzk")
	if tok == tok2 {
		t.Errorf("two mints returned the same token")
	}
	for _, bad := range []string{"", "D", "dzk1", "toolongprefix", "dz-k", "1ab"} {
		if _, _, err := Mint(bad); err == nil {
			t.Errorf("Mint(%q) accepted an invalid prefix", bad)
		}
	}
}

func TestOpen_InvalidOptions(t *testing.T) {
	good := baseOpts("/nonexistent/keys.yaml", nil)
	cases := map[string]func(*Options){
		"empty path":      func(o *Options) { o.Path = "" },
		"bad prefix":      func(o *Options) { o.Prefix = "D1" },
		"wildcard tool":   func(o *Options) { o.Tools = []string{"*"} },
		"empty tool":      func(o *Options) { o.Tools = []string{""} },
		"empty reserved":  func(o *Options) { o.ReservedPrefixes = []string{""} },
		"upper reserved":  func(o *Options) { o.ReservedPrefixes = []string{"Env-"} },
		"duplicate tools": func(o *Options) { o.Tools = []string{"a", "a"} },
	}
	for name, mut := range cases {
		o := good
		mut(&o)
		if _, err := Open(o); err == nil {
			t.Errorf("%s: Open accepted invalid options", name)
		}
	}
	if _, err := Open(good); err != nil {
		t.Errorf("missing file must not be an Open error: %v", err)
	}
}

// --- verify: reject paths ---------------------------------------------------

func TestKeyfile_UnknownKey401(t *testing.T) {
	tok, _ := mustMint(t)
	other, otherSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", otherSha, futureExpiry(), "ro_health")))
	if _, err := verify(s, other); err != nil {
		t.Fatalf("known key rejected: %v", err)
	}
	info, err := verify(s, tok) // well-formed, not in the file
	if info != nil || !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unknown well-formed token must be rejected with ErrInvalidToken, got info=%v err=%v", info, err)
	}
}

func TestKeyfile_MalformedChecksum401_NoLookup(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	before := s.lookups.Load()
	// flip the last char of the CRC
	last := tok[len(tok)-1]
	flip := byte('0')
	if last == '0' {
		flip = '1'
	}
	bad := tok[:len(tok)-1] + string(flip)
	if _, err := verify(s, bad); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("bad checksum must be ErrInvalidToken, got %v", err)
	}
	if got := s.lookups.Load(); got != before {
		t.Fatalf("malformed token reached the hash lookup (lookups %d -> %d)", before, got)
	}
}

func TestKeyfile_RejectsWrap401(t *testing.T) {
	tok, sha := mustMint(t)
	_, otherSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(
		entry("alice", sha, futureExpiry(), "ro_health"),
		entry("gone", otherSha, futureExpiry(), "ro_health"),
	))
	cases := map[string]string{
		"empty":        "",
		"wrong prefix": "abc1_" + tok[len("dzk1_"):],
		"wrong ver":    "dzk2_" + tok[len("dzk1_"):],
		"short":        tok[:len(tok)-1],
		"long":         tok + "0",
		"bad charset":  tok[:len(tok)-2] + "!!",
		"bad crc":      tok[:len(tok)-1] + string(rune(tok[len(tok)-1]^0x01)),
		"unknown":      func() string { u, _ := mustMint(t); return u }(),
	}
	for name, in := range cases {
		info, err := verify(s, in)
		if info != nil || !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: want nil info and errors.Is(ErrInvalidToken), got info=%v err=%v", name, info, err)
		}
	}
}

func TestKeyfile_ExpiredKey401(t *testing.T) {
	tok, sha := mustMint(t)
	// expires in the past at load time: dropped.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, past, "ro_health")))
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("key already expired at load must be rejected, got %v", err)
	}
	if st := s.Status(); st.Keys != 0 {
		t.Fatalf("entry already expired at load must be dropped, Status.Keys = %d", st.Keys)
	}

	// expires after load: valid, then the clock passes it.
	s2, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	if _, err := verify(s2, tok); err != nil {
		t.Fatalf("unexpired key rejected: %v", err)
	}
	s2.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	if _, err := verify(s2, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("key must be rejected once expires has passed, got %v", err)
	}
}

func TestKeyfile_ExpiresMandatoryUTC(t *testing.T) {
	cases := map[string]string{
		"missing":     "",
		"offset":      time.Now().Add(24 * time.Hour).In(time.FixedZone("x", 2*3600)).Format(time.RFC3339),
		"date only":   "2099-01-01",
		"lowercase z": "2099-01-01T00:00:00z",
		"garbage":     "tomorrow",
		"utc numeric": "2099-01-01T00:00:00+00:00",
	}
	for name, exp := range cases {
		tok, sha := mustMint(t)
		s, _, _ := openStore(t, fileOf(entry("alice", sha, exp, "ro_health")))
		if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s (%q): entry must be dropped, token was accepted (err=%v)", name, exp, err)
		}
	}
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, "2099-01-01T00:00:00Z", "ro_health")))
	if _, err := verify(s, tok); err != nil {
		t.Errorf("UTC Z expiry must be accepted: %v", err)
	}
}

func TestKeyfile_TokenInfoShape(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, "2099-01-01T00:00:00Z", "ro_health", "ro_metrics", "ro_health")))
	info, err := verify(s, tok)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", info.UserID)
	}
	if d := time.Until(info.Expiration); d <= 0 || d > time.Hour+time.Minute {
		t.Errorf("Expiration must be capped at ~1h from now, got %v", d)
	}
	got, _ := info.Extra[extraToolsKey].([]string)
	if !slices.Equal(got, []string{"ro_health", "ro_metrics"}) {
		t.Errorf("snapshot tools = %v, want [ro_health ro_metrics] (deduplicated)", got)
	}
	got[0] = "tampered" // callers get a copy
	info2, _ := verify(s, tok)
	if g2, _ := info2.Extra[extraToolsKey].([]string); g2[0] != "ro_health" {
		t.Errorf("snapshot is shared between requests: %v", g2)
	}
}

// --- file validation --------------------------------------------------------

func TestKeyfile_StrictDecode(t *testing.T) {
	good, goodSha := mustMint(t)
	_, dupSha := mustMint(t)
	exp := futureExpiry()
	e := entry("alice", goodSha, exp, "ro_health")
	cases := map[string]string{
		"unknown top-level field": "version: 1\nunknown: 1\nkeys:\n" + e,
		"unknown entry field":     "version: 1\nkeys:\n" + e + "    extra: yes\n",
		"version 2":               "version: 2\nkeys:\n" + e,
		"missing version":         "keys:\n" + e,
		"duplicate name":          fileOf(e, entry("alice", dupSha, exp, "ro_health")),
		"syntax error":            "version: 1\nkeys: [\n" + e,
		"empty file":              "\n",
		"second document":         fileOf(e) + "---\nversion: 1\nkeys: []\n",
	}
	for name, content := range cases {
		s, _, _ := openStore(t, content)
		if _, err := verify(s, good); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: whole file must be invalid (deny-all), but a key was accepted (err=%v)", name, err)
		}
		if st := s.Status(); st.LastReloadOK || st.Keys != 0 {
			t.Errorf("%s: Status = %+v, want LastReloadOK=false Keys=0", name, st)
		}
	}
}

func TestKeyfile_ReservedNames(t *testing.T) {
	exp := futureExpiry()
	var entries []string
	toks := map[string]string{}
	for _, name := range []string{"unknown", "anonymous", "env-operator", "system-x", "A", "1abc", "a", "bad_name", strings.Repeat("a", 33)} {
		tok, sha := mustMint(t)
		toks[name] = tok
		entries = append(entries, entry(name, sha, exp, "ro_health"))
	}
	okTok, okSha := mustMint(t)
	entries = append(entries, entry("muse", okSha, exp, "ro_health"))
	s, _, _ := openStore(t, fileOf(entries...))
	for name, tok := range toks {
		if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("name %q must be rejected, token was accepted (err=%v)", name, err)
		}
	}
	if _, err := verify(s, okTok); err != nil {
		t.Errorf("valid sibling entry must survive: %v", err)
	}
}

func TestKeyfile_BadHashDropped(t *testing.T) {
	tok, sha := mustMint(t)
	for name, h := range map[string]string{"upper": strings.ToUpper(sha), "short": sha[:63], "nonhex": strings.Repeat("g", 64)} {
		s, _, _ := openStore(t, fileOf(entry("alice", h, futureExpiry(), "ro_health")))
		if _, err := verify(s, tok); err == nil {
			t.Errorf("%s: invalid hash entry accepted", name)
		}
	}
}

func TestKeyfile_ToolNotRegisteredDropsEntry(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health", "not_a_registered_tool")))
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("entry naming an unregistered tool must be dropped, got err=%v", err)
	}
}

func TestKeyfile_EmptyToolsGrantsNothing(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry())))
	if _, err := verify(s, tok); err == nil {
		t.Fatal("entry with an empty tool list must be dropped")
	}
	f := s.ToolFilter()
	info := &auth.TokenInfo{Extra: map[string]any{extraToolsKey: []string{}}}
	if f(context.Background(), "ro_health", info) {
		t.Fatal("empty tool list granted a tool")
	}
}

func TestKeyfile_NoWildcard(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), `"*"`)))
	if _, err := verify(s, tok); err == nil {
		t.Fatal(`entry with tools ["*"] must be dropped`)
	}
	info := &auth.TokenInfo{Extra: map[string]any{extraToolsKey: []string{"*"}}}
	if s.ToolFilter()(context.Background(), "ro_health", info) {
		t.Fatal(`a "*" tool entry acted as a wildcard in the filter`)
	}
}

func TestKeyfile_BadEntryDroppedAlone(t *testing.T) {
	badTok, badSha := mustMint(t)
	goodTok, goodSha := mustMint(t)
	s, _, buf := openStore(t, fileOf(
		entry("alice", badSha, futureExpiry(), "nope"),
		entry("bob", goodSha, futureExpiry(), "ro_health"),
	))
	if _, err := verify(s, goodTok); err != nil {
		t.Fatalf("valid entry beside a bad one must still authenticate: %v", err)
	}
	if _, err := verify(s, badTok); err == nil {
		t.Fatal("bad entry authenticated")
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("a dropped entry must be logged at ERROR; log=%s", buf.String())
	}
	if st := s.Status(); !st.LastReloadOK || st.Keys != 1 {
		t.Errorf("Status = %+v, want LastReloadOK=true Keys=1", st)
	}
}

func TestKeyfile_MissingFileDeniesAll(t *testing.T) {
	tok, _ := mustMint(t)
	s, _, _ := openStore(t, "")
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("missing file must deny, got %v", err)
	}
	if st := s.Status(); !st.LastReloadOK || st.Keys != 0 {
		t.Errorf("Status = %+v, want LastReloadOK=true Keys=0", st)
	}
}

// --- reload -----------------------------------------------------------------

func TestKeyfile_RevokeByDelete_NoRestart(t *testing.T) {
	aTok, aSha := mustMint(t)
	bTok, bSha := mustMint(t)
	exp := futureExpiry()
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, exp, "ro_health"), entry("bob", bSha, exp, "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatal(err)
	}
	writeAtomic(t, path, fileOf(entry("bob", bSha, exp, "ro_health"))) // rename-replace
	if _, err := verify(s, aTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("revoked key must be rejected on the next call, got %v", err)
	}
	if _, err := verify(s, bTok); err != nil {
		t.Fatalf("remaining key rejected: %v", err)
	}
}

func TestKeyfile_ReloadTriggeredBySizeAndMtimeNotJustInode(t *testing.T) {
	aTok, aSha := mustMint(t)
	exp := futureExpiry()
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, exp, "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatal(err)
	}
	// In-place truncate to an empty-keys file: same inode, different size.
	if err := os.WriteFile(path, []byte("version: 1\nkeys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(s, aTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("in-place edit must trigger a reload, got %v", err)
	}
}

func TestKeyfile_StatCachedWithinTTL(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	s.statTTL = time.Hour
	if _, err := verify(s, aTok); err != nil { // primes the stat timestamp
		t.Fatal(err)
	}
	writeAtomic(t, path, "version: 1\nkeys: []\n")
	if _, err := verify(s, aTok); err != nil {
		t.Fatalf("within the stat TTL the file must not be re-stat'ed: %v", err)
	}
}

func TestKeyfile_ParseErrorDeniesAll(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, path, buf := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatal(err)
	}
	writeAtomic(t, path, "version: 1\nkeys: [[[\n")
	_, _ = verify(s, aTok) // triggers the reload; old snapshot may serve during the retry window
	time.Sleep(retryDelay + 150*time.Millisecond)
	if _, err := verify(s, aTok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("a persistent parse error must deny all (no keep-last-good), got %v", err)
	}
	if st := s.Status(); st.LastReloadOK || st.Keys != 0 {
		t.Errorf("Status = %+v, want LastReloadOK=false Keys=0", st)
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("deny-all must be logged at ERROR; log=%s", buf.String())
	}
	// A later fix recovers on the next change.
	writeAtomic(t, path, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	if _, err := verify(s, aTok); err != nil {
		t.Fatalf("a repaired file must recover: %v", err)
	}
	if !s.Status().LastReloadOK {
		t.Error("LastReloadOK must be true after recovery")
	}
}

// A torn write (non-atomic editor) is repaired within the 50 ms retry window.
func TestKeyfile_TornWriteRetry(t *testing.T) {
	aTok, aSha := mustMint(t)
	full := fileOf(entry("alice", aSha, futureExpiry(), "ro_health"))
	s2, path2, _ := openStore(t, full)
	// valid -> torn -> repaired inside the retry window
	if _, err := verify(s2, aTok); err != nil {
		t.Fatal(err)
	}
	writeAtomic(t, path2, full[:len(full)-4])
	_, _ = verify(s2, aTok) // triggers reload; first read fails
	if _, err := verify(s2, aTok); err != nil {
		t.Fatalf("the previous key set must keep serving during the retry window: %v", err)
	}
	time.AfterFunc(retryDelay/5, func() { writeAtomic(t, path2, full+"# repaired\n") })
	time.Sleep(retryDelay + 200*time.Millisecond)
	if _, err := verify(s2, aTok); err != nil {
		t.Fatalf("a torn write repaired within the retry window must not lock the key out: %v", err)
	}
	if st := s2.Status(); !st.LastReloadOK || st.Keys != 1 {
		t.Errorf("Status = %+v, want LastReloadOK=true Keys=1", st)
	}
}

// Seam variant: the first read is truncated, the second is valid.
func TestKeyfile_TornWriteRetry_FirstReadTruncated(t *testing.T) {
	aTok, aSha := mustMint(t)
	full := fileOf(entry("alice", aSha, futureExpiry(), "ro_health"))
	s, path, _ := openStore(t, "version: 1\nkeys: []\n")
	var mu sync.Mutex
	reads := 0
	s.readFile = func(p string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		b, err := os.ReadFile(p)
		if reads == 1 && err == nil {
			return b[:len(b)-4], nil
		}
		return b, err
	}
	writeAtomic(t, path, full)
	_, _ = verify(s, aTok) // first read truncated -> retry scheduled
	time.Sleep(retryDelay + 200*time.Millisecond)
	if _, err := verify(s, aTok); err != nil {
		t.Fatalf("second read is valid, key must be accepted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if reads != 2 {
		t.Errorf("reads = %d, want exactly 2 (one retry)", reads)
	}
}

func TestKeyfile_ReloadSingleFlight(t *testing.T) {
	aTok, aSha := mustMint(t)
	full := fileOf(entry("alice", aSha, futureExpiry(), "ro_health"))
	s, path, _ := openStore(t, full)
	writeAtomic(t, path, full[:len(full)-4]) // torn
	before := s.reloads.Load()

	const n = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	durs := make([]time.Duration, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			t0 := time.Now()
			_, errs[i] = verify(s, aTok)
			durs[i] = time.Since(t0)
		}()
	}
	close(start)
	wg.Wait()
	for i := range n {
		if durs[i] > 40*time.Millisecond {
			t.Errorf("verify %d blocked for %v during a reload (limit 40ms, retry window is 50ms)", i, durs[i])
		}
		if errs[i] != nil {
			t.Errorf("verify %d: old snapshot must serve during the retry window, got %v", i, errs[i])
		}
	}
	time.Sleep(retryDelay + 150*time.Millisecond)
	if got := s.reloads.Load() - before; got != 1 {
		t.Errorf("reload cycles = %d, want exactly 1", got)
	}
}

// --- composition ------------------------------------------------------------

func TestKeyfile_ComposableVerifier(t *testing.T) {
	aTok, aSha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", aSha, futureExpiry(), "ro_health")))
	legacy := "legacy-secret"
	chain := func(next auth.TokenVerifier) auth.TokenVerifier {
		return func(ctx context.Context, tok string, r *http.Request) (*auth.TokenInfo, error) {
			if tok == legacy {
				return &auth.TokenInfo{UserID: "legacy", Expiration: time.Now().Add(time.Minute)}, nil
			}
			return next(ctx, tok, r)
		}
	}
	v := chain(s.Verifier())
	if info, err := v(context.Background(), legacy, nil); err != nil || info.UserID != "legacy" {
		t.Fatalf("legacy path: %v %v", info, err)
	}
	if info, err := v(context.Background(), aTok, nil); err != nil || info.UserID != "alice" {
		t.Fatalf("keyfile path: %v %v", info, err)
	}
	if _, err := v(context.Background(), "nope", nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("unknown through chain: %v", err)
	}
}
