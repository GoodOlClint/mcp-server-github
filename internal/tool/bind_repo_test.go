package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/mcp-server-github/internal/replay"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

func repoWithRemote(t *testing.T, dir, name, url string) {
	t.Helper()
	initRepo(t, dir)
	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: name, URLs: []string{url}}); err != nil {
		t.Fatal(err)
	}
}

// boundServer fails the test if refusal reaches the token mint or the push,
// the two places a network call can start.
func boundServer(t *testing.T, root, bind string) (*Server, *int) {
	t.Helper()
	s, err := New(Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{root}, BindRepo: bind})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pushes := 0
	s.auth = func(context.Context) (transport.AuthMethod, error) {
		t.Error("auth called: a token mint is a network call")
		return nil, nil
	}
	s.push = func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
		pushes++
		return replay.Result{Head: "abc"}, nil
	}
	return s, &pushes
}

func TestBindRepoRefusesBeforeAnyNetworkCall(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	other := filepath.Join(root, "other")
	repoWithRemote(t, other, "origin", "git@github.com:Evil/other.git")
	alt := filepath.Join(root, "alt")
	repoWithRemote(t, alt, "mirror", "https://github.com/Evil/alt.git")
	nested := filepath.Join(root, "bound", "vendor", "clone")
	repoWithRemote(t, filepath.Join(root, "bound"), "origin", "git@github.com:GoodOlClint/bound.git")
	repoWithRemote(t, nested, "origin", "git@github.com:Evil/clone.git")

	cases := map[string]Input{
		"other origin":     {RepoPath: other, Branch: "b"},
		"alternate remote": {RepoPath: alt, Branch: "b", Remote: "mirror"},
		"nested clone":     {RepoPath: nested, Branch: "b"},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			s, pushes := boundServer(t, root, "GoodOlClint/bound")
			out := s.run(context.Background(), in)
			if out.Kind != KindRefused || !strings.Contains(out.Message, "GoodOlClint/bound") {
				t.Fatalf("got %q %q, want refused naming the bound repo", out.Kind, out.Message)
			}
			if *pushes != 0 {
				t.Fatal("push reached")
			}
		})
	}
}

func TestBindRepoAllowsTheBoundRepo(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(root, "bound")
	repoWithRemote(t, repo, "origin", "git@github.com:goodolclint/Bound.git")
	s, err := New(Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{root}, BindRepo: "GoodOlClint/bound"})
	if err != nil {
		t.Fatal(err)
	}
	s.auth = func(context.Context) (transport.AuthMethod, error) { return nil, nil }
	pushes := 0
	s.push = func(context.Context, replay.Client, replay.Options) (replay.Result, error) {
		pushes++
		return replay.Result{Head: "abc"}, nil
	}
	if out := s.run(context.Background(), Input{RepoPath: repo, Branch: "b"}); out.Kind != KindSuccess || pushes != 1 {
		t.Fatalf("got %q %q pushes=%d, want success", out.Kind, out.Message, pushes)
	}
}

func TestUnsetBindRepoAllowsAnyRepo(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(root, "any")
	repoWithRemote(t, repo, "origin", "git@github.com:Whoever/any.git")
	s := newServer(t, []string{root}, nil)
	if out := s.run(context.Background(), Input{RepoPath: repo, Branch: "b"}); out.Kind != KindSuccess {
		t.Fatalf("got %q %q, want success", out.Kind, out.Message)
	}
}

func TestNewRejectsMalformedBindRepo(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	for _, bad := range []string{"noslash", "a/b/c", "/b", "a/", "a b/c"} {
		if _, err := New(Config{Client: fakeClient{}, Transport: stubTransport{}, Roots: []string{root}, BindRepo: bad}); err == nil {
			t.Errorf("BindRepo %q accepted", bad)
		}
	}
}
