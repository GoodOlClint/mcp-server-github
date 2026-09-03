# ADR 0003 — Reset the local branch to the remote OIDs after a replay

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** `~/.claude/kickoffs/psproxmoxve-remediation/mcp-server-github-handoff-review.md`; operator interview 2026-09-03

## Context

After a replay the remote branch holds commits with the same trees as the local commits but different OIDs. A later local `git push` over them is rejected as non-fast-forward, and a second replay would re-send commits already on the remote unless the tool knows which local commits map to which remote ones.

## Decision

After a successful replay the tool runs `git fetch origin <branch>` and moves the local branch ref to the fetched remote OID. Because the trees are identical the working tree and index are unchanged. From then on `origin/<branch>..<branch>` is empty and a further replay sends only new commits. The tool verifies `git diff <old-local-tip> origin/<branch>` is empty before moving the ref; a non-empty diff is a hard error and the local ref is left alone.

## Rejected alternatives

- **Keep local OIDs and track a local-to-remote map under `.git/`.** More state, and every other git command on the machine still sees a diverged branch.
- **Do nothing.** Every subsequent push replays everything again.

## Consequences

- The tool rewrites local history the agent just created. Reflog keeps the old tips.
- The local branch must be the checked-out branch or the tool uses `git update-ref` without touching the worktree; the spike handles the checked-out case with `git reset --soft`.
- Byte-verify from the agent contract becomes redundant for pushes made by this tool.
