# Agent contract

Read this first. Then `CLAUDE.md`, `docs/design.md`, and the ADRs in `docs/decisions/`.

## 1. Setup

- You are in a git worktree on your own branch. Do not create other branches. Do not touch `main`.
- Never modify `CLAUDE.md`, `docs/design.md`, or `docs/decisions/` unless your unit row says so.
- Comment discipline is enforced by a hook: comments only for constraints the code cannot express. No narration, no justification prose.
- Do not read or copy `~/.config/github-agent/*.pem`. Live GitHub calls, if your unit has them, use the three `GITHUB_APP_*` env vars and read the key at runtime only.
- Budget: 350k tokens or two hours. At either, stop, write the §5 report, end with BLOCKED.

## 2. Scope

Your unit row in `docs/design.md` "Work split" is the whole scope. Reviewer suggestions that add behaviour go in the report, not the diff. Anything you notice outside scope goes in the report.

## 3. Tests

- Offline tests run against a temp repo you create in the test; no network in tests.
- Mutation-test every new test once: break the logic it pins, confirm it fails, restore.
- Python: `python3 -m pytest spike/ -q` green. Go: `go test ./...` green and `go vet ./...` clean.

## 4. Review before committing

Run `correctness-reviewer` and `security-reviewer` on your diff, synchronously (`run_in_background: false`), plus any extra reviewer your unit row names. Verify each finding against the code before acting; say in the report what you rejected and why. Re-run §3 after fixes.

## 5. Commit and report

- One commit on your branch, conventional-commit message, body explaining the why. Author and committer come from the environment; do not set them.
- Do not push. The orchestrator merges local branches until the remote exists.
- Your final message is all the orchestrator sees. Lead with DONE (commit sha, branch) or BLOCKED (why). Then: files touched, test counts, mutation-test evidence, reviewers run and findings acted on or rejected, out-of-scope notes.
