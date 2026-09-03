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
- Deletions carry no content, so a deletion is representable whatever the deleted file's mode was. Measured 2026-09-03 on `GoodOlClint/PSProxmoxVE`: `createCommitOnBranch` keeps the existing mode of a modified entry (a `100755` script edited through the mutation stays `100755`, Verified), and a new entry always lands as `100644`; the REST contents endpoint behaves the same and also has no mode parameter. The refusal set is therefore: any mode change, any new file that is executable locally, symlinks, submodules. Modifying an existing executable is allowed; deleting one is allowed. Submodule entries (mode `160000`) are refused in every position, including deletion, because the mutation's effect on a gitlink is unmeasured.
- Measured ceiling, P3 spike, 2026-09-03, pushing generated random-byte files to `GoodOlClint/PSProxmoxVE` from the operator's workstation (raw addition bytes; the request carries about 1.34x that after base64):

  | Payload | Attempts | Result |
  |---|---|---|
  | 1 MB | 1 | ok, 6 s |
  | 5 MB | 4 | ok, 6 to 9 s |
  | 8 MB | 3 | ok, 8 to 11 s |
  | 10 MB | 4 | 2 ok, 2 failed with `HTTP 499` |
  | 15 MB | 2 | both failed with `HTTP 499` |
  | 20 MB | 2 | 1 ok (31 s), 1 failed with `HTTP 499` |
  | 40 MB | 1 | client-side `urlopen error The write operation timed out` at the 30 s request timeout |
  | 200 files, ~15 B each, one commit | 1 | ok, 5 s |

  Exact text of the first failure: `github error: GraphQL request failed: HTTP 499: ` (empty response body; 499 is the edge closing the connection).

- `MAX_COMMIT_BYTES` is `7_200_000`, the largest size that succeeded on every attempt (8 MB) minus 10%. The failure is time-correlated rather than a hard byte limit, so the number encodes the operator's upstream bandwidth as much as GitHub's limit; a faster link would push it higher, and the Go port should re-measure rather than inherit it.
- The count of files in a commit is not a constraint at the scale that matters here: 200 additions in one mutation succeeded in 5 s.

- Go re-measure, G3, 2026-09-03, same repo and workstation, driven through the stdio MCP server with a 120 s request timeout and the ceiling raised out of the way. Sizes are MiB of random bytes in a single added file:

  | Payload | Attempts | Ok | Result |
  |---|---|---|---|
  | 3 MiB | 2 | 2 | ok, 4 s |
  | 4 MiB | 2 | 2 | ok, 6 to 9 s |
  | 5 MiB | 6 | 6 | ok, 6 to 14 s |
  | 6 MiB | 6 | 4 | 2 failed with `HTTP 499` |
  | 7 MiB | 6 | 5 | 1 failed with `HTTP 499` |
  | 8 MiB | 6 | 4 | 2 failed with `HTTP 499` |
  | 10 MiB | 6 | 4 | 2 failed with `HTTP 499` |
  | 15 MiB | 2 | 1 | 1 failed with `HTTP 499` |
  | 20 MiB | 2 | 0 | both failed with `HTTP 499` |
  | 30 MiB | 2 | 0 | both failed with `HTTP 499` |

- Every Go failure was `github: graphql request failed: HTTP 499: ` with an empty body, and every one of them landed between 5.9 s and 6.3 s of wall clock regardless of payload size, while the successes ran 4 s to 21 s. That is an edge cutting the connection on a roughly fixed deadline, not a byte limit: the 120 s client timeout is never reached, and raising it cannot help. Size only sets the probability of crossing the deadline.
- `tool.DefaultMaxCommitBytes` is `4_718_592`, 5 MiB (the largest size that succeeded on every Go attempt) minus 10%. It is lower than the spike's `7_200_000` because the uplink was slower on the re-measure day, which is the same conclusion the spike reached: this number tracks the operator's bandwidth, not a GitHub limit, so it should be re-measured rather than inherited. It lives in `internal/tool` and reaches the engine through `replay.Options.MaxCommitBytes`.
