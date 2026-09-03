# ADR 0006 — Git Data API for every push, replacing createCommitOnBranch

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** live probes on `GoodOlClint/PSProxmoxVE`, 2026-09-03; operator decision the same day

## Context

ADR 0002 rejected a REST fallback because the docs and the gh-aw analysis said Git Data API commits are unsigned. Measured 2026-09-03: `POST /git/commits` made with the App installation token and no `author` or `committer` field returns a commit GitHub signs itself (Verified, reason valid, committer GitHub). A `POST /git/trees` entry carries the real mode, so a `100755` script and a `120000` symlink both landed correctly. Supplying `author`, `committer`, or a date makes the commit unsigned, so identity and timestamps stay out of reach.

## Decision

Every push uses the Git Data API and nothing else: one `POST /git/blobs` per unique blob in the range (deduplicated by hash across commits), then per commit one `POST /git/trees` with `base_tree` = the remote parent's tree and one entry per changed path carrying its local mode (`sha: null` for deletions), one `POST /git/commits` with message, tree, and remote parent and no author or committer fields, and finally one non-force `PATCH /git/refs/heads/<branch>` to the last commit, whose fast-forward check replaces `expectedHeadOid`. The refusal set shrinks to merge commits and empty commits. `createCommitOnBranch`, the GraphQL client, and the mode refusals are deleted. Blob SHAs are content-addressed, so each upload is verified by comparing the returned SHA with the local one.

## Rejected alternatives

- **Keep `createCommitOnBranch` and use Git Data only for commits it cannot represent.** Two code paths for one behaviour; the fallback would carry the whole feature anyway.
- **Keep refusing modes.** Every chmod and new script needed an operator push; the reason for refusing (no signed path) turned out to be false.
- **Packfile upload.** No signed endpoint accepts one; a packfile carries finished commit objects.

## Consequences

- Calls per push are unique blobs plus two per commit plus one, against a 5000 per hour limit; trivial at remediation sizes.
- The per-request ceiling from ADR 0002 no longer applies; the limit is 100 MB per blob. `MaxCommitBytes` stays as a sanity cap and is re-measured.
- The remote OIDs still differ from local (GitHub is the committer), so ADR 0003 stands.
- ADR 0002's refusal policy is superseded; its deletion carve-out and mode measurements are history. ADR 0001's commit model stands with the transport half replaced.
- The test fake models blobs, trees, commits, and a fast-forward ref update instead of the mutation.
- Intermediate commits exist on the remote before the final ref update reaches them; a failure mid-push leaves no ref moved and GitHub garbage-collects the orphans.
