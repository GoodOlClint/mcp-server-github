# ADR 0006 — Git Data API for every push, replacing createCommitOnBranch

- **Status:** Accepted
- **Date:** 2026-09-03
- **Deciders:** operator + agent
- **Context source:** live probes on `GoodOlClint/PSProxmoxVE`, 2026-09-03; operator decision the same day

## Context

ADR 0002 rejected a REST fallback because the docs and the gh-aw analysis said Git Data API commits are unsigned. Measured 2026-09-03: `POST /git/commits` made with the App installation token and no `author` or `committer` field returns a commit GitHub signs itself (Verified, reason valid, committer GitHub). A `POST /git/trees` entry carries the real mode, so a `100755` script and a `120000` symlink both landed correctly. Supplying `author`, `committer`, or a date makes the commit unsigned, so identity and timestamps stay out of reach.

## Decision

Every push uses the Git Data API and nothing else: one `POST /git/blobs` per unique blob in the range (deduplicated by hash across commits), then per commit one `POST /git/trees` with `base_tree` = the remote parent's tree and one entry per changed path carrying its local mode (`sha: null` for deletions), one `POST /git/commits` with message, tree, and remote parent and no author or committer fields, and finally one non-force `PATCH /git/refs/heads/<branch>` to the last commit, whose fast-forward check replaces `expectedHeadOid`. `POST /git/commits` takes any number of parents, and measured 2026-09-03 a two-parent merge commit and a zero-parent root commit both come back Verified, so merge commits are replayed with every parent mapped to its remote OID (a parent outside the range must already exist on the remote) and a branch with no remote base (first push to an empty repository) is replayed from its root. The refusal set shrinks to empty commits, a parent that exists neither on the remote nor in the range, and blobs over 100 MB. `createCommitOnBranch`, the GraphQL client, and the mode refusals are deleted. Blob SHAs are content-addressed, so each upload is verified by comparing the returned SHA with the local one.

## Rejected alternatives

- **Keep `createCommitOnBranch` and use Git Data only for commits it cannot represent.** Two code paths for one behaviour; the fallback would carry the whole feature anyway. Held in reserve, not rejected outright: if the G6 timing measurement is materially worse than the GraphQL baseline (4.8 s for two commits over three files), the GraphQL path comes back as primary and Git Data handles only what it would have refused (mode changes, new executables, symlinks, submodules). Operator ruling 2026-09-03.
- **Keep refusing modes.** Every chmod and new script needed an operator push; the reason for refusing (no signed path) turned out to be false.
- **Packfile upload.** No signed endpoint accepts one; a packfile carries finished commit objects.

## Consequences

- Calls per push are unique blobs plus two per commit plus one, against a 5000 per hour limit; trivial at remediation sizes.
- The per-request ceiling from ADR 0002 no longer applies. Measured on `GoodOlClint/PSProxmoxVE` 2026-09-03 (G6, macOS 15.6 / darwin 25.6.0, Go 1.27, residential uplink), the binding limit is per blob and is not the documented 100 MB: `POST /git/blobs` rejects a request whose base64 `content` exceeds 32 MiB with `HTTP 401 Bad credentials`, which is a misleading status for a payload limit. Reproduced with plain `urllib` as well as through the tool, so it is GitHub's, not the client's.

| Blob content | base64 request body | Result |
|---|---|---|
| 10 MiB | 14.0 MB | success, 41.0 s / 31.1 s |
| 20 MiB | 28.0 MB | success, 63.2 s / 206.1 s |
| 24 MiB | 33.55 MB (32 MiB exactly) | success, 38.3 s |
| 25 MB | 34.95 MB | HTTP 401, 76 s |
| 30 MiB | 41.9 MB | HTTP 401, 80 s |
| 35 MiB | 48.9 MB | HTTP 401, 107 s / 118 s |
| 40 MiB | 55.9 MB | HTTP 401, 129 s |
| 50 MiB | 69.9 MB | HTTP 401, 149 s / 168 s / 185 s |

So the per-blob refusal is set to 24 MiB, and a blob over it is refused before anything is sent rather than failing mid-push as a bogus 401. The limit does not bound a commit: a 40 MB commit carried as two 20 MiB blobs succeeded in 60.6 s, so `MaxCommitBytes` stays a per-commit sanity cap and its default is 50 MB.

Push wall times against the 4.8 s GraphQL baseline for the same two-commit, three-file shape: 5.5 s for add + modify + delete over two commits; 9.4 s for three commits covering a chmod, a new 755 script, a symlink and a directory replaced by a file; 0.6 s for a re-run with nothing to push. The extra second over GraphQL buys the modes, and the cost is round trips rather than bytes.
- The remote OIDs still differ from local (GitHub is the committer), so ADR 0003 stands.
- ADR 0002's refusal policy is superseded; its deletion carve-out and mode measurements are history. ADR 0001's commit model stands with the transport half replaced.
- The test fake models blobs, trees, commits, and a fast-forward ref update instead of the mutation.
- Intermediate commits exist on the remote before the final ref update reaches them; a failure mid-push leaves no ref moved and GitHub garbage-collects the orphans. A branch the remote does not have is therefore created directly at the replayed tip rather than at the merge base: creating it at the merge base first would publish an empty branch that survives a later failure, which is the outcome `docs/design.md` set out to avoid. `POST /git/refs` returning 422 `Reference already exists` is the new-branch form of the head race and maps to the same typed error as `Update is not a fast forward`.
- A merge is replayed by mapping each local parent to its remote OID: a parent inside the range is the commit just built for it, a parent outside the range keeps its own OID and is confirmed present with `GET /git/commits/<sha>`; a parent that is neither is a refusal naming it. The tree is built from the remote first parent's tree, and a root commit is created with `parents: []` and no `base_tree` at all. Verified live 2026-09-03: a `git merge --no-ff` replayed with both parents intact and all three commits Verified.
- Every tree GitHub builds is compared with the tree the local commit names before the next call. `base_tree` is the local parent's tree and the entries carry local blob OIDs, so the result is determined; checking it turns any divergence into a refusal with nothing published, instead of a post-push tree mismatch on a branch that already moved. Verified live against a commit that replaces a directory with a file of the same name.
