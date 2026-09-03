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
	"testing"

	"github.com/GoodOlClint/mcp-server-github/internal/replay"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeClient struct{}

func (fakeClient) BranchHead(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (fakeClient) CreateBranch(context.Context, string, string, string, string) (string, error) {
	return "", nil
}

func (fakeClient) CreateCommit(context.Context, string, string, string, string, string, []replay.FileAddition, []string) (string, error) {
	return "", nil
}

type stubTransport struct{}

func (stubTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("stub transport")
}

func (stubTransport) Token(context.Context) (string, error) { return "stub-token", nil }

// plainTransport has no Token method, so New must reject it.
type plainTransport struct{}

func (plainTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("plain transport")
}

// initRepo makes a real repository at dir so go-git's PlainOpen succeeds.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
}

// newServer builds a Server whose auth and push are stubs, so no test touches
// the network or the App key.
func newServer(t *testing.T, roots []string, push func(context.Context, replay.Client, replay.Options) (replay.Result, error)) *Server {
	t.Helper()
	s, err := New(Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: roots})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.auth = func(context.Context) (transport.AuthMethod, error) {
		return &githttp.BasicAuth{Username: "x-access-token", Password: "stub-token"}, nil
	}
	if push != nil {
		s.push = push
	} else {
		s.push = func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
			return replay.Result{Head: "deadbeef"}, nil
		}
	}
	return s
}

func TestNewRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no client", Config{Transport: stubTransport{}, Roots: []string{dir}}},
		{"no transport", Config{Client: fakeClient{}, Roots: []string{dir}}},
		{"no roots", Config{Client: fakeClient{}, Transport: stubTransport{}}},
		{"relative root", Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{"relative"}}},
		{"missing root", Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{filepath.Join(dir, "nope")}}},
		{"root is a file", Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{fileIn(dir)}}},
		{"transport without a token", Config{Client: fakeClient{}, Transport: plainTransport{}, Roots: []string{dir}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func fileIn(dir string) string {
	p := filepath.Join(dir, "a-file")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		panic(err)
	}
	return p
}

func TestNewHonoursExplicitMaxCommitBytes(t *testing.T) {
	s, err := New(Config{
		Client: fakeClient{}, Transport: stubTransport{},
		Roots: []string{t.TempDir()}, MaxCommitBytes: 123,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.maxCommitBytes != 123 {
		t.Fatalf("maxCommitBytes = %d, want 123", s.maxCommitBytes)
	}
}

func TestRootIsAcceptedWhenItIsTheRepoItself(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	initRepo(t, root)
	s := newServer(t, []string{root}, nil)
	got, err := s.resolveRepoPath(root)
	if err != nil {
		t.Fatalf("resolveRepoPath: %v", err)
	}
	if got != root {
		t.Fatalf("resolveRepoPath = %q, want %q", got, root)
	}
}

func TestNewDefaultsMaxCommitBytes(t *testing.T) {
	s := newServer(t, []string{t.TempDir()}, nil)
	if s.maxCommitBytes != DefaultMaxCommitBytes {
		t.Fatalf("maxCommitBytes = %d, want %d", s.maxCommitBytes, DefaultMaxCommitBytes)
	}
}

func TestRepoPathValidation(t *testing.T) {
	root := t.TempDir()
	// macOS hands out /var/folders temp dirs behind a symlink.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	good := filepath.Join(root, "good")
	initRepo(t, good)

	notRepo := filepath.Join(root, "not-a-repo")
	if err := os.MkdirAll(notRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	outsideRepo := filepath.Join(outside, "elsewhere")
	initRepo(t, outsideRepo)

	// A symlink inside the root pointing at a repo outside it must not smuggle
	// the outside repo past the allow-list.
	escaping := filepath.Join(root, "escape")
	if err := os.Symlink(outsideRepo, escaping); err != nil {
		t.Fatal(err)
	}

	// A symlink inside the root pointing at a repo inside it is fine.
	inward := filepath.Join(root, "inward")
	if err := os.Symlink(good, inward); err != nil {
		t.Fatal(err)
	}

	// A file, not a directory.
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A nested directory inside a repo: go-git must not walk up to find .git.
	nested := filepath.Join(good, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	s := newServer(t, []string{root}, nil)

	for _, tc := range []struct {
		name     string
		path     string
		want     string
		wantKind string
	}{
		{"relative", "relative/path", "absolute", KindRefused},
		{"outside root", outsideRepo, "outside every --repo-root", KindRefused},
		{"symlink escaping root", escaping, "outside every --repo-root", KindRefused},
		{"not a directory", file, "is not a directory", KindRefused},
		{"not a repository", notRepo, "repository does not exist", KindRefused},
		{"nested inside a repository", nested, "repository does not exist", KindRefused},
		{"empty", "", "required", KindRefused},
		// A path that is missing or unreadable is a filesystem problem, not a
		// policy refusal: an agent must not read it as "stop, use git push".
		{"missing", filepath.Join(root, "nope"), "no such file", KindError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.resolveRepoPath(tc.path)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := kindFor(err); got != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got, tc.wantKind)
			}
		})
	}

	t.Run("accepts a repo in the root", func(t *testing.T) {
		got, err := s.resolveRepoPath(good)
		if err != nil {
			t.Fatalf("resolveRepoPath: %v", err)
		}
		if got != good {
			t.Fatalf("resolveRepoPath = %q, want %q", got, good)
		}
	})

	t.Run("resolves an inward symlink", func(t *testing.T) {
		got, err := s.resolveRepoPath(inward)
		if err != nil {
			t.Fatalf("resolveRepoPath: %v", err)
		}
		if got != good {
			t.Fatalf("resolveRepoPath = %q, want %q", got, good)
		}
	})
}

// A gitdir-file worktree is refused whether or not its git directory is inside
// a root, and the refusal names the main working tree.
func TestGitDirFileWorktreeIsRefused(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "main")
	initRepo(t, main)
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"HEAD":      "ref: refs/heads/wt\n",
		"commondir": "../..\n",
		"gitdir":    filepath.Join(base, "wt", ".git") + "\n",
	} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt := filepath.Join(base, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		roots []string
		want  []string
	}{
		{"gitdir inside a root", []string{base}, []string{"linked worktree", `main working tree "` + main + `"`}},
		{"gitdir outside every root", []string{wt}, []string{"linked worktree", "outside every --repo-root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, tc.roots, nil)
			_, err := s.resolveRepoPath(wt)
			if err == nil {
				t.Fatal("want a refusal, got nil")
			}
			if kindFor(err) != KindRefused {
				t.Fatalf("kind = %q, want refused", kindFor(err))
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// A malformed or oversized .git file is a filesystem problem, not a policy
// refusal, and never yields a truncated git directory.
func TestGitDirFileErrors(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(t, []string{base}, nil)

	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"no gitdir prefix", "not a gitdir\n", "does not name a git directory"},
		{"empty target", "gitdir: \n", "does not name a git directory"},
		{"empty file", "", "does not name a git directory"},
		{"oversized", "gitdir: " + strings.Repeat("a", maxGitDirFileBytes), "larger than"},
		{"missing target", "gitdir: " + filepath.Join(base, "nowhere") + "\n", "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(base, "r-"+strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := s.resolveRepoPath(dir)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if kindFor(err) != KindError {
				t.Fatalf("kind = %q, want error", kindFor(err))
			}
		})
	}
}

// A relative gitdir target is resolved against the repository, as go-git does.
func TestGitDirFileRelativeTarget(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "wt")
	target := filepath.Join(base, "wt", "real-git")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: real-git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := gitDirOf(dir)
	if err != nil {
		t.Fatalf("gitDirOf: %v", err)
	}
	if got != target {
		t.Fatalf("gitDirOf = %q, want %q", got, target)
	}
}

func TestDefaultsAreFilled(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)

	var got replay.Options
	s := newServer(t, []string{root}, func(_ context.Context, _ replay.Client, o replay.Options) (replay.Result, error) {
		got = o
		return replay.Result{Head: "abc"}, nil
	})

	out := s.run(context.Background(), Input{RepoPath: repo, Branch: "feature"})
	if out.Kind != KindSuccess {
		t.Fatalf("kind = %q (%s), want success", out.Kind, out.Message)
	}
	if got.Base != "main" || got.Remote != "origin" {
		t.Fatalf("base/remote = %q/%q, want main/origin", got.Base, got.Remote)
	}
	if got.MaxCommitBytes != DefaultMaxCommitBytes {
		t.Fatalf("MaxCommitBytes = %d, want %d", got.MaxCommitBytes, DefaultMaxCommitBytes)
	}
	if got.RepoPath != repo {
		t.Fatalf("RepoPath = %q, want %q", got.RepoPath, repo)
	}
	if got.Branch != "feature" {
		t.Fatalf("Branch = %q, want feature", got.Branch)
	}
	basic, ok := got.Auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("Auth = %T, want *http.BasicAuth minted for this call", got.Auth)
	}
	if basic.Username != "x-access-token" || basic.Password == "" {
		t.Fatalf("Auth = %+v, want an x-access-token basic auth carrying a token", basic)
	}
}

func TestExplicitBaseAndRemoteSurvive(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)

	var got replay.Options
	s := newServer(t, []string{root}, func(_ context.Context, _ replay.Client, o replay.Options) (replay.Result, error) {
		got = o
		return replay.Result{Head: "abc"}, nil
	})
	s.run(context.Background(), Input{RepoPath: repo, Branch: "b", Base: "trunk", Remote: "upstream"})
	if got.Base != "trunk" || got.Remote != "upstream" {
		t.Fatalf("base/remote = %q/%q, want trunk/upstream", got.Base, got.Remote)
	}
}

func TestMissingBranchIsRefused(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)
	s := newServer(t, []string{root}, func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
		t.Fatal("push must not run without a branch")
		return replay.Result{}, nil
	})
	out := s.run(context.Background(), Input{RepoPath: repo, Branch: "  "})
	if out.Kind != KindRefused {
		t.Fatalf("kind = %q, want refused", out.Kind)
	}
}

// A panic in the replay engine must become a tool error, not a dead server:
// the engine indexes an empty pair slice when the range is empty and the remote
// branch does not exist yet.
func TestPanicInPushBecomesAToolError(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)

	s := newServer(t, []string{root}, func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
		var pairs []replay.Pair
		_ = pairs[len(pairs)-1]
		return replay.Result{}, nil
	})

	res, out, err := s.Handle(context.Background(), nil, Input{RepoPath: repo, Branch: "b"})
	if err != nil {
		t.Fatalf("Handle returned a protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("panic did not set IsError")
	}
	if out.Kind != KindError {
		t.Fatalf("kind = %q, want error", out.Kind)
	}
	if !strings.Contains(out.Message, "internal error") {
		t.Fatalf("message %q does not report an internal error", out.Message)
	}
}

func TestClassify(t *testing.T) {
	pairs := []replay.Pair{{Local: "l1", Remote: "r1"}, {Local: "l2", Remote: "r2"}}

	for _, tc := range []struct {
		name      string
		res       replay.Result
		err       error
		wantKind  string
		wantPairs int
		wantHead  string
	}{
		{
			name:      "success",
			res:       replay.Result{Pairs: pairs, Head: "r2"},
			wantKind:  KindSuccess,
			wantPairs: 2,
			wantHead:  "r2",
		},
		{
			name:     "success with nothing to do",
			res:      replay.Result{Head: "r0"},
			wantKind: KindSuccess,
			wantHead: "r0",
		},
		{
			name:     "refused",
			err:      &replay.RefusedError{OID: "abc", Path: "scripts/x.sh", Reason: "mode change"},
			wantKind: KindRefused,
		},
		{
			name:     "wrapped refused",
			err:      fmt.Errorf("open: %w", &replay.RefusedError{Reason: "bad remote"}),
			wantKind: KindRefused,
		},
		{
			name:     "sync error with nothing replayed",
			err:      &replay.SyncError{Reason: "head moved"},
			wantKind: KindRetryable,
		},
		{
			name:      "sync error after a partial replay",
			res:       replay.Result{Pairs: pairs},
			err:       &replay.SyncError{Reason: "head moved mid-range", Replayed: pairs},
			wantKind:  KindPartial,
			wantPairs: 2,
		},
		{
			name:      "sync error carrying pairs the result lacks",
			err:       &replay.SyncError{Reason: "head moved mid-range", Replayed: pairs},
			wantKind:  KindPartial,
			wantPairs: 2,
		},
		{
			name:     "head mismatch",
			err:      &replay.HeadMismatchError{Expected: "abc", Message: "stale"},
			wantKind: KindError,
		},
		{
			name:     "other error",
			err:      errors.New("boom"),
			wantKind: KindError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := classify(tc.res, tc.err)
			if out.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", out.Kind, tc.wantKind)
			}
			if len(out.Pairs) != tc.wantPairs {
				t.Fatalf("pairs = %d, want %d", len(out.Pairs), tc.wantPairs)
			}
			if out.Head != tc.wantHead {
				t.Fatalf("head = %q, want %q", out.Head, tc.wantHead)
			}
			if tc.wantKind == KindSuccess {
				if out.Message != "" {
					t.Fatalf("success carries message %q", out.Message)
				}
			} else if out.Message == "" {
				t.Fatal("non-success carries no message")
			}
		})
	}
}

func TestRefusedMessageNamesThePath(t *testing.T) {
	out := classify(replay.Result{}, &replay.RefusedError{
		OID: "abc123", Path: "scripts/build.sh", Reason: "mode 100755 is not representable",
	})
	if !strings.Contains(out.Message, "scripts/build.sh") {
		t.Fatalf("message %q does not name the path", out.Message)
	}
}

func TestHandleSetsIsError(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)

	t.Run("success", func(t *testing.T) {
		s := newServer(t, []string{root}, nil)
		res, out, err := s.Handle(context.Background(), nil, Input{RepoPath: repo, Branch: "b"})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if res != nil && res.IsError {
			t.Fatal("success set IsError")
		}
		if out.Kind != KindSuccess {
			t.Fatalf("kind = %q, want success", out.Kind)
		}
	})

	t.Run("failure", func(t *testing.T) {
		s := newServer(t, []string{root}, func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
			return replay.Result{}, errors.New("boom")
		})
		res, out, err := s.Handle(context.Background(), nil, Input{RepoPath: repo, Branch: "b"})
		if err != nil {
			t.Fatalf("Handle returned a protocol error: %v", err)
		}
		if res == nil || !res.IsError {
			t.Fatal("failure did not set IsError")
		}
		if out.Kind != KindError {
			t.Fatalf("kind = %q, want error", out.Kind)
		}
	})
}

func TestAuthFailureIsNotRefusal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	initRepo(t, repo)

	s := newServer(t, []string{root}, func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
		t.Fatal("push must not run when auth fails")
		return replay.Result{}, nil
	})
	s.auth = func(context.Context) (transport.AuthMethod, error) { return nil, errors.New("no token") }

	out := s.run(context.Background(), Input{RepoPath: repo, Branch: "b"})
	if out.Kind != KindError {
		t.Fatalf("kind = %q, want error", out.Kind)
	}
}

// The MCP round trip: an in-process client sees the tool in tools/list with the
// declared schema, and a call with a bad repo_path comes back as a tool error
// carrying kind "refused".
func TestMCPRoundTrip(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s := newServer(t, []string{root}, nil)
	srv := mcp.NewServer(&mcp.Implementation{Name: "push-verified", Version: "test"}, nil)
	s.Register(srv)

	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "push_verified" {
		t.Fatalf("tools/list = %+v, want one push_verified", tools.Tools)
	}
	tl := tools.Tools[0]
	for _, want := range []string{"Verified", "RE-AUTHORED", "REFUSES", "git commit"} {
		if !strings.Contains(tl.Description, want) {
			t.Fatalf("description does not mention %q", want)
		}
	}
	schema, err := json.Marshal(tl.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type       string `json:"type"`
		Required   []string
		Properties map[string]struct {
			Type    string
			Default any
		}
	}
	if err := json.Unmarshal(schema, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "object" {
		t.Fatalf("schema type = %q, want object", decoded.Type)
	}
	if strings.Join(decoded.Required, ",") != "repo_path,branch" {
		t.Fatalf("required = %v, want [repo_path branch]", decoded.Required)
	}
	for _, name := range []string{"repo_path", "branch", "base", "remote"} {
		p, ok := decoded.Properties[name]
		if !ok {
			t.Fatalf("schema has no property %q", name)
		}
		if p.Type != "string" {
			t.Fatalf("property %q type = %q, want string", name, p.Type)
		}
	}
	if decoded.Properties["base"].Default != "main" {
		t.Fatalf("base default = %v, want main", decoded.Properties["base"].Default)
	}
	if decoded.Properties["remote"].Default != "origin" {
		t.Fatalf("remote default = %v, want origin", decoded.Properties["remote"].Default)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "push_verified",
		Arguments: map[string]any{"repo_path": "not/absolute", "branch": "feature"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Fatal("bad repo_path did not set IsError")
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out Output
	if err := json.Unmarshal(structured, &out); err != nil {
		t.Fatalf("structured content %s: %v", structured, err)
	}
	if out.Kind != KindRefused {
		t.Fatalf("kind = %q, want refused (message %q)", out.Kind, out.Message)
	}
	if out.Message == "" {
		t.Fatal("refusal carries no message")
	}
}
