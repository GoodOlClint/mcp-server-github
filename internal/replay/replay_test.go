package replay

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// -- https transport backed by on-disk bare repos, so the tests exercise the
// real remote URL parsing without a network.

type served struct {
	dir    string
	client *git.Repository
}

var (
	loaderMu sync.RWMutex
	loaderBy = map[string]*served{}
)

type testLoader struct{}

func (testLoader) Load(ep *transport.Endpoint) (storer.Storer, error) {
	loaderMu.RLock()
	s, ok := loaderBy[ep.Path]
	loaderMu.RUnlock()
	if !ok {
		return nil, transport.ErrRepositoryNotFound
	}
	st := filesystem.NewStorage(osfs.New(s.dir), cache.NewObjectLRUDefault())
	return &clientAwareStorer{Storer: st, client: s.client}, nil
}

// clientAwareStorer lets object reads fall back to the fetching repository.
// go-git's in-process server resolves every "have" line the client advertises
// and fails the whole fetch when one names an object it does not hold; real git
// servers ignore those. Without the fallback no test could fetch a branch while
// the local repository still had unreplayed commits.
type clientAwareStorer struct {
	storer.Storer
	client *git.Repository
}

func (s *clientAwareStorer) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := s.Storer.EncodedObject(t, h)
	if err == plumbing.ErrObjectNotFound && s.client != nil {
		return s.client.Storer.EncodedObject(t, h)
	}
	return obj, err
}

func init() {
	client.InstallProtocol("https", server.NewClient(testLoader{}))
}

// -- object construction ---------------------------------------------------

type fileSpec struct {
	mode    filemode.FileMode
	content []byte
	hash    plumbing.Hash
}

func regular(content string) fileSpec {
	return fileSpec{mode: filemode.Regular, content: []byte(content)}
}

func regularBytes(content []byte) fileSpec {
	return fileSpec{mode: filemode.Regular, content: content}
}

func executable(content string) fileSpec {
	return fileSpec{mode: filemode.Executable, content: []byte(content)}
}

func symlink(target string) fileSpec {
	return fileSpec{mode: filemode.Symlink, content: []byte(target)}
}

func submodule(h string) fileSpec {
	return fileSpec{mode: filemode.Submodule, hash: plumbing.NewHash(h)}
}

func writeBlob(t *testing.T, s storer.EncodedObjectStorer, content []byte) plumbing.Hash {
	t.Helper()
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(content)))
	w, err := obj.Writer()
	if err != nil {
		t.Fatalf("blob writer: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close blob: %v", err)
	}
	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store blob: %v", err)
	}
	return h
}

// writeTree stores files as a tree, sorting entries the way git does so the
// hashes match trees built anywhere else from the same content.
func writeTree(t *testing.T, s storer.EncodedObjectStorer, files map[string]fileSpec) plumbing.Hash {
	t.Helper()
	leaves := map[string]fileSpec{}
	dirs := map[string]map[string]fileSpec{}
	for p, spec := range files {
		head, rest, nested := strings.Cut(p, "/")
		if !nested {
			leaves[p] = spec
			continue
		}
		if dirs[head] == nil {
			dirs[head] = map[string]fileSpec{}
		}
		dirs[head][rest] = spec
	}

	var entries []object.TreeEntry
	for name, spec := range leaves {
		h := spec.hash
		if h.IsZero() {
			h = writeBlob(t, s, spec.content)
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: spec.mode, Hash: h})
	}
	for name, sub := range dirs {
		entries = append(entries, object.TreeEntry{
			Name: name, Mode: filemode.Dir, Hash: writeTree(t, s, sub)})
	}
	sortKey := func(e object.TreeEntry) string {
		if e.Mode == filemode.Dir {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(entries, func(i, j int) bool { return sortKey(entries[i]) < sortKey(entries[j]) })

	tree := &object.Tree{Entries: entries}
	obj := s.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		t.Fatalf("encode tree: %v", err)
	}
	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store tree: %v", err)
	}
	return h
}

var clockTick atomic.Int64

func authorSig(name string) object.Signature {
	return object.Signature{
		Name:  name,
		Email: strings.ToLower(name) + "@example.com",
		When:  time.Unix(1600000000+clockTick.Add(1), 0).UTC(),
	}
}

func writeCommitObject(t *testing.T, s storer.EncodedObjectStorer, sig object.Signature,
	parents []plumbing.Hash, message string, tree plumbing.Hash) plumbing.Hash {
	t.Helper()
	c := &object.Commit{
		Author: sig, Committer: sig, Message: message,
		TreeHash: tree, ParentHashes: parents,
	}
	obj := s.NewEncodedObject()
	if err := c.Encode(obj); err != nil {
		t.Fatalf("encode commit: %v", err)
	}
	h, err := s.SetEncodedObject(obj)
	if err != nil {
		t.Fatalf("store commit: %v", err)
	}
	return h
}

func setBranch(t *testing.T, repo *git.Repository, branch string, h plumbing.Hash) {
	t.Helper()
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatalf("set %s: %v", branch, err)
	}
}

func branchHash(t *testing.T, repo *git.Repository, branch string) plumbing.Hash {
	t.Helper()
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		t.Fatalf("resolve %s: %v", branch, err)
	}
	return ref.Hash()
}

// flattenTree renders a tree as a path-to-spec map, keeping submodule entries
// as bare hashes so the fake can round-trip them.
func flattenTree(t *testing.T, repo *git.Repository, h plumbing.Hash, prefix string, out map[string]fileSpec) {
	t.Helper()
	tree, err := repo.TreeObject(h)
	if err != nil {
		t.Fatalf("read tree %s: %v", h, err)
	}
	for _, e := range tree.Entries {
		path := prefix + e.Name
		if e.Mode == filemode.Dir {
			flattenTree(t, repo, e.Hash, path+"/", out)
			continue
		}
		if e.Mode == filemode.Submodule {
			out[path] = fileSpec{mode: e.Mode, hash: e.Hash}
			continue
		}
		content, err := readBlob(repo, e.Hash)
		if err != nil {
			t.Fatalf("read blob %s: %v", e.Hash, err)
		}
		out[path] = fileSpec{mode: e.Mode, content: content}
	}
}

// -- fixture ---------------------------------------------------------------

type fixture struct {
	local     *git.Repository
	localPath string
	remote    *git.Repository
	remoteDir string
	seed      plumbing.Hash
	epPath    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()

	remoteDir := filepath.Join(tmp, "remote.git")
	remote, err := git.PlainInit(remoteDir, true)
	if err != nil {
		t.Fatalf("init remote: %v", err)
	}
	if err := remote.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set remote HEAD: %v", err)
	}
	epPath := "/someorg/" + strings.NewReplacer("/", "-", " ", "_").Replace(t.Name()) + ".git"
	entry := &served{dir: remoteDir}
	loaderMu.Lock()
	loaderBy[epPath] = entry
	loaderMu.Unlock()
	t.Cleanup(func() {
		loaderMu.Lock()
		delete(loaderBy, epPath)
		loaderMu.Unlock()
	})

	localPath := filepath.Join(tmp, "local")
	local, err := git.PlainInit(localPath, false)
	if err != nil {
		t.Fatalf("init local: %v", err)
	}
	if _, err := local.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{"https://github.com" + epPath},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	entry.client = local
	f := &fixture{local: local, localPath: localPath, remote: remote, remoteDir: remoteDir, epPath: epPath}
	f.seed = f.commit(t, "main", nil, "seed", map[string]fileSpec{"README.md": regular("seed\n")})
	if err := local.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	f.push(t, "main")
	return f
}

// commit writes a commit on the local branch with the full tree given by files.
func (f *fixture) commit(t *testing.T, branch string, parents []plumbing.Hash,
	message string, files map[string]fileSpec) plumbing.Hash {
	t.Helper()
	tree := writeTree(t, f.local.Storer, files)
	h := writeCommitObject(t, f.local.Storer, authorSig("Dev"), parents, message, tree)
	setBranch(t, f.local, branch, h)
	return h
}

func (f *fixture) commitAt(t *testing.T, branch string, parents []plumbing.Hash,
	message string, files map[string]fileSpec, when time.Time) plumbing.Hash {
	t.Helper()
	sig := authorSig("Dev")
	sig.When = when
	tree := writeTree(t, f.local.Storer, files)
	h := writeCommitObject(t, f.local.Storer, sig, parents, message, tree)
	setBranch(t, f.local, branch, h)
	return h
}

// remoteCommit writes a commit straight into the bare remote, as a third party
// (or an earlier interrupted replay) would have left it.
func (f *fixture) remoteCommit(t *testing.T, branch string, parents []plumbing.Hash,
	message string, files map[string]fileSpec) plumbing.Hash {
	t.Helper()
	tree := writeTree(t, f.remote.Storer, files)
	h := writeCommitObject(t, f.remote.Storer, botSig, parents, message, tree)
	setBranch(t, f.remote, branch, h)
	return h
}

func (f *fixture) push(t *testing.T, branch string) {
	t.Helper()
	spec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch))
	err := f.local.PushContext(context.Background(), &git.PushOptions{
		RemoteName: "origin", RefSpecs: []config.RefSpec{spec},
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		t.Fatalf("push %s: %v", branch, err)
	}
}

func (f *fixture) options(branch string) Options {
	return Options{RepoPath: f.localPath, Branch: branch, Base: "main", Remote: "origin",
		MaxCommitBytes: testMaxCommitBytes}
}

// unserve makes every network operation on the fixture remote fail, so a test
// can prove a check runs before the first fetch.
func (f *fixture) unserve() {
	loaderMu.Lock()
	delete(loaderBy, f.epPath)
	loaderMu.Unlock()
}

// -- fake client -----------------------------------------------------------

// testMaxCommitBytes is the ceiling the tool passes; replay has no default.
const testMaxCommitBytes int64 = 4_718_592

var botSig = object.Signature{
	Name: "Bot", Email: "bot@example.com", When: time.Unix(1500000000, 0).UTC(),
}

type call struct {
	kind     string
	branch   string
	expected string
	message  string
}

// fakeClient applies each mutation to the bare remote, so the tree comparison
// and ref move Push performs afterwards run against a real repository.
type fakeClient struct {
	t           *testing.T
	remote      *git.Repository
	calls       []call
	advanceAt   int
	advanceNow  bool
	corrupt     bool
	failAt      int
	failWith    error
	onCreate    func()
	afterCreate func()
	creations   int
	extraCount  int
}

func (f *fakeClient) mutations() int {
	n := 0
	for _, c := range f.calls {
		if c.kind == "create_commit" || c.kind == "create_branch" {
			n++
		}
	}
	return n
}

func (f *fakeClient) BranchHead(_ context.Context, _, _, branch string) (string, error) {
	f.calls = append(f.calls, call{kind: "branch_head", branch: branch})
	if f.advanceNow {
		f.advanceNow = false
		f.advanceRemote(branch)
	}
	ref, err := f.remote.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return "", nil
	}
	return ref.Hash().String(), nil
}

func (f *fakeClient) CreateBranch(_ context.Context, _, _, branch, fromOID string) (string, error) {
	f.calls = append(f.calls, call{kind: "create_branch", branch: branch, expected: fromOID})
	setBranch(f.t, f.remote, branch, plumbing.NewHash(fromOID))
	return fromOID, nil
}

func (f *fakeClient) CreateCommit(_ context.Context, _, _, branch, expectedHeadOID, message string,
	additions []FileAddition, deletions []string) (string, error) {
	f.calls = append(f.calls, call{
		kind: "create_commit", branch: branch, expected: expectedHeadOID, message: message})
	f.creations++
	if f.onCreate != nil {
		f.onCreate()
	}
	if f.failAt == f.creations {
		return "", f.failWith
	}
	if f.advanceAt == f.creations {
		f.advanceRemote(branch)
	}

	head := branchHash(f.t, f.remote, branch)
	if head.String() != expectedHeadOID {
		return "", &HeadMismatchError{Expected: expectedHeadOID,
			Message: "branch is at " + head.String()}
	}
	parent, err := f.remote.CommitObject(head)
	if err != nil {
		return "", err
	}
	files := map[string]fileSpec{}
	flattenTree(f.t, f.remote, parent.TreeHash, "", files)
	for _, d := range deletions {
		delete(files, d)
	}
	for _, a := range additions {
		content := a.Contents
		if f.corrupt {
			content = append(append([]byte{}, content...), []byte("corrupt\n")...)
		}
		spec := regularBytes(content)
		if prev, ok := files[a.Path]; ok {
			spec.mode = prev.mode
		}
		files[a.Path] = spec
	}
	tree := writeTree(f.t, f.remote.Storer, files)
	h := writeCommitObject(f.t, f.remote.Storer, botSig, []plumbing.Hash{head}, message, tree)
	setBranch(f.t, f.remote, branch, h)
	if f.afterCreate != nil {
		hook := f.afterCreate
		f.afterCreate = nil
		hook()
	}
	return h.String(), nil
}

// advanceRemote lands a foreign commit on the remote branch, as a third party
// pushing mid-replay would.
func (f *fakeClient) advanceRemote(branch string) {
	head := branchHash(f.t, f.remote, branch)
	parent, err := f.remote.CommitObject(head)
	if err != nil {
		f.t.Fatalf("read %s: %v", head, err)
	}
	files := map[string]fileSpec{}
	flattenTree(f.t, f.remote, parent.TreeHash, "", files)
	f.extraCount++
	files[fmt.Sprintf("foreign%d.txt", f.extraCount)] = regular("foreign\n")
	tree := writeTree(f.t, f.remote.Storer, files)
	h := writeCommitObject(f.t, f.remote.Storer, botSig, []plumbing.Hash{head}, "foreign commit", tree)
	setBranch(f.t, f.remote, branch, h)
}

func newFake(t *testing.T, f *fixture) *fakeClient {
	return &fakeClient{t: t, remote: f.remote}
}

// -- OwnerRepo -------------------------------------------------------------

func TestOwnerRepo(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		owner, repo string
		refuse      string
	}{
		{name: "scp form", url: "git@github.com:someorg/somerepo.git", owner: "someorg", repo: "somerepo"},
		{name: "ssh url", url: "ssh://git@github.com/someorg/somerepo.git", owner: "someorg", repo: "somerepo"},
		{name: "https url", url: "https://github.com/someorg/somerepo", owner: "someorg", repo: "somerepo"},
		{name: "non github scp", url: "git@gitlab.com:someorg/somerepo.git", refuse: "is not github.com"},
		{name: "lookalike host", url: "https://github.com.evil.com/someorg/somerepo.git", refuse: "is not github.com"},
		{name: "unsupported scheme", url: "git://github.com/someorg/somerepo.git", refuse: "unsupported scheme"},
		{name: "not owner repo", url: "https://github.com/someorg", refuse: "does not look like owner/repo"},
		{name: "traversal segments", url: "https://github.com/../..", refuse: "does not look like owner/repo"},
		{name: "space in owner", url: "https://github.com/a b/c", refuse: "does not look like owner/repo"},
		{name: "query string", url: "https://github.com/o/r?x=1", refuse: "query or fragment"},
		{name: "explicit port", url: "https://github.com:8443/o/r", refuse: "names a port"},
		{name: "https with password", url: "https://someuser:somepassword@github.com/o/r.git",
			refuse: "carries credentials"},
		{name: "https with username only", url: "https://ghs_sometoken@github.com/o/r.git",
			refuse: "carries credentials"},
		{name: "ssh url with another user", url: "ssh://mallory@github.com/o/r.git",
			refuse: "carries credentials"},
		{name: "ssh url with password", url: "ssh://git:somepassword@github.com/o/r.git",
			refuse: "carries credentials"},
		{name: "scp form with another user", url: "mallory@github.com:o/r.git",
			refuse: "carries credentials"},
		{name: "https with git as user", url: "https://git@github.com/o/r.git",
			refuse: "carries credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo, err := git.PlainInit(dir, false)
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			if _, err := repo.CreateRemote(&config.RemoteConfig{
				Name: "origin", URLs: []string{tc.url}}); err != nil {
				t.Fatalf("create remote: %v", err)
			}
			owner, name, err := OwnerRepo(dir, "origin")
			if tc.refuse != "" {
				var refused *RefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("want RefusedError, got %v", err)
				}
				if !strings.Contains(refused.Error(), tc.refuse) {
					t.Fatalf("want %q in %q", tc.refuse, refused.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("OwnerRepo: %v", err)
			}
			if owner != tc.owner || name != tc.repo {
				t.Fatalf("got %s/%s, want %s/%s", owner, name, tc.owner, tc.repo)
			}
		})
	}
}

func TestOwnerRepoRedactsCredentials(t *testing.T) {
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin",
		URLs: []string{"https://someuser:ghs_supersecrettoken@gitlab.com/someorg/somerepo.git"}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	_, _, err = OwnerRepo(dir, "origin")
	if err == nil {
		t.Fatal("want refusal")
	}
	if strings.Contains(err.Error(), "ghs_supersecrettoken") || strings.Contains(err.Error(), "someuser") {
		t.Fatalf("credentials leaked: %s", err)
	}
	if !strings.Contains(err.Error(), "gitlab.com") {
		t.Fatalf("want the host named: %s", err)
	}
}

func TestOwnerRepoRefusesGitHubURLWithUserinfo(t *testing.T) {
	const password = "ghs_supersecretpassword"
	for _, tc := range []struct{ name, url string }{
		{"https with password", "https://someuser:" + password + "@github.com/someorg/somerepo.git"},
		{"https with token as user", "https://" + password + "@github.com/someorg/somerepo.git"},
		{"ssh url with password", "ssh://git:" + password + "@github.com/someorg/somerepo.git"},
		{"scp form with token as user", password + "@github.com:someorg/somerepo.git"},
		{"scp form with password", "git:" + password + "@github.com:someorg/somerepo.git"},
		{"schemeless with password", "https:/someuser:" + password + "@github.com/someorg/somerepo.git"},
		{"opaque url with password", "https:x://someuser:" + password + "@github.com/someorg/somerepo.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			repo, err := git.PlainInit(dir, false)
			if err != nil {
				t.Fatalf("init: %v", err)
			}
			if _, err := repo.CreateRemote(&config.RemoteConfig{
				Name: "origin", URLs: []string{tc.url}}); err != nil {
				t.Fatalf("create remote: %v", err)
			}
			_, _, err = OwnerRepo(dir, "origin")
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("want RefusedError, got %v", err)
			}
			if strings.Contains(refused.Error(), password) || strings.Contains(refused.Error(), "someuser") {
				t.Fatalf("credentials leaked: %s", refused.Error())
			}
		})
	}
}

func TestPushEmptyRangeWithoutRemoteBranchIsRefused(t *testing.T) {
	f := newFixture(t)
	setBranch(t, f.local, "feature", f.seed)

	fake := newFake(t, f)
	_, err := Push(context.Background(), fake, f.options("feature"))
	var refused *RefusedError
	if !errors.As(err, &refused) || !strings.Contains(refused.Reason, "nothing to push") {
		t.Fatalf("want nothing-to-push refusal, got %v", err)
	}
	for _, c := range fake.calls {
		if c.kind == "create_branch" || c.kind == "create_commit" {
			t.Fatalf("want no mutation, got %+v", fake.calls)
		}
	}
	if branchHash(t, f.local, "feature") != f.seed {
		t.Fatal("local ref moved")
	}
	if _, err := f.remote.Reference(plumbing.NewBranchReferenceName("feature"), true); err == nil {
		t.Fatal("remote branch was created")
	}
}

func TestPushUsesHTTPSTransportForSCPFormOrigin(t *testing.T) {
	f := newFixture(t)
	if err := f.local.DeleteRemote("origin"); err != nil {
		t.Fatalf("delete remote: %v", err)
	}
	if _, err := f.local.CreateRemote(&config.RemoteConfig{
		Name: "origin", URLs: []string{"git@github.com:" + strings.TrimPrefix(f.epPath, "/")},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "one", map[string]fileSpec{"a.txt": regular("a")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push over scp-form origin: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c1.String() {
		t.Fatalf("pairs = %+v, want one pair for %s", res.Pairs, c1)
	}
}

func TestPushRefusesNonPositiveMaxCommitBytesBeforeFetching(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	// With the remote unserved every fetch fails, so only a check made before
	// the first one can produce the refusal.
	f.unserve()

	for _, max := range []int64{0, -1} {
		fake := newFake(t, f)
		o := f.options("feature")
		o.MaxCommitBytes = max
		_, err := Push(context.Background(), fake, o)
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("max %d: want RefusedError, got %v", max, err)
		}
		if !strings.Contains(refused.Reason, "MaxCommitBytes") {
			t.Fatalf("max %d: want the option named, got %q", max, refused.Reason)
		}
		if len(fake.calls) != 0 {
			t.Fatalf("max %d: want no client calls, got %+v", max, fake.calls)
		}
	}
}

func TestPushRejectsInvalidRefNames(t *testing.T) {
	f := newFixture(t)
	fake := newFake(t, f)
	for _, o := range []Options{
		{RepoPath: f.localPath, Branch: "--upload-pack=x", Base: "main", Remote: "origin"},
		{RepoPath: f.localPath, Branch: "feature", Base: "../etc", Remote: "origin"},
		{RepoPath: f.localPath, Branch: "feature", Base: "main", Remote: "-x"},
	} {
		_, err := Push(context.Background(), fake, o)
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("want RefusedError for %+v, got %v", o, err)
		}
		if !strings.Contains(refused.Reason, "invalid") {
			t.Fatalf("want the invalid name reported, got %q", refused.Reason)
		}
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
}

// -- range and replay ------------------------------------------------------

func TestPushTwoCommitsOldestFirst(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	c2 := f.commit(t, "feature", []plumbing.Hash{c1}, "add b\n\nbody",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 2 || res.Pairs[0].Local != c1.String() || res.Pairs[1].Local != c2.String() {
		t.Fatalf("want %s then %s, got %+v", c1, c2, res.Pairs)
	}
	for _, p := range res.Pairs {
		if p.Local == p.Remote {
			t.Fatalf("remote OID equals local OID: %+v", p)
		}
	}
	if res.Head != res.Pairs[1].Remote {
		t.Fatalf("head %s, want %s", res.Head, res.Pairs[1].Remote)
	}
	if got := branchHash(t, f.local, "feature").String(); got != res.Head {
		t.Fatalf("local ref %s, want %s", got, res.Head)
	}
	if got := branchHash(t, f.remote, "feature").String(); got != res.Head {
		t.Fatalf("remote ref %s, want %s", got, res.Head)
	}
	msgs := []string{}
	for _, c := range fake.calls {
		if c.kind == "create_commit" {
			msgs = append(msgs, c.message)
		}
	}
	if len(msgs) != 2 || msgs[0] != "add a" || msgs[1] != "add b\n\nbody" {
		t.Fatalf("messages %q", msgs)
	}
	head, err := f.local.Reference(plumbing.HEAD, false)
	if err != nil || head.Target() != plumbing.NewBranchReferenceName("main") {
		t.Fatalf("HEAD disturbed: %v %v", head, err)
	}
}

func TestPushRemoteAlreadyHasFirstCommit(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.push(t, "feature")
	c2 := f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c2.String() {
		t.Fatalf("want only %s, got %+v", c2, res.Pairs)
	}
}

func TestPushRefusesWhenLocalBehindRemote(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.push(t, "feature")
	f.remoteCommit(t, "feature", []plumbing.Hash{c1}, "someone else's work",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "z.txt": regular("z\n")})

	fake := newFake(t, f)
	_, err := Push(context.Background(), fake, f.options("feature"))
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want RefusedError, got %v", err)
	}
	if !strings.Contains(refused.Reason, "behind") {
		t.Fatalf("want 'behind' in %q", refused.Reason)
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
	if branchHash(t, f.local, "feature") != c1 {
		t.Fatal("local ref moved")
	}
}

func TestPushResumesAfterPartialReplay(t *testing.T) {
	f := newFixture(t)
	tree1 := map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")}
	tree2 := map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")}
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a", tree1)
	c2 := f.commit(t, "feature", []plumbing.Hash{c1}, "add b", tree2)

	r1 := f.remoteCommit(t, "feature", []plumbing.Hash{f.seed}, "add a", tree1)
	if r1 == c1 {
		t.Fatal("fixture bug: replayed commit must have a different OID")
	}

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 2 {
		t.Fatalf("want two pairs, got %+v", res.Pairs)
	}
	if res.Pairs[0] != (Pair{Local: c1.String(), Remote: r1.String()}) {
		t.Fatalf("commit 1 not adopted: %+v", res.Pairs[0])
	}
	if res.Pairs[1].Local != c2.String() {
		t.Fatalf("want %s replayed, got %s", c2, res.Pairs[1].Local)
	}
	if n := fake.creations; n != 1 {
		t.Fatalf("want one createCommit, got %d", n)
	}
	if got := branchHash(t, f.local, "feature").String(); got != res.Head {
		t.Fatalf("local ref %s, want %s", got, res.Head)
	}
}

func TestPushRefusesResumeMismatch(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})
	f.remoteCommit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("different\n")})

	fake := newFake(t, f)
	_, err := Push(context.Background(), fake, f.options("feature"))
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want RefusedError, got %v", err)
	}
	if !strings.Contains(refused.Reason, "behind") {
		t.Fatalf("want 'behind' in %q", refused.Reason)
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
}

// -- refusals --------------------------------------------------------------

func TestPushRefusals(t *testing.T) {
	base := map[string]fileSpec{"README.md": regular("seed\n"), "keep.txt": regular("k\n")}
	with := func(extra map[string]fileSpec) map[string]fileSpec {
		out := map[string]fileSpec{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	subHash := "1111111111111111111111111111111111111111"

	cases := []struct {
		name   string
		build  func(t *testing.T, f *fixture)
		reason string
		max    int64
	}{
		{
			name: "executable addition",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "add script",
					with(map[string]fileSpec{"run.sh": executable("#!/bin/sh\n")}))
			},
			reason: "executable bit",
		},
		{
			name: "mode change to executable",
			build: func(t *testing.T, f *fixture) {
				c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add script",
					with(map[string]fileSpec{"run.sh": regular("#!/bin/sh\n")}))
				f.commit(t, "feature", []plumbing.Hash{c1}, "chmod",
					with(map[string]fileSpec{"run.sh": executable("#!/bin/sh\n")}))
			},
			reason: "mode change",
		},
		{
			name: "symlink",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "add link",
					with(map[string]fileSpec{"link.txt": symlink("keep.txt")}))
			},
			reason: "not a regular file",
		},
		{
			name: "submodule addition",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "add submodule",
					with(map[string]fileSpec{"vendor/dep": submodule(subHash)}))
			},
			reason: "submodule",
		},
		{
			name: "submodule deletion",
			build: func(t *testing.T, f *fixture) {
				c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add submodule",
					with(map[string]fileSpec{"vendor/dep": submodule(subHash)}))
				f.commit(t, "feature", []plumbing.Hash{c1}, "drop submodule", with(nil))
			},
			reason: "submodule",
		},
		{
			name: "merge commit",
			build: func(t *testing.T, f *fixture) {
				left := f.commit(t, "left", []plumbing.Hash{f.seed}, "left",
					with(map[string]fileSpec{"l.txt": regular("l\n")}))
				right := f.commit(t, "right", []plumbing.Hash{f.seed}, "right",
					with(map[string]fileSpec{"r.txt": regular("r\n")}))
				f.commit(t, "feature", []plumbing.Hash{left, right}, "merge",
					with(map[string]fileSpec{"l.txt": regular("l\n"), "r.txt": regular("r\n")}))
			},
			reason: "merge commit",
		},
		{
			name: "empty commit",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "empty", with(nil))
			},
			reason: "empty",
		},
		{
			name: "oversize commit",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "add big",
					with(map[string]fileSpec{"big.bin": regularBytes(make([]byte, 4096))}))
			},
			reason: "exceeds MaxCommitBytes",
			max:    1024,
		},
		{
			name: "dot git path",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "sneak into .git",
					with(map[string]fileSpec{".GIT/x": regular("bad\n")}))
			},
			reason: ".GIT",
		},
		{
			name: "dot dot path",
			build: func(t *testing.T, f *fixture) {
				f.commit(t, "feature", []plumbing.Hash{f.seed}, "escape",
					with(map[string]fileSpec{"dir/../../etc/passwd": regular("bad\n")}))
			},
			reason: "..",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c0 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "seed feature", base)
			f.seed = c0
			tc.build(t, f)
			before := branchHash(t, f.local, "feature")

			fake := newFake(t, f)
			o := f.options("feature")
			if tc.max > 0 {
				o.MaxCommitBytes = tc.max
			}
			_, err := Push(context.Background(), fake, o)

			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("want RefusedError, got %v", err)
			}
			if !strings.Contains(refused.Error(), tc.reason) {
				t.Fatalf("want %q in %q", tc.reason, refused.Error())
			}
			if fake.mutations() != 0 {
				t.Fatalf("want zero mutations, got %d", fake.mutations())
			}
			if branchHash(t, f.local, "feature") != before {
				t.Fatal("local ref moved")
			}
		})
	}
}

func TestPushAcceptsDeletionOfExecutable(t *testing.T) {
	f := newFixture(t)
	base := map[string]fileSpec{"README.md": regular("seed\n"), "run.sh": executable("#!/bin/sh\n")}
	main := f.commit(t, "main", []plumbing.Hash{f.seed}, "add script", base)
	f.push(t, "main")

	c1 := f.commit(t, "feature", []plumbing.Hash{main}, "drop script",
		map[string]fileSpec{"README.md": regular("seed\n")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c1.String() {
		t.Fatalf("pairs %+v", res.Pairs)
	}
	files := map[string]fileSpec{}
	head, err := f.remote.CommitObject(plumbing.NewHash(res.Head))
	if err != nil {
		t.Fatalf("read remote head: %v", err)
	}
	flattenTree(t, f.remote, head.TreeHash, "", files)
	if _, ok := files["run.sh"]; ok {
		t.Fatal("run.sh still present on the remote")
	}
}

func TestPushDeletionAndNonUTF8BlobRoundTrip(t *testing.T) {
	f := newFixture(t)
	payload := []byte{0xff, 0xfe, 0x00, 0x01, 'b', 'i', 'n'}
	main := f.commit(t, "main", []plumbing.Hash{f.seed}, "add gone",
		map[string]fileSpec{"README.md": regular("seed\n"), "gone.txt": regular("gone\n")})
	f.push(t, "main")
	f.commit(t, "feature", []plumbing.Hash{main}, "binary in, text out",
		map[string]fileSpec{"README.md": regular("seed\n"), "bin.dat": regularBytes(payload)})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}

	files := map[string]fileSpec{}
	head, err := f.remote.CommitObject(plumbing.NewHash(res.Head))
	if err != nil {
		t.Fatalf("read remote head: %v", err)
	}
	flattenTree(t, f.remote, head.TreeHash, "", files)
	if _, ok := files["gone.txt"]; ok {
		t.Fatal("deleted file survived, so it was not sent as a deletion")
	}
	if got := files["bin.dat"].content; string(got) != string(payload) {
		t.Fatalf("blob bytes %v, want %v", got, payload)
	}
}

// -- branch creation, mismatch, re-run, tree check -------------------------

func TestPushCreatesRemoteBranchAtMergeBase(t *testing.T) {
	f := newFixture(t)
	forkPoint := f.seed
	f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.remoteCommit(t, "main", []plumbing.Hash{f.seed}, "main moves",
		map[string]fileSpec{"README.md": regular("seed\n"), "m.txt": regular("m\n")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	var created []call
	for _, c := range fake.calls {
		if c.kind == "create_branch" {
			created = append(created, c)
		}
	}
	if len(created) != 1 {
		t.Fatalf("want one createBranch, got %d", len(created))
	}
	if created[0].expected != forkPoint.String() {
		t.Fatalf("branch created at %s, want the merge base %s", created[0].expected, forkPoint)
	}
	if got := branchHash(t, f.local, "feature").String(); got != res.Head {
		t.Fatalf("local ref %s, want %s", got, res.Head)
	}
}

func TestPushHeadMismatchOnSecondCommitReportsFirstPair(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	c2 := f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})
	before := branchHash(t, f.local, "feature")

	fake := newFake(t, f)
	fake.advanceAt = 2
	res, err := Push(context.Background(), fake, f.options("feature"))

	var sync *SyncError
	if !errors.As(err, &sync) {
		t.Fatalf("want SyncError, got %v", err)
	}
	if len(sync.Replayed) != 1 || sync.Replayed[0].Local != c1.String() {
		t.Fatalf("want commit 1 replayed, got %+v", sync.Replayed)
	}
	if len(res.Pairs) != 1 || res.Pairs[0] != sync.Replayed[0] {
		t.Fatalf("result pairs %+v", res.Pairs)
	}
	if !strings.Contains(sync.Error(), sync.Replayed[0].Remote) {
		t.Fatalf("remote OID missing from %q", sync.Error())
	}
	if !strings.Contains(sync.Reason, c2.String()) {
		t.Fatalf("want the stalled commit named in %q", sync.Reason)
	}
	if branchHash(t, f.local, "feature") != before {
		t.Fatal("local ref moved")
	}
}

func TestPushWrapsOtherClientErrorsWithPairsSoFar(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})

	boom := errors.New("network exploded")
	fake := newFake(t, f)
	fake.failAt = 2
	fake.failWith = boom

	res, err := Push(context.Background(), fake, f.options("feature"))
	if !errors.Is(err, boom) {
		t.Fatalf("want the client error wrapped, got %v", err)
	}
	var sync *SyncError
	if errors.As(err, &sync) {
		t.Fatalf("client errors must not become SyncError: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c1.String() {
		t.Fatalf("want commit 1 in the pairs, got %+v", res.Pairs)
	}
}

func TestPushRerunAfterSuccessSendsZeroMutations(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})

	fake := newFake(t, f)
	first, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	before := fake.mutations()

	second, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(second.Pairs) != 0 {
		t.Fatalf("want no pairs, got %+v", second.Pairs)
	}
	if second.Head != first.Head {
		t.Fatalf("head %s, want %s", second.Head, first.Head)
	}
	if fake.mutations() != before {
		t.Fatalf("re-run sent %d extra mutations", fake.mutations()-before)
	}
}

func TestPushTreeMismatchLeavesLocalRefAlone(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})

	fake := newFake(t, f)
	fake.corrupt = true
	res, err := Push(context.Background(), fake, f.options("feature"))

	var sync *SyncError
	if !errors.As(err, &sync) {
		t.Fatalf("want SyncError, got %v", err)
	}
	if len(sync.Replayed) != 1 || sync.Replayed[0].Local != c1.String() {
		t.Fatalf("want the pair reported, got %+v", sync.Replayed)
	}
	if len(res.Pairs) != 1 {
		t.Fatalf("result pairs %+v", res.Pairs)
	}
	if branchHash(t, f.local, "feature") != c1 {
		t.Fatal("local ref moved despite the tree mismatch")
	}
}

func TestPushRefusesWhenRemoteMovedBeforeFirstMutation(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.push(t, "feature")
	f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})

	fake := newFake(t, f)
	fake.advanceNow = true

	_, err := Push(context.Background(), fake, f.options("feature"))
	var sync *SyncError
	if !errors.As(err, &sync) {
		t.Fatalf("want SyncError, got %v", err)
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
}

func TestPushRefusesWhenLocalRefMovedDuringReplay(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})

	fake := newFake(t, f)
	var moved plumbing.Hash
	fake.onCreate = func() {
		fake.onCreate = nil
		moved = f.commit(t, "feature", []plumbing.Hash{c1}, "landed mid-replay",
			map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "late.txt": regular("late\n")})
	}

	res, err := Push(context.Background(), fake, f.options("feature"))
	var sync *SyncError
	if !errors.As(err, &sync) {
		t.Fatalf("want SyncError, got %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c1.String() {
		t.Fatalf("want the replayed pair reported, got %+v", res.Pairs)
	}
	if branchHash(t, f.local, "feature") != moved {
		t.Fatal("local ref was moved despite the concurrent commit")
	}
}

func TestPushRefusesModeChangeFromExecutable(t *testing.T) {
	f := newFixture(t)
	main := f.commit(t, "main", []plumbing.Hash{f.seed}, "add script",
		map[string]fileSpec{"README.md": regular("seed\n"), "run.sh": executable("#!/bin/sh\n")})
	f.push(t, "main")
	f.commit(t, "feature", []plumbing.Hash{main}, "chmod -x and edit",
		map[string]fileSpec{"README.md": regular("seed\n"), "run.sh": regular("#!/bin/sh\nedited\n")})

	fake := newFake(t, f)
	_, err := Push(context.Background(), fake, f.options("feature"))
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want RefusedError, got %v", err)
	}
	if !strings.Contains(refused.Reason, "mode change") || refused.Path != "run.sh" {
		t.Fatalf("want the mode change named for run.sh, got %q", refused.Error())
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
}

func TestPushKeepsModeWhenEditingExecutable(t *testing.T) {
	f := newFixture(t)
	main := f.commit(t, "main", []plumbing.Hash{f.seed}, "add script",
		map[string]fileSpec{"README.md": regular("seed\n"), "run.sh": executable("#!/bin/sh\n")})
	f.push(t, "main")
	c1 := f.commit(t, "feature", []plumbing.Hash{main}, "edit script",
		map[string]fileSpec{"README.md": regular("seed\n"), "run.sh": executable("#!/bin/sh\nedited\n")})

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c1.String() {
		t.Fatalf("pairs = %+v, want one pair for %s", res.Pairs, c1)
	}
	files := map[string]fileSpec{}
	remoteTip, err := f.remote.CommitObject(branchHash(t, f.remote, "feature"))
	if err != nil {
		t.Fatal(err)
	}
	flattenTree(t, f.remote, remoteTip.TreeHash, "", files)
	if files["run.sh"].mode != filemode.Executable {
		t.Fatalf("remote run.sh mode = %v, want executable kept", files["run.sh"].mode)
	}
}

// writeTree must hash the way git does, or the tree comparisons the resume and
// post-replay checks rely on would only agree with themselves.
func TestWriteTreeMatchesGitObjectHashes(t *testing.T) {
	repo, err := git.PlainInit(t.TempDir(), true)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := writeBlob(t, repo.Storer, []byte("seed\n")).String(); got != "e31de1f3a235fd5e8f97207b8e43cd2aa06a6417" {
		t.Fatalf("blob hash %s", got)
	}
	sub := writeTree(t, repo.Storer, map[string]fileSpec{"b.txt": regular("b\n")})
	if sub.String() != "f8f7aefc2900a3d737cea9eee45729fd55761e1a" {
		t.Fatalf("subtree hash %s", sub)
	}
	root := writeTree(t, repo.Storer, map[string]fileSpec{
		"README.md": regular("seed\n"), "dir/b.txt": regular("b\n")})
	if root.String() != "3fb27db6db7402d3217ada47c8a419e2e7f3c1b8" {
		t.Fatalf("root tree hash %s", root)
	}
	// git orders a tree entry as if it ended in "/", so "a.txt" sorts before "a/"
	collide := writeTree(t, repo.Storer, map[string]fileSpec{
		"a.txt": regular("y\n"), "a/f": regular("x\n")})
	if collide.String() != "5fd4a545766c36092103f88d565718e4fb42e2ac" {
		t.Fatalf("directory-sort tree hash %s", collide)
	}
}

func TestPushRefusesNestedDotGitPath(t *testing.T) {
	f := newFixture(t)
	f.commit(t, "feature", []plumbing.Hash{f.seed}, "sneak into a nested .git",
		map[string]fileSpec{"README.md": regular("seed\n"), "docs/.git/hooks/post-checkout": regular("bad\n")})

	fake := newFake(t, f)
	_, err := Push(context.Background(), fake, f.options("feature"))
	var refused *RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want RefusedError, got %v", err)
	}
	if !strings.Contains(refused.Error(), ".git") {
		t.Fatalf("want the .git segment named, got %q", refused.Error())
	}
	if fake.mutations() != 0 {
		t.Fatalf("want zero mutations, got %d", fake.mutations())
	}
}

func TestPushOrdersTopologicallyNotByTimestamp(t *testing.T) {
	f := newFixture(t)
	c1 := f.commitAt(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")},
		time.Unix(1700000000, 0).UTC())
	c2 := f.commitAt(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")},
		time.Unix(1600000000, 0).UTC())

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 2 || res.Pairs[0].Local != c1.String() || res.Pairs[1].Local != c2.String() {
		t.Fatalf("want parent before child, got %+v", res.Pairs)
	}
}

func TestPushLeavesCheckedOutBranchIndexAlone(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	if err := f.local.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("feature"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	head, err := f.local.Reference(plumbing.HEAD, false)
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	if head.Type() != plumbing.SymbolicReference || head.Target() != plumbing.NewBranchReferenceName("feature") {
		t.Fatalf("HEAD is no longer a symref to feature: %v", head)
	}
	resolved, err := f.local.Head()
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	if resolved.Hash().String() != res.Head || resolved.Hash() == c1 {
		t.Fatalf("HEAD resolves to %s, want %s", resolved.Hash(), res.Head)
	}
}

func TestPushRefusesWhenFetchedHeadIsNotTheReplayedHead(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	tree := map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")}

	fake := newFake(t, f)
	fake.afterCreate = func() {
		f.remoteCommit(t, "feature", []plumbing.Hash{f.seed}, "rewritten", tree)
	}

	res, err := Push(context.Background(), fake, f.options("feature"))
	var sync *SyncError
	if !errors.As(err, &sync) {
		t.Fatalf("want SyncError, got %v", err)
	}
	if len(res.Pairs) != 1 {
		t.Fatalf("want the pair reported, got %+v", res.Pairs)
	}
	if branchHash(t, f.local, "feature") != c1 {
		t.Fatal("local ref moved onto a commit the replay did not produce")
	}
}

func TestPushWorksWhenBaseBranchIsGone(t *testing.T) {
	f := newFixture(t)
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")})
	f.push(t, "feature")
	c2 := f.commit(t, "feature", []plumbing.Hash{c1}, "add b",
		map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")})

	if err := f.remote.Storer.SetReference(plumbing.NewSymbolicReference(
		plumbing.HEAD, plumbing.NewBranchReferenceName("feature"))); err != nil {
		t.Fatalf("set remote HEAD: %v", err)
	}
	if err := f.remote.Storer.RemoveReference(plumbing.NewBranchReferenceName("main")); err != nil {
		t.Fatalf("remove main: %v", err)
	}

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 1 || res.Pairs[0].Local != c2.String() {
		t.Fatalf("pairs %+v", res.Pairs)
	}
}

func TestPushResumesWhenTheMessageHasNoBlankLine(t *testing.T) {
	f := newFixture(t)
	tree1 := map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n")}
	tree2 := map[string]fileSpec{"README.md": regular("seed\n"), "a.txt": regular("a\n"), "b.txt": regular("b\n")}
	c1 := f.commit(t, "feature", []plumbing.Hash{f.seed}, "add a\nbody without a blank line", tree1)
	f.commit(t, "feature", []plumbing.Hash{c1}, "add b", tree2)
	// createCommitOnBranch takes a headline and a body and rejoins them
	r1 := f.remoteCommit(t, "feature", []plumbing.Hash{f.seed}, "add a\n\nbody without a blank line", tree1)

	fake := newFake(t, f)
	res, err := Push(context.Background(), fake, f.options("feature"))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Pairs) != 2 || res.Pairs[0].Remote != r1.String() {
		t.Fatalf("want commit 1 adopted, got %+v", res.Pairs)
	}
	if fake.creations != 1 {
		t.Fatalf("want one createCommit, got %d", fake.creations)
	}
}
