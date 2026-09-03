# mcp-server-github

An MCP stdio server with one tool, `push_verified`. It replaces `git push` for a branch: it replays the local commits GitHub does not have as **Verified**, GitHub-App-authored commits, using the `createCommitOnBranch` GraphQL mutation, one API call per commit. File content is read from the local git object database, so no file content passes through the model.

Local `git commit` is unchanged. Only the push changes.

## Loud caveats

- **Every replayed commit is re-authored as the App and gets a new OID.** On success the local branch is reset to the remote OIDs. The trees are identical, so the working tree and the index do not change, and the old tips stay in the reflog.
- **One GitHub API call per commit.** A range of N commits is N mutations. A failure part way through leaves the earlier commits on the remote; re-running resumes from the first commit the remote lacks.
- **It refuses, before sending anything, commits it cannot represent.** `createCommitOnBranch` carries a path and base64 content but no file mode, so a mode change, a symlink or a submodule addition or modification, a merge commit, an empty commit, or a commit over the payload ceiling is refused with the commit and path named. Nothing is sent for the whole range. Use a plain local `git push` for those. See ADR 0002.
- **The remote must be `github.com`.** Any other host is refused.
- **`repo_path` must resolve inside a `--repo-root`.** Symlinks are resolved before the check. `--repo-root` bounds which local repositories can be read, **not** which GitHub repository is written: the destination comes from the named remote in that repository's `.git/config`. Point the roots at directories whose remotes you trust.
- **Linked worktrees are refused.** go-git reads a linked worktree's refs only with `EnableDotGitCommonDir`, which the replay engine does not set, so the tool refuses one and names the main working tree, which holds the same refs.

## Result

The tool always returns structured JSON: `kind`, `message`, `pairs` (local-to-remote OID pairs), `head`. Every `kind` except `success` also sets `isError` on the MCP result.

| `kind` | Meaning |
|---|---|
| `success` | The range is on the remote and the local branch was reset onto it. `pairs` is empty when there was nothing to do. |
| `refused` | The tool will not send this range: an unrepresentable commit, a bad `repo_path`, or a non-GitHub remote. Nothing was sent. |
| `retryable` | The remote moved before anything was sent. Nothing landed; call again. |
| `partial` | Some commits reached GitHub and then the replay stopped. `pairs` lists them; a re-run resumes. |
| `error` | Anything else. `pairs`, when present, lists what reached GitHub. |

## Arguments

| Name | Required | Default | Meaning |
|---|---|---|---|
| `repo_path` | yes | | Absolute path to the local repository, inside a `--repo-root`. |
| `branch` | yes | | Local branch whose commits are replayed. |
| `base` | no | `main` | Branch the remote branch is created from when it does not exist yet. |
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
| `--endpoint` | `https://api.github.com/graphql` | GraphQL endpoint. |
| `--timeout` | `120s` | Per-request timeout for GitHub GraphQL calls. |
| `--max-commit-bytes` | measured, see ADR 0002 | Per-commit payload ceiling. The number tracks the uplink, not a GitHub limit, so re-measure rather than inherit it. |

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
