package keyfile

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	fileVersion = 1
	maxFileSize = 1 << 20
	digestLen   = 32

	maxAttrs     = 32
	maxAttrValue = 256
)

var (
	nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	hexRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lineRE = regexp.MustCompile(`line (\d+)`)

	attrKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

type rawFile struct {
	Version int      `yaml:"version"`
	Keys    []rawKey `yaml:"keys"`
}

type rawKey struct {
	Name    string            `yaml:"name"`
	SHA256  string            `yaml:"sha256"`
	Expires string            `yaml:"expires"`
	Tools   []string          `yaml:"tools"`
	Attrs   map[string]string `yaml:"attrs"`
}

// keyEntry is one validated key. tools is never mutated after load.
type keyEntry struct {
	name    string
	digest  [digestLen]byte
	expires time.Time
	tools   []string
	attrs   map[string]string
}

// loadError describes why a file could not be used. Its text is a fixed
// category (plus a YAML line number): parser messages can quote file content,
// which may include hashes, so they are never logged.
type loadError struct{ kind string }

func (e *loadError) Error() string { return e.kind }

// parse strictly decodes data. A structural problem returns an error (the whole
// file is invalid); an invalid individual entry is logged and dropped.
func (s *Store) parse(data []byte) ([]*keyEntry, error) {
	if len(data) > maxFileSize {
		return nil, &loadError{"file too large"}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f rawFile
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &loadError{"empty file"}
		}
		kind := "yaml error"
		if m := lineRE.FindStringSubmatch(err.Error()); m != nil {
			kind += " near line " + m[1]
		}
		return nil, &loadError{kind}
	}
	var extra rawFile
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &loadError{"multiple yaml documents"}
	}
	if f.Version != fileVersion {
		return nil, &loadError{"unsupported version"}
	}
	if err := checkDuplicates(f.Keys); err != nil {
		return nil, err
	}
	if err := checkAttrs(f.Keys); err != nil {
		return nil, err
	}
	var out []*keyEntry
	for i, k := range f.Keys {
		e, reason := s.validate(k)
		if reason != "" {
			name := ""
			if nameRE.MatchString(k.Name) {
				name = k.Name
			}
			s.logger.Error("keyfile: entry dropped", slog.Int("index", i),
				slog.String("name", name), slog.String("reason", reason))
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// checkDuplicates makes a repeated name or hash invalidate the whole file: two
// entries claiming one identity cannot be resolved safely.
func checkDuplicates(keys []rawKey) error {
	names := make(map[string]struct{}, len(keys))
	hashes := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if _, dup := names[k.Name]; dup {
			return &loadError{"duplicate name"}
		}
		names[k.Name] = struct{}{}
		if k.SHA256 == "" {
			continue
		}
		if _, dup := hashes[k.SHA256]; dup {
			return &loadError{"duplicate hash"}
		}
		hashes[k.SHA256] = struct{}{}
	}
	return nil
}

// checkAttrs enforces the attrs shape. Keys and values are consumer data the
// library never interprets, but a malformed one invalidates the whole file.
func checkAttrs(keys []rawKey) error {
	for _, k := range keys {
		if len(k.Attrs) > maxAttrs {
			return &loadError{"too many attrs"}
		}
		for key, val := range k.Attrs {
			if !attrKeyRE.MatchString(key) {
				return &loadError{"invalid attr key"}
			}
			if len(val) > maxAttrValue {
				return &loadError{"attr value too long"}
			}
		}
	}
	return nil
}

// validate returns the entry, or a non-empty drop reason.
func (s *Store) validate(k rawKey) (*keyEntry, string) {
	if !nameRE.MatchString(k.Name) {
		return nil, "invalid name"
	}
	if s.isReserved(k.Name) {
		return nil, "reserved name"
	}
	if !hexRE.MatchString(k.SHA256) {
		return nil, "invalid sha256"
	}
	expires, ok := parseExpiry(k.Expires)
	if !ok {
		return nil, "expires missing, not RFC 3339, or not UTC"
	}
	if !s.now().Before(expires) {
		return nil, "expired"
	}
	if len(k.Tools) == 0 {
		return nil, "no tools"
	}
	for _, t := range k.Tools {
		if t == "*" {
			return nil, "wildcard tool not allowed"
		}
		if _, known := s.tools[t]; !known {
			return nil, "tool not registered"
		}
	}
	attrs := maps.Clone(k.Attrs)
	if attrs == nil {
		attrs = map[string]string{}
	}
	if s.validator != nil {
		if err := s.validator(k.Name, maps.Clone(attrs)); err != nil {
			return nil, fmt.Sprintf("rejected by consumer validator: %v", err)
		}
	}
	e := &keyEntry{name: k.Name, expires: expires, tools: dedupe(k.Tools), attrs: attrs}
	raw, err := hex.DecodeString(k.SHA256)
	if err != nil || len(raw) != digestLen {
		return nil, "invalid sha256"
	}
	copy(e.digest[:], raw)
	return e, ""
}

func (s *Store) isReserved(name string) bool {
	if slices.Contains(reservedExact, name) {
		return true
	}
	for _, p := range s.reserved {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseExpiry accepts only RFC 3339 timestamps written in UTC with a literal "Z".
func parseExpiry(v string) (time.Time, bool) {
	if !strings.HasSuffix(v, "Z") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}
