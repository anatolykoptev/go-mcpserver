//go:build unix

package keyfile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

func openAt(t *testing.T, path string) (*Store, *syncBuf) {
	t.Helper()
	logger, buf := debugLogger()
	s, err := Open(baseOpts(path, logger))
	if err != nil {
		t.Fatal(err)
	}
	s.statTTL = 0
	return s, buf
}

func TestKeyfile_RefusesWritableFile(t *testing.T) {
	tok, sha := mustMint(t)
	for mode, wantOK := range map[os.FileMode]bool{
		0o600: true, 0o640: true, 0o644: true, 0o400: true,
		0o620: false, 0o602: false, 0o660: false, 0o666: false,
	} {
		path := filepath.Join(privateDir(t), "keys.yaml")
		writeAtomic(t, path, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		s, buf := openAt(t, path)
		_, err := verify(s, tok)
		if wantOK && err != nil {
			t.Errorf("mode %#o must be accepted: %v", mode, err)
		}
		if !wantOK {
			if !errors.Is(err, auth.ErrInvalidToken) || s.Status().LastReloadOK {
				t.Errorf("mode %#o must be refused (err=%v status=%+v)", mode, err, s.Status())
			}
			if !strings.Contains(buf.String(), "writable") {
				t.Errorf("mode %#o: refusal category not logged: %s", mode, buf.String())
			}
		}
	}
}

func TestKeyfile_RefusesNonRegularFiles(t *testing.T) {
	dir := privateDir(t)
	asDir := filepath.Join(dir, "adir")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"directory": asDir, "fifo": fifo} {
		done := make(chan *Store, 1)
		bufs := make(chan *syncBuf, 1)
		go func() { s, buf := openAt(t, path); bufs <- buf; done <- s }()
		select {
		case s := <-done:
			if s.Status().LastReloadOK {
				t.Errorf("%s: must be refused", name)
			}
			if buf := <-bufs; !strings.Contains(buf.String(), "not a regular file") {
				t.Errorf("%s: refusal category not logged: %s", name, buf.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Open blocked", name)
		}
	}
}

func TestKeyfile_RefusesSymlink(t *testing.T) {
	tok, sha := mustMint(t)
	dir := privateDir(t)
	real := filepath.Join(dir, "real.yaml")
	writeAtomic(t, real, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	link := filepath.Join(dir, "keys.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s, buf := openAt(t, link)
	if _, err := verify(s, tok); err == nil {
		t.Fatal("a symlinked key file must be refused")
	}
	if !strings.Contains(buf.String(), "symlink") {
		t.Errorf("refusal category not logged: %s", buf.String())
	}
}

func TestKeyfile_ParentDirectoryChecks(t *testing.T) {
	tok, sha := mustMint(t)
	for mode, wantOK := range map[os.FileMode]bool{
		0o700: true, 0o755: true, 0o750: true,
		0o777: false, 0o770: false, 0o702: false,
		0o777 | os.ModeSticky: true, // like /tmp
	} {
		dir := privateDir(t)
		path := filepath.Join(dir, "keys.yaml")
		writeAtomic(t, path, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		s, buf := openAt(t, path)
		_, err := verify(s, tok)
		if wantOK && err != nil {
			t.Errorf("dir mode %v must be accepted: %v", mode, err)
		}
		if !wantOK {
			if err == nil || s.Status().LastReloadOK {
				t.Errorf("dir mode %v must be refused", mode)
			}
			if !strings.Contains(buf.String(), "parent directory") {
				t.Errorf("dir mode %v: category not logged: %s", mode, buf.String())
			}
		}
	}
}

func TestKeyfile_RefusesUntrustedOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns everything")
	}
	tok, sha := mustMint(t)
	path := filepath.Join(privateDir(t), "keys.yaml")
	writeAtomic(t, path, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	s, buf := openAt(t, path)
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("control: own file must be accepted: %v", err)
	}
	// Pretend the process runs as someone else: the file is now owned by
	// neither the process user nor root.
	s.euid = func() int { return os.Geteuid() + 1 }
	writeAtomic(t, path, fileOf(entry("alice", sha, futureExpiry(), "ro_health", "ro_metrics")))
	_, _ = verifyWithin(t, s, tok, 5*time.Second) // first read refused; retry pending
	waitIdle(t, s)
	if _, err := verify(s, tok); err == nil {
		t.Fatal("a file owned by another non-root user must be refused")
	}
	if !strings.Contains(buf.String(), "untrusted owner") {
		t.Errorf("category not logged: %s", buf.String())
	}
}

// A persistent stat error denies all, starts no reload cycles and logs at most
// once a minute; the store recovers when stat works again.
func TestKeyfile_StatErrorBacksOff(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tok, sha := mustMint(t)
	sub := filepath.Join(privateDir(t), "d")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "keys.yaml")
	writeAtomic(t, path, fileOf(entry("alice", sha, futureExpiry(), "ro_health")))
	s, buf := openAt(t, path)
	if _, err := verify(s, tok); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	before := s.reloads.Load()
	for range 10 {
		if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("stat failure must deny all, got %v", err)
		}
	}
	if got := s.reloads.Load() - before; got != 0 {
		t.Errorf("reload cycles during a persistent stat error = %d, want 0", got)
	}
	if n := strings.Count(buf.String(), "cannot stat"); n != 1 {
		t.Errorf("stat-error log lines = %d, want 1 per minute", n)
	}
	if err := os.Chmod(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("store must recover once stat works again: %v", err)
	}
}

// The change signature must carry the inode and ctime, not just mtime and size.
func TestKeyfile_SignatureCapturesInodeAndCtime(t *testing.T) {
	aTok, aSha := mustMint(t)
	bTok, bSha := mustMint(t) // same length hash: identical size
	exp := "2099-01-01T00:00:00Z"
	s, path, _ := openStore(t, fileOf(entry("alice", aSha, exp, "ro_health")))
	before, err := s.statSig()
	if err != nil {
		t.Fatal(err)
	}
	if before.ino == 0 {
		t.Fatalf("signature must carry the inode: %+v", before)
	}
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "netbsd":
		if before.ctimeNS == 0 {
			t.Fatalf("signature must carry the ctime on %s: %+v", runtime.GOOS, before)
		}
	}
	st, _ := os.Stat(path)
	writeAtomic(t, path, fileOf(entry("alice", bSha, exp, "ro_health")))
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil { // same mtime
		t.Fatal(err)
	}
	after, err := s.statSig()
	if err != nil {
		t.Fatal(err)
	}
	if after.size != before.size || after.mtimeNS != before.mtimeNS {
		t.Fatalf("setup: size/mtime should coincide: %+v vs %+v", before, after)
	}
	if after.ino == before.ino {
		t.Errorf("rename-replace must change the inode in the signature")
	}
	if after.ctimeNS == before.ctimeNS {
		t.Errorf("rename-replace must change the ctime in the signature")
	}
	if _, err := verify(s, aTok); err == nil {
		t.Error("old key accepted after a same-size, same-mtime replacement")
	}
	if _, err := verify(s, bTok); err != nil {
		t.Errorf("new key rejected after replacement: %v", err)
	}
}

// A persistent refusal records the real file signature: the file is not
// re-read (and the log not re-written) every stat TTL, but a chmod that fixes
// the file (ctime change, same inode/mtime/size) is picked up at once.
func TestKeyfile_PersistentRefusalIsNotReReadEveryTTL(t *testing.T) {
	tok, sha := mustMint(t)
	path := filepath.Join(privateDir(t), "keys.yaml")
	writeAtomic(t, path, fileOf(entry("alice", sha, "2099-01-01T00:00:00Z", "ro_health")))
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	logger, buf := debugLogger()
	s, err := Open(baseOpts(path, logger))
	if err != nil {
		t.Fatal(err)
	}
	s.sleep = func(time.Duration) {}
	clock := time.Now()
	s.now = func() time.Time { return clock }
	reads := 0
	base := s.readFile
	s.readFile = func(p string) ([]byte, error) { reads++; return base(p) }

	before := s.reloads.Load()
	for range 5 {
		clock = clock.Add(2 * defaultStatTTL)
		if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("writable file must deny all, got %v", err)
		}
		waitIdle(t, s)
	}
	if got := s.reloads.Load() - before; got != 0 {
		t.Errorf("reload cycles over 5 TTLs of a persistent refusal = %d, want 0", got)
	}
	if reads != 0 {
		t.Errorf("file re-read %d times over 5 TTLs of a persistent refusal, want 0", reads)
	}
	if n := strings.Count(buf.String(), "retrying once"); n > 1 {
		t.Errorf("\"retrying once\" logged %d times, want at most 1", n)
	}

	if err := os.Chmod(path, 0o600); err != nil { // ctime-only change
		t.Fatal(err)
	}
	clock = clock.Add(2 * defaultStatTTL)
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("chmod fix must be picked up: %v", err)
	}
}

// A group-writable parent is not visible in the file's own signature, so that
// refusal stays re-checkable: fixing the directory must be picked up.
func TestKeyfile_ParentRefusalRecoversAfterDirectoryFix(t *testing.T) {
	tok, sha := mustMint(t)
	dir := privateDir(t)
	path := filepath.Join(dir, "keys.yaml")
	writeAtomic(t, path, fileOf(entry("alice", sha, "2099-01-01T00:00:00Z", "ro_health")))
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	s, _ := openAt(t, path)
	s.sleep = func(time.Duration) {}
	if _, err := verify(s, tok); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("group-writable parent must deny all, got %v", err)
	}
	waitIdle(t, s)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(s, tok); err != nil {
		t.Fatalf("fixing the parent directory must be picked up: %v", err)
	}
}
