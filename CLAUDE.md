# mcp-server-github

Pushes local git commits to GitHub as Verified commits through `createCommitOnBranch`, reading file content from the local repo so none passes through the model.

## Architecture

Product: Go, go-git for every repo operation (no `os/exec`), `ghinstallation` App auth, `net/http` GraphQL, `modelcontextprotocol/go-sdk` stdio server. `spike/` holds the Python proof-of-semantics and is deleted once the Go DoD passes. Design and work split: `docs/design.md`.

## Canonical pipelines

- All GitHub writes flow through `internal/replay/` (spike: `spike/push_verified.py`); no REST git-data path exists (ADR 0002).
- Local ref sync after a push lives in the same package (ADR 0003).

## ADRs

Read `docs/decisions/` before architectural changes. 0001 commit model, 0002 refusal policy, 0003 local sync, 0004 language and phasing.

## Build / run / test

- Spike: `python3 spike/push_verified.py <repo_path> <branch> [--base main]`; tests `python3 -m pytest spike/`
- Product: `go build ./cmd/mcp-server-github`; tests `go test ./...`
