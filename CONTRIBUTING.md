# Contributing to go-mcpserver

Changes are welcome when they keep the exported API and default behaviour stable and are covered
by tests.

## Development

Requires Go 1.26 and golangci-lint v2.12.2.

```sh
make test    # go test -v -race -count=1 ./...
make lint    # golangci-lint run ./...
```

CI (`.github/workflows/ci.yml`) runs exactly these two. If they pass locally they pass there. A
pull request that changes only documents skips them.

## Commits

Conventional Commits (`feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `deps:`, `chore:`, `ci:`,
`test:`). release-please reads them to cut versions and write [CHANGELOG.md](CHANGELOG.md); do not
tag or edit the changelog by hand. The library is pre-1.0, so a breaking change is marked `feat!:`
or a `BREAKING CHANGE:` footer and raises the minor version.

## Pull requests

- Branch from `master`, keep the change focused, and fill in the pull-request template.
- Add tests for behaviour changes, including the unauthenticated and rejected paths for anything
  that touches auth.
- Update [docs/INDEX.md](docs/INDEX.md) when you add or rename a document.
- Do not add secrets, hostnames or personal data to any file; the repository is public.

## Reporting problems

Bugs and feature requests go through the issue templates. Report vulnerabilities privately as
described in [SECURITY.md](SECURITY.md).
