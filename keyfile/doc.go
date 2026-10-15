// Package keyfile verifies bearer tokens against a hashed, per-key key file
// and restricts each key to an explicit list of tools.
//
// Experimental: the API may change in a minor release.
//
// # File format
//
//	version: 1
//	keys:
//	  - name: reporter                  # ^[a-z][a-z0-9-]{1,31}$, unique
//	    sha256: <64 lowercase hex of the raw token>
//	    expires: 2027-01-12T00:00:00Z   # mandatory, RFC 3339, UTC ("Z") only
//	    tools: [tool_a, tool_b]         # must all be registered; "*" is not accepted
//
// The file is decoded strictly: an unknown field, a wrong version, a duplicate
// name, a duplicate hash or a YAML error makes the whole file invalid, and an
// invalid file means deny-all. An individual entry that is invalid (bad or
// reserved name, bad hash, missing, non-UTC or past expiry, empty tool list,
// unregistered tool) is dropped on its own and logged at ERROR. A missing file
// is an empty key set.
//
// # Reload
//
// Each verification stats the file (at most once per second) and re-reads it
// when its inode, mtime or size changed. A failed read or parse keeps the
// previous key set for one 50 ms retry window, then installs deny-all until the
// file next changes. There is deliberately no keep-last-good: a typo in an edit
// that revokes a key must not leave that key alive. Reloads are single-flight
// and never block other requests.
//
// Replace the file atomically (write a temporary file, then rename it over the
// key file).
//
// # Tokens
//
// Tokens look like "<prefix>1_<43 base62><6 base62 CRC-32>" and are produced by
// [Mint]. The checksum lets a malformed token be rejected without a lookup and
// lets secret scanners match tokens precisely. Only SHA-256 digests are stored.
// No token, token prefix or token hash is ever logged.
//
// # Enforcement
//
// [Store.Verifier] snapshots the key's tool list into the returned TokenInfo and
// [Store.ToolFilter] reads only that snapshot, so a reload between verification
// and filtering cannot change the decision. A missing TokenInfo is denied.
//
// The REST bridge (Config.RESTBridge) is not supported with this package: its
// calls run over an in-memory session that carries no TokenInfo, so the filter
// denies every bridged call (fail closed).
package keyfile
