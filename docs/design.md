# Design: push_verified

Status: Proposed, 2026-09-03. Approval of this document and ADRs 0001 to 0004 is the build gate.

## Problem

Remediation agents on PSProxmoxVE push through `push_files`, which needs full file content in the model, mints its own SHAs, and drops file modes. Local push avoids all three but produces unsigned commits. The operator wants Verified commits so a require-signed-commits rule can be turned on later, without file content passing through the model.

## Shape

One tool, `push_verified(repo_path, branch, base="main", remote="origin")`, returning the local-to-remote OID pairs and the final remote head. Agents commit locally as today. The tool replays `origin/<base>..<branch>` onto GitHub one `createCommitOnBranch` per commit, then resets the local branch to the remote OIDs. See ADR 0001, 0003.

Phase 1 is `push_verified.py`, a Python CLI spike that proves the semantics and is then discarded. Phase 2 is the product: a Go stdio MCP server using go-git for every repository operation, no subprocess. See ADR 0004.

## Flow

1. Resolve `owner/repo` from the remote URL. Refuse remotes that are not `github.com`.
2. `git fetch origin <base> <branch>`. Compute `range = origin/<branch>..<branch>` if the remote branch exists, else `origin/<base>..<branch>`.
3. For each commit in the range, oldest first: `git diff-tree -r --no-commit-id -M0 <parent> <commit>` gives paths, statuses, and modes. Any mode other than `100644`, any type change, any merge or empty commit: refuse (ADR 0002). Read blob contents via `git cat-file blob <oid>`, never from the working tree.
4. Mint an installation token (cached until 5 minutes before expiry).
5. If the remote branch does not exist, `createRef` from the merge base of `<branch>` and `origin/<base>`, not the base tip.
6. For each commit: `createCommitOnBranch` with `expectedHeadOid` = current remote head, additions and deletions from step 3, message = local subject and body. Record the returned OID as the new head.
7. `git fetch origin <branch>`; assert `git diff <local-tip> origin/<branch>` is empty; move the local ref (ADR 0003).
8. Return the list of local to remote OID pairs and the final remote head.

## Failure modes

| Case | Behaviour |
|---|---|
| Remote head moved between fetch and mutation | `expectedHeadOid` mismatch; stop, report the commits already replayed, do not retry automatically |
| Network failure mid-range | Stop, report the pairs already replayed. A re-run resumes: when the remote branch has commits the local branch lacks, compare them oldest-first against the local range by tree OID and commit message; if every remote-only commit matches a local one in order, adopt them (reset those local commits to the remote OIDs) and continue from the first unmatched local commit. Any mismatch is a refusal ("local branch is behind"). |
| Resulting mode not 100644, symlink, submodule (any entry with mode 160000, including its deletion), oversize | Refuse before the first mutation; the whole range is walked and every blob read before the first mutation is sent |
| Local branch behind remote | Refuse; the agent has commits it did not push |
| Diff after replay non-empty | Hard error, local ref untouched |

## Tool boundary (rulings after G1/G2, 2026-09-03)

- Error classification lives in `internal/tool`: `refused` (fix the commit or arguments), `retryable` (a head race, nothing landed), `partial` (any failure after N>0 commits landed; a re-run resumes), `error` (anything else with nothing landed). Replay's typed errors stay unclassified; no string matching on messages. Revised 2026-09-03 after G3 review: `partial` is decided by the pair count, not the error type.
- `repo_path` is validated at the tool boundary per ADR 0005: absolute, existing, symlinks resolved, inside a `--repo-root` allow-list that the server requires at launch; linked worktrees refused with the main tree named.
- The commit byte ceiling default is owned by `internal/tool` and set from the Go re-measure; replay only enforces the number it is given.
- Auth is an installation token minted immediately before each push and handed to go-git as basic auth; no supplier abstraction.

## Auth

`GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY_PATH`, identical to the official server. No PAT path. The App needs Contents read and write, which it already has.

## Coexistence with github-mcp-server

Add `--exclude-tools create_or_update_file,push_files` to the official server's launch once this tool is in the agent contract. No allow-list maintenance.

## Hosted github-mcp-server

Not an option for App auth. The App-auth mode is documented as stdio-only, and the hosted server needs a bearer token the client supplies. Claude Code cannot refresh an hourly installation token in a static header, so bridging would need a local token-refreshing proxy, which is more moving parts than the stdio server it replaces.

## Definition of done (both phases)

- On a throwaway branch of PSProxmoxVE with two local commits touching three files: the tool pushes, GitHub shows both commits as Verified authored by `goodolclint-claude[bot]`, `git diff` against `origin/<branch>` is empty, `git log origin/<branch>..<branch>` is empty.
- A branch with a `*.sh` mode change is refused with the path named and nothing is pushed.
- Re-running after a successful push sends zero mutations.
- Payload ceiling measured and recorded in ADR 0002.
- Offline tests: the range walk, refusal logic, and local reset against a temp repo, no network. Phase 2 reuses the phase 1 cases as Go table tests.
- Phase 2 only: `grep -r os/exec` over the module returns nothing; `push_verified` appears in `tools/list` over stdio and the live push works through an MCP client.

## Out of scope

Custom author or timestamps, merge commits, force push, rebase, the `mcp-server-git` verb set, REST fallback.

## Work split

Same shape as the PSProxmoxVE remediation: one worktree agent per unit, model by tier, synchronous reviewers before commit, orchestrator (Fable) verifies and merges. No two in-flight units edit the same file. This repo has no remote yet, so units land as local branches the orchestrator merges into `main`; the PR loop starts once the repo is on GitHub (open decision below).

| Unit | Tier | Model | Files | Depends on |
|---|---|---|---|---|
| P1 Range walk, refusal set, local reset, offline tests (Python) | moderate | Sonnet | `spike/push_verified.py` (git half), `spike/test_push_verified.py` | none |
| P2 App JWT to installation token, GraphQL client, `createRef` + `createCommitOnBranch` (Python) | mechanical | Sonnet | `spike/gh.py` | none |
| P3 Integrate P1+P2, live DoD on a throwaway PSProxmoxVE branch, measure payload ceiling, record in ADR 0002 | subtle runtime | Opus | `spike/push_verified.py` (main), ADR 0002 | P1, P2 |
| G1 Go module, go-git range walk, refusal set, blob reads, fetch + ref reset, partial-replay resume, table tests from P1/P3 | cross-cutting | Opus | `internal/replay/` | P3 |
| G2 `ghinstallation` auth, GraphQL client, mutations | mechanical | Sonnet | `internal/github/` | P3 |
| G3 MCP stdio server with one tool, wiring G1+G2, live DoD through an MCP client | cross-cutting | Opus | `cmd/mcp-server-github/`, `internal/tool/` | G1, G2 |
| G5 Fixes from the G3 review: empty-range panic, `partial` by pair count, userinfo refusal, single ceiling default | subtle runtime | Opus | `internal/replay/`, `internal/tool/` | G3 |
| G4 Agent-contract update for PSProxmoxVE, `--exclude-tools` on the official server, ADR statuses to Accepted | code-owned | orchestrator drafts, operator approves | external repos | G5 |

Waves: P1 and P2 in parallel, then P3 alone. G1 and G2 in parallel after P3, then G3 alone, then G4. Each agent gets the contract, the unit row, the DoD lines that apply, and runs `correctness-reviewer` plus `security-reviewer` synchronously; G1 and G3 add `architecture-reviewer`. Per-agent budget 350k tokens or two hours, then BLOCKED.

Live-push units (P3, G3) need the App PEM at `~/.config/github-agent/claude.pem`. If the agent sandbox cannot read it, the agent stops at "offline tests green, live step not run" and the orchestrator runs the live DoD.

Resolved 2026-09-03: the repo `GoodOlClint/mcp-server-github` exists; an installation token cannot create personal-account repos, so the operator created it. Spike units merged locally; Go units dogfood `push_verified`.

Spike findings carried into the Go units: the measured ceiling is time-bound (HTTP 499 from the edge, 30 s client timeout), so G2 uses a 120 s request timeout and G3 re-measures; `expectedHeadOid` compares against the ref the range was computed from; `HeadMismatchError` is a typed error, not a string match; replay and github packages share only an interface and a fake client for tests.
