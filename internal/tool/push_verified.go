// Package tool exposes the replay engine as the MCP tool push_verified.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoodOlClint/mcp-server-github/internal/github"
	"github.com/GoodOlClint/mcp-server-github/internal/replay"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultMaxCommitBytes is the per-commit payload ceiling: 5 MiB, the largest
// size that succeeded on every Go re-measure attempt, minus 10%. See ADR 0002.
const DefaultMaxCommitBytes int64 = 4_718_592

const (
	defaultBase   = "main"
	defaultRemote = "origin"
)

// Error kinds reported in the tool's structured output.
const (
	KindSuccess   = "success"
	KindRefused   = "refused"
	KindRetryable = "retryable"
	KindPartial   = "partial"
	KindError     = "error"
)

const toolDescription = `Replaces "git push" for a branch: replays the local commits GitHub does not have as Verified, App-authored commits.

LOUD CAVEATS, read before calling:
- Every replayed commit is RE-AUTHORED as the GitHub App and gets a NEW OID; on success the local branch is RESET to the remote OIDs (the trees are identical, so the working tree and index do not change).
- One GitHub API call per commit. A range of N commits is N mutations, and a failure part way through leaves the earlier commits on the remote; re-running resumes.
- REFUSES, before sending anything, any commit that changes a file mode, adds or modifies a symlink or a submodule, is a merge commit, or exceeds the payload ceiling. Use a plain local "git push" for those.
- "git commit" stays local and unchanged. This tool replaces the push, not the commit.
- File content is read from the local git object database, never from the model.`

// Input is the tool's argument set.
type Input struct {
	RepoPath string `json:"repo_path"`
	Branch   string `json:"branch"`
	Base     string `json:"base,omitempty"`
	Remote   string `json:"remote,omitempty"`
}

// Pair maps a local commit OID to the remote OID it was replayed as.
type Pair struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// Output is the tool's structured result. Kind is always set; Message is empty
// only on success.
type Output struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
	Pairs   []Pair `json:"pairs,omitempty"`
	Head    string `json:"head,omitempty"`
}

// Config builds a Server. Roots is the allow-list of directories a repo_path
// may resolve inside; an empty Roots is an error, so the caller must pass at
// least one.
type Config struct {
	Client         replay.Client
	Transport      http.RoundTripper
	Roots          []string
	MaxCommitBytes int64
}

// Server holds the resolved configuration behind the push_verified handler.
type Server struct {
	client         replay.Client
	roots          []string
	maxCommitBytes int64
	auth           func(context.Context) (transport.AuthMethod, error)
	push           func(context.Context, replay.Client, replay.Options) (replay.Result, error)
}

// New resolves and validates the configuration. Every root must be an absolute
// path to an existing directory; roots are resolved through symlinks so that
// containment is decided on real paths.
func New(cfg Config) (*Server, error) {
	if cfg.Client == nil {
		return nil, errors.New("tool: no GitHub client")
	}
	if cfg.Transport == nil {
		return nil, errors.New("tool: no authenticated transport")
	}
	// github.Token needs an installation transport; assert it at startup
	// rather than on the first call.
	if _, ok := cfg.Transport.(interface {
		Token(context.Context) (string, error)
	}); !ok {
		return nil, fmt.Errorf("tool: transport %T does not expose an installation token", cfg.Transport)
	}
	if len(cfg.Roots) == 0 {
		return nil, errors.New("tool: at least one repo root is required")
	}
	roots := make([]string, 0, len(cfg.Roots))
	for _, r := range cfg.Roots {
		resolved, err := resolveDir(r)
		if err != nil {
			return nil, fmt.Errorf("tool: repo root %q: %w", r, err)
		}
		roots = append(roots, resolved)
	}
	max := cfg.MaxCommitBytes
	if max <= 0 {
		max = DefaultMaxCommitBytes
	}
	rt := cfg.Transport
	return &Server{
		client:         cfg.Client,
		roots:          roots,
		maxCommitBytes: max,
		auth: func(ctx context.Context) (transport.AuthMethod, error) {
			tok, err := github.Token(ctx, rt)
			if err != nil {
				return nil, err
			}
			return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
		},
		push: replay.Push,
	}, nil
}

func resolveDir(p string) (string, error) {
	if p == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("is not a directory")
	}
	return resolved, nil
}

// inputSchema is the declared schema for push_verified arguments.
func inputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"repo_path": {
				Type:        "string",
				Description: "Absolute path to the local git repository, inside a configured --repo-root.",
			},
			"branch": {
				Type:        "string",
				Description: "Local branch whose commits are replayed. It must be the branch holding the commits, not a tag or a detached HEAD.",
			},
			"base": {
				Type:        "string",
				Description: "Branch the new remote branch is created from when it does not exist yet.",
				Default:     json.RawMessage(`"` + defaultBase + `"`),
			},
			"remote": {
				Type:        "string",
				Description: "Name of the git remote to push to. It must point at github.com.",
				Default:     json.RawMessage(`"` + defaultRemote + `"`),
			},
		},
		Required:             []string{"repo_path", "branch"},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

// Register adds push_verified to an MCP server.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "push_verified",
		Description: toolDescription,
		InputSchema: inputSchema(),
	}, s.Handle)
}

// Handle runs one push_verified call. Every non-success outcome is returned as
// a tool error: IsError on the result, the reason in the structured output.
func (s *Server) Handle(ctx context.Context, _ *mcp.CallToolRequest, in Input) (*mcp.CallToolResult, Output, error) {
	out := s.run(ctx, in)
	if out.Kind == KindSuccess {
		return nil, out, nil
	}
	return &mcp.CallToolResult{IsError: true}, out, nil
}

// run recovers from a panic in the replay engine: this handler runs inside the
// server's only process, so an unrecovered panic would drop the JSON-RPC stream
// and leave the caller with no report of what already reached GitHub.
func (s *Server) run(ctx context.Context, in Input) (out Output) {
	defer func() {
		if r := recover(); r != nil {
			out = Output{Kind: KindError, Message: fmt.Sprintf("internal error: %v", r)}
		}
	}()

	if in.Base == "" {
		in.Base = defaultBase
	}
	if in.Remote == "" {
		in.Remote = defaultRemote
	}
	if strings.TrimSpace(in.Branch) == "" {
		return Output{Kind: KindRefused, Message: "branch is required"}
	}

	repoPath, err := s.resolveRepoPath(in.RepoPath)
	if err != nil {
		return Output{Kind: kindFor(err), Message: err.Error()}
	}

	auth, err := s.auth(ctx)
	if err != nil {
		return Output{Kind: KindError, Message: err.Error()}
	}

	res, err := s.push(ctx, s.client, replay.Options{
		RepoPath:       repoPath,
		Branch:         in.Branch,
		Base:           in.Base,
		Remote:         in.Remote,
		MaxCommitBytes: s.maxCommitBytes,
		Auth:           auth,
	})
	return classify(res, err)
}

func classify(res replay.Result, err error) Output {
	pairs := toPairs(res.Pairs)
	if err == nil {
		return Output{Kind: KindSuccess, Pairs: pairs, Head: res.Head}
	}

	var sync *replay.SyncError
	isSync := errors.As(err, &sync)
	if len(pairs) == 0 && isSync {
		pairs = toPairs(sync.Replayed)
	}

	// Commits reached GitHub, so the outcome is resumable whatever failed next.
	if len(pairs) > 0 {
		return Output{Kind: KindPartial, Message: err.Error(), Pairs: pairs}
	}

	var refused *replay.RefusedError
	if errors.As(err, &refused) {
		return Output{Kind: KindRefused, Message: err.Error()}
	}
	if isSync {
		return Output{Kind: KindRetryable, Message: err.Error()}
	}
	return Output{Kind: KindError, Message: err.Error()}
}

// refusal marks a boundary error as policy rather than a filesystem failure, so
// an unreadable or missing path is not reported as a terminal refusal.
type refusal struct{ err error }

func (r *refusal) Error() string { return r.err.Error() }

func (r *refusal) Unwrap() error { return r.err }

func refusef(format string, a ...any) error { return &refusal{fmt.Errorf(format, a...)} }

func kindFor(err error) string {
	var r *refusal
	if errors.As(err, &r) {
		return KindRefused
	}
	return KindError
}

func toPairs(in []replay.Pair) []Pair {
	if len(in) == 0 {
		return nil
	}
	out := make([]Pair, 0, len(in))
	for _, p := range in {
		out = append(out, Pair{Local: p.Local, Remote: p.Remote})
	}
	return out
}

// resolveRepoPath applies the boundary rules: absolute, existing, resolved
// through symlinks, inside a configured root, and openable by go-git without
// searching parent directories. A linked worktree is refused: go-git reads a
// linked worktree only with EnableDotGitCommonDir, which the replay engine does
// not set, so accepting one would fail later with an unrelated message.
func (s *Server) resolveRepoPath(raw string) (string, error) {
	if raw == "" {
		return "", refusef("repo_path is required")
	}
	if !filepath.IsAbs(raw) {
		return "", refusef("repo_path %q must be an absolute path", raw)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(raw))
	if err != nil {
		return "", fmt.Errorf("repo_path %q: %w", raw, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("repo_path %q: %w", raw, err)
	}
	if !info.IsDir() {
		return "", refusef("repo_path %q is not a directory", raw)
	}
	if !s.inRoots(resolved) {
		return "", refusef("repo_path %q resolves to %q, which is outside every --repo-root", raw, resolved)
	}

	gitDir, err := gitDirOf(resolved)
	if err != nil {
		return "", fmt.Errorf("repo_path %q: %w", raw, err)
	}
	if gitDir != "" {
		msg := fmt.Sprintf("repo_path %q is a linked worktree; its git directory is %q", raw, gitDir)
		if main := mainWorkTreeOf(gitDir); main != "" {
			msg += fmt.Sprintf(". Push from the main working tree %q instead; it holds the same refs", main)
		}
		if !s.inRoots(gitDir) {
			msg += ", and that git directory is outside every --repo-root"
		}
		return "", &refusal{errors.New(msg)}
	}

	if _, err := git.PlainOpenWithOptions(resolved, &git.PlainOpenOptions{DetectDotGit: false}); err != nil {
		if errors.Is(err, git.ErrRepositoryNotExists) {
			return "", refusef("repo_path %q: %v", raw, err)
		}
		return "", fmt.Errorf("repo_path %q: %w", raw, err)
	}
	return resolved, nil
}

// inRoots reports whether path is a configured root or lies under one.
func (s *Server) inRoots(path string) bool {
	for _, root := range s.roots {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

const maxGitDirFileBytes = 4096

// gitDirOf returns the resolved git directory when repoPath/.git is a gitdir
// file, and "" when it is an ordinary directory or absent; an absent .git is
// left for go-git to report.
func gitDirOf(repoPath string) (string, error) {
	dot := filepath.Join(repoPath, ".git")
	info, err := os.Lstat(dot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", nil
	}
	if !info.Mode().IsRegular() {
		return "", errors.New(".git is neither a directory nor a regular file")
	}
	// Reading the whole file keeps the path this function validates identical to
	// the path go-git would open.
	if info.Size() > maxGitDirFileBytes {
		return "", fmt.Errorf(".git file is larger than %d bytes", maxGitDirFileBytes)
	}
	raw, err := os.ReadFile(dot)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) {
		return "", errors.New(".git file does not name a git directory")
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if gitDir == "" {
		return "", errors.New(".git file does not name a git directory")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(gitDir))
	if err != nil {
		return "", fmt.Errorf("git directory %q: %w", gitDir, err)
	}
	return resolved, nil
}

// mainWorkTreeOf derives the main working tree of a linked worktree from the
// commondir file git writes beside its refs, and returns "" when it cannot.
func mainWorkTreeOf(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(raw))
	if common == "" {
		return ""
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(common))
	if err != nil {
		return ""
	}
	if filepath.Base(resolved) != ".git" {
		return ""
	}
	return filepath.Dir(resolved)
}
