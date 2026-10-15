package keyfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpserver "github.com/anatolykoptev/go-mcpserver"
)

func openWith(t *testing.T, content string, mut func(*Options)) (*Store, string, *syncBuf) {
	t.Helper()
	path := filepath.Join(privateDir(t), "keys.yaml")
	writeAtomic(t, path, content)
	logger, buf := debugLogger()
	o := baseOpts(path, logger)
	if mut != nil {
		mut(&o)
	}
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	s.statTTL = 0
	return s, path, buf
}

func entryAttrs(name, sha string, attrs string, tools ...string) string {
	return entry(name, sha, futureExpiry(), tools...) + "    attrs:\n" + attrs
}

// A tool handler sees the verified key's attrs through Attrs(ctx).
func TestKeyfile_AttrsRoundTripToHandler(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entryAttrs("alice", sha,
		"      role: reader\n      run_as: svc_ro\n      exec_mode: safe\n", "ro_health")))

	server := mcp.NewServer(&mcp.Implementation{Name: "attrs", Version: "0.0.1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ro_health"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
		b, _ := json.Marshal(Attrs(ctx))
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil, nil
	})
	cfg := mcpserver.Config{Name: "attrs", Version: "0.0.1", DisableRequestLog: true}
	if err := s.Install(&cfg); err != nil {
		t.Fatal(err)
	}
	ts := mcpserver.NewTestServer(t, server, cfg)
	res, err := connect(t, ts, tok).CallTool(context.Background(), &mcp.CallToolParams{Name: "ro_health"})
	if err != nil || res.IsError {
		t.Fatalf("call: err=%v res=%#v", err, res)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"role": "reader", "run_as": "svc_ro", "exec_mode": "safe"}
	if !maps.Equal(got, want) {
		t.Fatalf("handler saw attrs %v, want %v", got, want)
	}
}

func TestKeyfile_AttrsOptionalDefaultsEmpty(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	info, err := verify(s, tok)
	if err != nil {
		t.Fatal(err)
	}
	if a := AttrsOf(info); a == nil || len(a) != 0 {
		t.Fatalf("attrs = %#v, want empty non-nil map", a)
	}
	if AttrsOf(nil) != nil || AttrsOf(&auth.TokenInfo{}) != nil || Attrs(context.Background()) != nil {
		t.Fatal("no keyfile identity must yield nil attrs")
	}
}

// A malformed attrs map invalidates that entry only.
func TestKeyfile_AttrsInvalidShapeDropsEntry(t *testing.T) {
	tok, sha := mustMint(t)
	goodTok, goodSha := mustMint(t)
	many := strings.Builder{}
	for i := range 33 {
		fmt.Fprintf(&many, "      k%d: v\n", i)
	}
	cases := map[string]string{
		"uppercase key":  "      Role: x\n",
		"leading digit":  "      1a: x\n",
		"dash in key":    "      a-b: x\n",
		"key too long":   "      a" + strings.Repeat("b", 32) + ": x\n",
		"value too long": "      role: " + strings.Repeat("x", 257) + "\n",
		"too many attrs": many.String(),
		"nested map":     "      role:\n        nested: x\n",
		"list value":     "      role: [a, b]\n",
		"integer value":  "      role: 5\n",
		"bool value":     "      role: true\n",
	}
	for name, attrs := range cases {
		s, _, _ := openStore(t, fileOf(
			entryAttrs("alice", sha, attrs, "ro_health"),
			entry("bob", goodSha, futureExpiry(), "ro_health"),
		))
		if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: entry with a bad attrs shape must be dropped, key accepted (err=%v)", name, err)
		}
		if _, err := verify(s, goodTok); err != nil {
			t.Errorf("%s: sibling entry must survive: %v", name, err)
		}
	}
	// boundaries are accepted
	okAttrs := "      a" + strings.Repeat("b", 31) + ": " + strings.Repeat("x", 256) + "\n"
	s, _, _ := openStore(t, fileOf(entryAttrs("alice", sha, okAttrs, "ro_health")))
	if _, err := verify(s, tok); err != nil {
		t.Errorf("max-size attrs rejected: %v", err)
	}
}

func TestKeyfile_UnknownEntryFieldStillInvalidWithAttrs(t *testing.T) {
	tok, sha := mustMint(t)
	s, _, _ := openStore(t, fileOf(entryAttrs("alice", sha, "      role: x\n", "ro_health")+"    bogus: 1\n"))
	if _, err := verify(s, tok); err == nil {
		t.Fatal("unknown entry field must still invalidate the file")
	}
}

func TestKeyfile_ValidatorDropsOnlyThatEntry(t *testing.T) {
	badTok, badSha := mustMint(t)
	goodTok, goodSha := mustMint(t)
	var calls atomic.Int32
	validator := func(name string, attrs map[string]string) error {
		calls.Add(1)
		if attrs["role"] == "forbidden" {
			return fmt.Errorf("role %q not allowed", attrs["role"])
		}
		attrs["role"] = "mutated-by-validator" // must not leak into the store
		return nil
	}
	content := fileOf(
		entryAttrs("alice", badSha, "      role: forbidden\n", "ro_health"),
		entryAttrs("bob", goodSha, "      role: reader\n", "ro_health"),
	)
	s, path, buf := openWith(t, content, func(o *Options) { o.ValidateAttrs = validator })
	if _, err := verify(s, badTok); err == nil {
		t.Fatal("entry rejected by the validator authenticated")
	}
	info, err := verify(s, goodTok)
	if err != nil {
		t.Fatalf("sibling entry must survive: %v", err)
	}
	if got := AttrsOf(info)["role"]; got != "reader" {
		t.Fatalf("validator mutated stored attrs: role = %q", got)
	}
	logs := buf.String()
	if !strings.Contains(logs, "alice") || !strings.Contains(logs, "level=ERROR") {
		t.Errorf("dropped entry must be logged at ERROR by name; log=%s", logs)
	}
	for _, secret := range []string{badTok, goodTok, badSha, goodSha} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains token material: %s", logs)
		}
	}
	// runs again on every reload
	before := calls.Load()
	writeAtomic(t, path, "# touched\n"+content)
	if _, err := verify(s, goodTok); err != nil {
		t.Fatal(err)
	}
	if calls.Load() <= before {
		t.Errorf("validator not re-run on reload (calls %d -> %d)", before, calls.Load())
	}
	// a reload that now violates the validator drops the key
	writeAtomic(t, path, fileOf(entryAttrs("bob", goodSha, "      role: forbidden\n", "ro_health")))
	if _, err := verify(s, goodTok); err == nil {
		t.Error("reloaded entry must be re-validated")
	}
}

func TestKeyfile_AttrsSnapshotImmuneToStoreChanges(t *testing.T) {
	tok, sha := mustMint(t)
	s, path, _ := openStore(t, fileOf(entryAttrs("alice", sha, "      role: reader\n", "ro_health")))
	info, err := verify(s, tok)
	if err != nil {
		t.Fatal(err)
	}
	// 1. mutate the store's own entry after verify
	for _, e := range s.snap.Load().keys {
		e.attrs["role"] = "admin"
	}
	if got := AttrsOf(info)["role"]; got != "reader" {
		t.Fatalf("snapshot follows in-store mutation: role = %q", got)
	}
	// 2. the file changes and reloads between verify and use
	writeAtomic(t, path, fileOf(entryAttrs("alice", sha, "      role: other\n", "ro_health")))
	info2, err := verify(s, tok)
	if err != nil {
		t.Fatal(err)
	}
	if got := AttrsOf(info)["role"]; got != "reader" {
		t.Fatalf("snapshot changed by reload: role = %q", got)
	}
	if got := AttrsOf(info2)["role"]; got != "other" {
		t.Fatalf("next request must see reloaded attrs, got %q", got)
	}
	// 3. Attrs returns a copy
	AttrsOf(info)["role"] = "tampered"
	if got := AttrsOf(info)["role"]; got != "reader" {
		t.Fatalf("AttrsOf returned a shared map: role = %q", got)
	}
}
