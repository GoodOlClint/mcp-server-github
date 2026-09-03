# ADR 0001 — Local commit, replay to GitHub on push via createCommitOnBranch

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** `~/.claude/kickoffs/psproxmoxve-remediation/mcp-server-github-handoff-review.md`; operator interview 2026-09-03

## Context

Remediation agents push through the official server's `push_files`, which needs every changed file's full content as a tool-call parameter. The commits it makes are Verified because GitHub signs API-created commits. Local `git push` needs no content in the model but yields unsigned commits, and PSProxmoxVE may later require Verified commits. The Sonnet handoff proposed making `git_commit` itself the API call on a session-scoped staging branch, with a durable session file, fast-forward-or-replay on push, and a TTL reaper for orphans.

## Decision

Agents commit locally with ordinary git. The tool pushes by walking `origin/<base>..<branch>` and issuing one `createCommitOnBranch` mutation per local commit onto the remote branch, creating the branch from the base OID if it does not exist. The tool reads file contents from the local repository itself, so no content passes through the model. The remote branch must be at the expected head before each mutation (`expectedHeadOid`); a mismatch is an error, not a merge.

## Rejected alternatives

- **Commit is the API call, staging branch per session (the handoff).** Needs durable session state under `.git/`, a reaper for crashed sessions, and divergence handling on push. Every agent already owns a private branch in a private worktree under the one-actor rule, so the divergence case the machinery defends against is already excluded by process.
- **Local push over HTTPS with the App installation token.** Zero code and keeps SHAs and file modes, but the commits are unsigned. Rejected only because the Verified badge is the goal; it remains the fallback for cases ADR 0002 refuses.
- **Fabricated merge commits on divergence.** `createCommitOnBranch` is single-parent; not representable.

## Consequences

- Transport moved from `createCommitOnBranch` to the Git Data API in [ADR 0006](0006-git-data-api-for-every-push-replacing-createcommitonbranch.md); the commit model here is unchanged.

- One network call per commit; the tool must be idempotent on retry by re-reading the remote head.
- Remote OIDs differ from local OIDs (ADR 0003 handles the local side).
- No custom author, committer, or timestamp on the replayed commits; the App is the author. The original local timestamp is not preserved. The Verified badge therefore attests that the App made the commit, not who wrote the local commit; every commit in the range is re-authored, including ones a human made locally.
- Empty commits and merge commits in the range are errors.
