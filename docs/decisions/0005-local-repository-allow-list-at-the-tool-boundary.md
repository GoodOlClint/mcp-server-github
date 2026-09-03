# ADR 0005 — Local repository allow-list at the tool boundary

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** G3 architecture and security review, 2026-09-03

## Context

The MCP tool takes `repo_path` from the model. The server runs with the operator's filesystem access and an App token, so an unconstrained path lets a prompt-injected model replay any repository on the machine that has a `github.com` remote the App is installed on.

## Decision

The server requires one or more `--repo-root` flags at launch and refuses to start without them. `repo_path` must be absolute, exist, resolve (symlinks followed) to a location inside a root, and open as a git repository without `.git` discovery upward. Linked worktrees are refused with the main working tree named, because go-git does not read a linked worktree's refs without common-dir support and the failure it produced was misleading. Policy violations are `refused`; filesystem errors are `error`.

## Rejected alternatives

- **No allow-list, trust the caller.** The caller is a model; the boundary is the only place the operator's intent is enforced.
- **Allow linked worktrees when the gitdir is inside a root.** Was the first ruling; go-git could not open them correctly, so accepting them produced `remote not found` instead of a push.
- **Per-call approval prompt.** Not available over stdio and defeats the automation.

## Consequences

- The launch config carries an absolute path per allowed tree; `~` is not expanded.
- Agents in linked worktrees pass the main working tree path and their branch name; branch refs live in the shared git dir.
- The destination GitHub repository is still derived from the remote, not constrained; see issue #2 for the `--repo` allow-list.
- Validation opens the repository once and replay opens it again; see issue #3.
