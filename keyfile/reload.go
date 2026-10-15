package keyfile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// fileSig identifies a file version. It is read before the content, so a write
// that lands mid-read changes the signature and triggers another reload.
type fileSig struct {
	ino     uint64
	mtimeNS int64
	size    int64
	missing bool
}

type snapshot struct {
	keys    map[[digestLen]byte]*keyEntry
	sig     fileSig
	fileSHA string
	ok      bool // false = deny-all after a failed load
}

func denyAll(sig fileSig, fileSHA string, ok bool) *snapshot {
	return &snapshot{keys: map[[digestLen]byte]*keyEntry{}, sig: sig, fileSHA: fileSHA, ok: ok}
}

// statDue applies the stat cache: at most one stat per statTTL.
func (s *Store) statDue() bool {
	if s.statTTL <= 0 {
		return true
	}
	now := time.Now().UnixNano()
	last := s.lastStatNS.Load()
	return now-last >= int64(s.statTTL) && s.lastStatNS.CompareAndSwap(last, now)
}

func (s *Store) statSig() (fileSig, error) {
	fi, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileSig{missing: true}, nil
	}
	if err != nil {
		return fileSig{}, err
	}
	return fileSig{ino: inode(fi), mtimeNS: fi.ModTime().UnixNano(), size: fi.Size()}, nil
}

// maybeReload re-reads the file when its signature changed. It is single-flight
// and never waits for another reload: while one is in progress (including its
// retry window) other requests keep using the current snapshot. The first read
// happens inline (it is a small file); only the retry runs in the background.
func (s *Store) maybeReload() {
	if !s.statDue() {
		return
	}
	sig, err := s.statSig()
	if err == nil && sig == s.snap.Load().sig {
		return
	}
	if !s.reloading.CompareAndSwap(false, true) {
		return
	}
	s.reloads.Add(1)
	snap, err := s.attempt()
	if err == nil {
		s.finish(snap, nil)
		s.reloading.Store(false)
		return
	}
	s.logger.Warn("keyfile: key file unusable, retrying once",
		slog.String("problem", err.Error()), slog.Duration("retry_in", retryDelay))
	go func() {
		defer s.reloading.Store(false)
		time.Sleep(retryDelay)
		s.finish(s.attempt())
	}()
}

// reloadBlocking is the Open-time variant: first read, one retry, then install.
// The caller holds s.reloading.
func (s *Store) reloadBlocking() {
	defer s.reloading.Store(false)
	snap, err := s.attempt()
	if err != nil {
		s.logger.Warn("keyfile: key file unusable, retrying once",
			slog.String("problem", err.Error()), slog.Duration("retry_in", retryDelay))
		time.Sleep(retryDelay)
		snap, err = s.attempt()
	}
	s.finish(snap, err)
}

// finish installs the result of a load: the new key set, or deny-all.
func (s *Store) finish(snap *snapshot, err error) {
	if err != nil {
		s.logger.Error("keyfile: key file invalid, denying all keys",
			slog.String("problem", err.Error()))
		s.snap.Store(denyAll(snap.sig, snap.fileSHA, false))
		return
	}
	s.snap.Store(snap)
	s.logger.Info("keyfile: key file loaded", slog.Int("keys", len(snap.keys)))
}

// attempt stats, reads and parses the file once. On failure the returned
// snapshot carries the signature and hash of what was seen, so the failed
// version is not re-read until the file changes again.
func (s *Store) attempt() (*snapshot, error) {
	sig, err := s.statSig()
	if err != nil {
		return denyAll(fileSig{}, "", false), &loadError{"stat failed"}
	}
	if sig.missing {
		return denyAll(sig, "", true), nil
	}
	data, err := s.readFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return denyAll(fileSig{missing: true}, "", true), nil
	}
	if err != nil {
		return denyAll(sig, "", false), &loadError{"read failed"}
	}
	sum := sha256.Sum256(data)
	fileSHA := hex.EncodeToString(sum[:])
	entries, err := s.parse(data)
	if err != nil {
		return denyAll(sig, fileSHA, false), err
	}
	keys := make(map[[digestLen]byte]*keyEntry, len(entries))
	for _, e := range entries {
		keys[e.digest] = e
	}
	return &snapshot{keys: keys, sig: sig, fileSHA: fileSHA, ok: true}, nil
}
