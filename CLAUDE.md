# mcp-server-github

Pushes local git commits to GitHub as Verified commits through `createCommitOnBranch`, reading file content from the local repo so none passes through the model.

## Architecture

Go, go-git for every repo operation (no `os/exec`), `ghinstallation` App auth, `net/http` GraphQL, `modelcontextprotocol/go-sdk` stdio server. Layout: `cmd/mcp-server-github` (flags, launch), `internal/tool` (MCP tool, boundary validation, error kinds, ceiling default), `internal/replay` (go-git walk, refusal set, replay, local ref sync), `internal/github` (App auth, GraphQL client). The Python spike that proved the semantics was deleted on 2026-09-03; its measurements live in ADR 0002. Design and work split: `docs/design.md`.

## Canonical pipelines

- All GitHub writes flow through `internal/replay.Push`; no REST git-data path exists (ADR 0002).
- Local ref sync after a push lives in the same package (ADR 0003).
- Path and repository policy is enforced only in `internal/tool` (ADR 0005); replay trusts what it is handed.
- Error kinds are assigned only in `internal/tool.classify`; replay returns typed errors and never classifies.

## ADRs

Read `docs/decisions/` before architectural changes. 0001 commit model, 0002 refusal policy and measured ceiling, 0003 local sync, 0004 language and phasing, 0005 repository allow-list.

## Build / run / test

- Build: `go build ./cmd/mcp-server-github`
- Test: `go test ./... && go vet ./...`; the module must contain no `os/exec` import.
- Launch config: `README.md`.
