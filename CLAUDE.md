# CLAUDE.md

Notes for coding agents working in go-mcpserver: a Go library (module
`github.com/anatolykoptev/go-mcpserver`, Go 1.26) that boots an MCP server over the official
go-sdk: HTTP (`/mcp`, stateless StreamableHTTP) or stdio, with health routes, middleware, auth and
a small client. Many services import it, so the exported API and default behaviour are contracts.
Every document is indexed in [docs/INDEX.md](docs/INDEX.md).

## Layout

One root package `mcpserver`: `config.go` (`Config` and defaults), `mcpserver.go` (`Run`, `Build`,
`Serve`), `server.go` (`NewServer`), `auth.go`, `middleware.go`, `health.go`, `rest.go`, `hooks.go`,
`lenient.go`, `testing.go`. `mcpclient/` is the reusable client. Tests sit beside the code.
`ROADMAP.md` lists what has shipped per version.

## Gate

CI (`.github/workflows/ci.yml`) runs two jobs on `ubuntu-latest`:

```sh
go test -v -race -count=1 ./...
golangci-lint run          # golangci-lint v2.12.2
```

`make test` and `make lint` run the same. A docs-only PR (Markdown, `docs/`, licence, templates)
skips both through the `docs-scope` job in the same workflow; that block is canonical across the
fleet and compared byte for byte, so do not edit it here. Versions come from release-please
(Conventional Commits); never tag or edit `CHANGELOG.md` by hand.

## Invariants that are easy to break

- **Auth wraps `/mcp` only.** `/health`, `/metrics` and metadata routes stay unauthenticated by design.
- **A bypassed request never gets an anonymous identity.** With `LoopbackBypass`, `LoopbackIdentity`
  must return a real `*TokenInfo`; a nil result is rejected, and a `ToolFilter` without
  `LoopbackIdentity` logs a startup warning. The extractor may trust only edge-set attributes.
- **Stateless is the default** and issues no session IDs. `Stateless=false` needs a `SessionTimeout`
  or sessions never expire.
- **`KeepAlive`, `SchemaCache` and `SupportedProtocolVersions` apply only through `NewServer`**; `Run`
  and `Build` warn when they are set. `MaxRequestBodyBytes` applies in `Run`, `Build` and `Serve`
  (default 16 MiB).
- **`mcpclient` separates `ErrRejected` from `ErrUnreachable`**: a 413 or a JSON-RPC error must never
  be hidden by `WithUnreachableTolerant(true)`.
- **Default CORS is off** (`CORSOrigins` nil); `DisableLocalhostProtection` is for use behind a
  trusted reverse proxy only.
- **This repository is public.** No hostnames, operator paths, account names, tokens or personal
  data in any tracked file, tests and fixtures included.
