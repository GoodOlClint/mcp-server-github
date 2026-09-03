# mcp-server-github

An MCP stdio server with one tool, `push_verified`. It replaces `git push` for a branch: it replays the local commits GitHub does not have as **Verified**, GitHub-App-authored commits, over the Git Data REST API. File content is read from the local git object database, so no file content passes through the model.

Local `git commit` is unchanged. Only the push changes.

## Loud caveats

- **Every replayed commit is re-authored as the App and gets a new OID.** On success the local branch is reset to the remote OIDs. The trees are identical, so the working tree and the index do not change, and the old tips stay in the reflog.
- **File modes are carried.** A `100755` script, a `120000` symlink and a `160000` submodule entry land on the remote with the mode they have locally, so a `chmod +x` and a new executable script push like any other change. See ADR 0006.
- **The remote branch moves once, at the end.** A push is one blob upload per distinct file content, then a tree and a commit per local commit, then a single non-force ref update. Any failure before that ref update leaves the remote branch exactly where it was: the objects already uploaded are unreachable and GitHub collects them, and re-running replays the range from scratch.
- **It refuses, before sending anything, commits it will not replay.** A merge commit, an empty commit, a path git itself will not check out (a `.git` segment, a `..` segment, an absolute path, invalid UTF-8), an empty commit message, a single file over 24 MiB (the largest blob the API accepts in one request, see ADR 0006), or a commit over `--max-commit-bytes` is refused with the commit and path named. Nothing is sent for the whole range. Use a plain local `git push` for those.
- **The remote must be `github.com`.** Any other host is refused.
- **`repo_path` must resolve inside a `--repo-root`.** Symlinks are resolved before the check. `--repo-root` bounds which local repositories can be read, **not** which GitHub repository is written: the destination comes from the named remote in that repository's `.git/config`. Point the roots at directories whose remotes you trust.
- **Linked worktrees are refused.** go-git reads a linked worktree's refs only with `EnableDotGitCommonDir`, which the replay engine does not set, so the tool refuses one and names the main working tree, which holds the same refs.

## Result

The tool always returns structured JSON: `kind`, `message`, `pairs` (local-to-remote OID pairs), `head`. Every `kind` except `success` also sets `isError` on the MCP result.

| `kind` | Meaning |
|---|---|
| `success` | The range is on the remote and the local branch was reset onto it. `pairs` is empty when there was nothing to do. |
| `refused` | The tool will not send this range: a refused commit, a bad `repo_path`, or a non-GitHub remote. Nothing was sent. |
| `retryable` | The remote branch moved before the ref update, so nothing landed on it; call again. |
| `partial` | The commits reached the remote branch and the local ref reset then failed. `pairs` lists them; a re-run resumes. |
| `error` | Anything else. Nothing reached the remote branch. |

## Arguments

| Name | Required | Default | Meaning |
|---|---|---|---|
| `repo_path` | yes | | Absolute path to the local repository, inside a `--repo-root`. |
| `branch` | yes | | Local branch whose commits are replayed. |
| `base` | no | `main` | Branch the range is computed against when the remote does not have the branch yet. |
| `remote` | no | `origin` | Git remote to push to. Must point at github.com. |

## Build

```
go build ./cmd/mcp-server-github
```

## Auth

The same three variables the official GitHub MCP server uses for App auth. There is no PAT path. The App needs Contents read and write on the target repositories.

- `GITHUB_APP_ID`
- `GITHUB_APP_INSTALLATION_ID`
- `GITHUB_APP_PRIVATE_KEY_PATH`

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--repo-root` | none | Directory a `repo_path` may resolve inside. Repeatable. **Required**: with no root, no path is accepted. |
| `--endpoint` | `https://api.github.com` | REST API root. |
| `--timeout` | `120s` | Per-request timeout for GitHub API calls. |
| `--max-commit-bytes` | `52428800` (50 MB), see ADR 0006 | Per-commit ceiling on uploaded bytes. It tracks the uplink, not a GitHub limit, so re-measure rather than inherit it. |

## Launch config for Claude Code

```
claude mcp add --scope user push-verified \
  -e GITHUB_APP_ID=... \
  -e GITHUB_APP_INSTALLATION_ID=... \
  -e GITHUB_APP_PRIVATE_KEY_PATH=... \
  -- /path/to/mcp-server-github --repo-root /Users/you/Source
```

## Coexistence with the official github-mcp-server

The official server's `create_or_update_file` and `push_files` mint their own blob SHAs, need full file content in the model, and drop file modes. Once `push_verified` is in the agent contract, exclude them:

```
--exclude-tools create_or_update_file,push_files
```
