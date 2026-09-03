# ADR 0002 — Refuse rather than fall back when createCommitOnBranch cannot represent a commit

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** `~/.claude/kickoffs/psproxmoxve-remediation/mcp-server-github-handoff-review.md`; operator interview 2026-09-03

## Context

`CreateCommitOnBranchInput.fileChanges.additions` carries only `path` and base64 `contents`. There is no file mode, so an executable bit, a symlink, or a submodule pointer cannot be expressed. The mutation also has a request size ceiling. The REST git data API can express modes and bigger trees but its commits are unsigned.

## Decision

When a commit in the range contains a mode change, a non-regular-file entry, or exceeds the payload ceiling, the tool fails before any mutation is sent, naming the commit, the path, and the reason. It never sends a partial range. The agent contract's existing exception (local `git push` for such PRs) remains the escape hatch.

## Rejected alternatives

- **REST blob/tree/commit fallback flagged as unverified.** Two code paths and a branch with mixed Verified and unsigned commits, which defeats a future require-signed-commits rule anyway.
- **Silently drop the mode change.** This is what `push_files` does today and it already bit the remediation effort.

## Consequences

- Shell-script PRs and anything touching modes stay on local push until GitHub adds a mode field to the mutation.
- The payload ceiling is measured against the target repo's largest file before the spike is called done, and the number is recorded here.
