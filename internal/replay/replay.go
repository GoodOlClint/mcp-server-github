// Package replay replays local commits onto GitHub through the Git Data API
// and then moves the local branch ref onto the returned OIDs. See docs/design.md
// and docs/decisions/0001, 0003 and 0006.
package replay

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// RefusedError reports a commit or remote the replay will not send. Nothing
// has reached the remote branch when it is returned.
type RefusedError struct {
	OID    string
	Path   string
	Reason string
}

func (e *RefusedError) Error() string {
	var b strings.Builder
	b.WriteString("refused")
	if e.OID != "" {
		fmt.Fprintf(&b, " commit %s", e.OID)
	}
	if e.Path != "" {
		fmt.Fprintf(&b, " path %q", e.Path)
	}
	b.WriteString(": ")
	b.WriteString(e.Reason)
	return b.String()
}

// Pair maps a local commit OID to the remote OID it was replayed as.
type Pair struct {
	Local  string
	Remote string
}

// SyncError reports that the local and remote tips disagree. Refs are left
// untouched; Replayed holds the pairs already sent.
type SyncError struct {
	Reason   string
	Replayed []Pair
}

func (e *SyncError) Error() string {
	if len(e.Replayed) == 0 {
		return "sync error: " + e.Reason
	}
	parts := make([]string, 0, len(e.Replayed))
	for _, p := range e.Replayed {
		parts = append(parts, p.Local+"->"+p.Remote)
	}
	return "sync error: " + e.Reason + "; already replayed: [" + strings.Join(parts, ", ") + "]"
}

// Result is the outcome of a Push. Pairs is populated once the commits are on
// the remote branch, so it is non-empty with an error only when the local ref
// sync that follows failed; Head is set only when the replay completed and the
// local ref was moved.
type Result struct {
	Pairs []Pair
	Head  string
}

// Options configures Push. Auth is passed to every go-git network operation;
// callers use &http.BasicAuth{Username: "x-access-token", Password: token}.
// MaxCommitBytes has no default here: the ceiling is owned by the caller and a
// non-positive value is an argument refusal.
type Options struct {
	RepoPath       string
	Branch         string
	Base           string
	Remote         string
	MaxCommitBytes int64
	Auth           transport.AuthMethod
}

var refNameRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

func validateRefName(name, label string) error {
	if !refNameRe.MatchString(name) || strings.HasPrefix(name, "-") || strings.Contains(name, "..") {
		return &RefusedError{Reason: fmt.Sprintf("invalid %s %q", label, name)}
	}
	return nil
}

// redactURL removes any userinfo from a remote URL so credentials never reach
// an error message.
func redactURL(raw string) string {
	if !strings.Contains(raw, "://") {
		if m := scpRe.FindStringSubmatch(raw); m != nil && m[1] == sshUser {
			return raw
		}
		// Anything before the last @ is userinfo in every form that reaches
		// here, including the ones scpRe rejects for holding a password.
		if i := strings.LastIndex(raw, "@"); i >= 0 {
			return "<redacted>@" + raw[i+1:]
		}
		return raw
	}
	u, err := url.Parse(raw)
	// An opaque url keeps its userinfo inside Opaque, where clearing User
	// cannot reach it.
	if err != nil || u.Opaque != "" {
		return "<unparseable url>"
	}
	u.User = nil
	return u.String()
}

var scpRe = regexp.MustCompile(`^([A-Za-z0-9._-]+)@([^:/]+):(.+)$`)

// OwnerRepo resolves owner and repo from the configured URL of remote. Only
// github.com over the ssh scp form, ssh:// or https:// is accepted.
func OwnerRepo(repoPath, remote string) (string, string, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return "", "", fmt.Errorf("open %s: %w", repoPath, err)
	}
	owner, name, _, err := ownerRepoURL(repo, remote)
	return owner, name, err
}

// ownerRepoURL reads the remote config exactly once and returns the URL it
// validated, so no later operation can be pointed elsewhere by a concurrent
// rewrite of .git/config.
func ownerRepoURL(repo *git.Repository, remote string) (string, string, string, error) {
	if err := validateRefName(remote, "remote"); err != nil {
		return "", "", "", err
	}
	rem, err := repo.Remote(remote)
	if err != nil {
		return "", "", "", fmt.Errorf("remote %s: %w", remote, err)
	}
	urls := rem.Config().URLs
	if len(urls) == 0 {
		return "", "", "", &RefusedError{Reason: "remote " + remote + " has no url"}
	}
	owner, name, err := parseRemoteURL(remote, urls[0])
	if err != nil {
		return "", "", "", err
	}
	return owner, name, "https://github.com/" + owner + "/" + name + ".git", nil
}

var repoSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// sshUser is the only userinfo github.com accepts over ssh; any other userinfo
// is a credential the caller must not put in a remote url.
const sshUser = "git"

func parseRemoteURL(remote, raw string) (string, string, error) {
	safe := redactURL(raw)
	refuse := func(reason string) (string, string, error) {
		return "", "", &RefusedError{Reason: fmt.Sprintf("remote %s url %q %s", remote, safe, reason)}
	}

	var host, path string
	if !strings.Contains(raw, "://") {
		m := scpRe.FindStringSubmatch(raw)
		if m == nil {
			return refuse("is not a recognised git url")
		}
		if m[1] != sshUser {
			return refuse("carries credentials")
		}
		host, path = m[2], m[3]
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return refuse("is not a recognised git url")
		}
		if u.Scheme != "ssh" && u.Scheme != "https" {
			return refuse("uses unsupported scheme " + u.Scheme)
		}
		if u.User != nil {
			_, hasPassword := u.User.Password()
			if u.Scheme != "ssh" || hasPassword || u.User.Username() != sshUser {
				return refuse("carries credentials")
			}
		}
		if u.RawQuery != "" || u.Fragment != "" {
			// go-git appends both to the endpoint path, so the string this
			// function validates would not be the string it requests.
			return refuse("carries a query or fragment")
		}
		if u.Port() != "" {
			return refuse("names a port")
		}
		host, path = u.Hostname(), u.Path
	}

	if !strings.EqualFold(host, "github.com") {
		return refuse("is not github.com")
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	segments := strings.Split(path, "/")
	if len(segments) != 2 {
		return refuse("does not look like owner/repo")
	}
	for _, seg := range segments {
		if !repoSegmentRe.MatchString(seg) || seg == "." || seg == ".." {
			return refuse("does not look like owner/repo")
		}
	}
	return segments[0], segments[1], nil
}

func (o Options) normalise() Options {
	if o.Base == "" {
		o.Base = "main"
	}
	if o.Remote == "" {
		o.Remote = "origin"
	}
	return o
}

// Push replays the commits the remote branch lacks onto GitHub over the Git
// Data API, then moves refs/heads/<branch> onto the remote OIDs. Nothing
// reaches the remote branch until the single ref update at the end, so every
// failure before it leaves the branch where it was.
func Push(ctx context.Context, c Client, o Options) (Result, error) {
	o = o.normalise()
	for _, v := range []struct{ name, label string }{
		{o.Branch, "branch"}, {o.Base, "base"}, {o.Remote, "remote"},
	} {
		if err := validateRefName(v.name, v.label); err != nil {
			return Result{}, err
		}
	}
	if o.MaxCommitBytes <= 0 {
		return Result{}, &RefusedError{Reason: fmt.Sprintf(
			"MaxCommitBytes must be positive, got %d", o.MaxCommitBytes)}
	}

	repo, err := git.PlainOpen(o.RepoPath)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", o.RepoPath, err)
	}
	owner, repoName, remoteURL, err := ownerRepoURL(repo, o.Remote)
	if err != nil {
		return Result{}, err
	}

	remoteHasBranch, remoteHasBase, err := fetchRefs(ctx, repo, o, remoteURL)
	if err != nil {
		return Result{}, err
	}

	localTip, err := repo.Reference(plumbing.NewBranchReferenceName(o.Branch), true)
	if err != nil {
		return Result{}, fmt.Errorf("resolve refs/heads/%s: %w", o.Branch, err)
	}

	var local, remoteOnly []*object.Commit
	var tracked *plumbing.Reference
	if remoteHasBranch || remoteHasBase {
		name := plumbing.NewRemoteReferenceName(o.Remote, o.Base)
		if remoteHasBranch {
			name = plumbing.NewRemoteReferenceName(o.Remote, o.Branch)
		}
		tracked, err = repo.Reference(name, true)
		if err != nil {
			return Result{}, fmt.Errorf("resolve %s: %w", name, err)
		}
		local, remoteOnly, err = diverged(repo, tracked.Hash(), localTip.Hash())
		if err != nil {
			return Result{}, err
		}
		if !remoteHasBranch {
			// remoteOnly here is base-only history, which is not a replay concern.
			remoteOnly = nil
		}
	} else {
		// The remote holds neither the branch nor the base, so nothing of this
		// history is there: replay the branch from its root.
		tip, err := repo.CommitObject(localTip.Hash())
		if err != nil {
			return Result{}, fmt.Errorf("read %s: %w", localTip.Hash(), err)
		}
		local, err = walkUntil(repo, tip, nil)
		if err != nil {
			return Result{}, err
		}
	}

	adopted, pending, err := matchReplayed(local, remoteOnly)
	if err != nil {
		return Result{}, err
	}

	if len(pending) == 0 && len(adopted) == 0 {
		// Nothing to replay. When the remote lacks the branch there is nothing
		// to place on it either, so no branch is created.
		if remoteHasBranch {
			return Result{Pairs: nil, Head: tracked.Hash().String()}, nil
		}
		return Result{}, &RefusedError{Reason: fmt.Sprintf(
			"nothing to push: %s has no commits beyond %s/%s and does not exist on the remote", o.Branch, o.Remote, o.Base)}
	}

	changes := make([]*commitChange, 0, len(pending))
	for _, cm := range pending {
		ch, err := changesFor(ctx, repo, cm, o.MaxCommitBytes)
		if err != nil {
			return Result{}, err
		}
		changes = append(changes, ch)
	}

	head, err := c.BranchHead(ctx, owner, repoName, o.Branch)
	if err != nil {
		return Result{}, fmt.Errorf("branch head: %w", err)
	}
	switch {
	case head == "" && remoteHasBranch:
		return Result{}, &SyncError{Reason: fmt.Sprintf(
			"%s/%s exists locally as %s but not on GitHub", o.Remote, o.Branch, tracked.Hash())}
	case head == "":
		// The branch is new to the remote; it is created at the replayed tip.
	case !remoteHasBranch:
		return Result{}, &SyncError{Reason: fmt.Sprintf(
			"%s/%s appeared on GitHub at %s after the fetch; nothing was sent, re-run to recompute the range",
			o.Remote, o.Branch, head)}
	case head != tracked.Hash().String():
		return Result{}, &SyncError{Reason: fmt.Sprintf(
			"%s/%s is at %s, not the fetched %s; nothing was sent, re-run to recompute the range",
			o.Remote, o.Branch, head, tracked.Hash())}
	}

	pairs := make([]Pair, 0, len(adopted)+len(changes))
	pairs = append(pairs, adopted...)
	remoteHead := head

	if len(changes) > 0 {
		// remote maps a local commit OID to the OID it was replayed as, so a
		// merge parent inside the range resolves to the commit just built.
		remote := make(map[plumbing.Hash]string, len(adopted)+len(changes))
		for i, p := range adopted {
			remote[plumbing.NewHash(p.Local)] = adopted[i].Remote
		}
		// treeOf saves a round trip for a parent this run created.
		treeOf := make(map[string]string, len(changes))

		if err := uploadBlobs(ctx, c, repo, owner, repoName, changes); err != nil {
			return Result{}, err
		}

		created := make([]Pair, 0, len(changes))
		var last string
		for _, ch := range changes {
			parents, err := remoteParents(ctx, c, owner, repoName, ch, remote)
			if err != nil {
				return Result{}, err
			}
			baseTree := ""
			if len(parents) > 0 {
				baseTree = treeOf[parents[0]]
				if baseTree == "" {
					if baseTree, err = c.CommitTree(ctx, owner, repoName, parents[0]); err != nil {
						return Result{}, fmt.Errorf("tree of %s: %w", parents[0], err)
					}
					treeOf[parents[0]] = baseTree
				}
			}
			tree, err := c.CreateTree(ctx, owner, repoName, baseTree, treeEntries(ch))
			if err != nil {
				return Result{}, fmt.Errorf("create tree for %s: %w", ch.oid, err)
			}
			// base_tree is the local first parent's tree and the entries carry
			// local blob OIDs, so the result must be the tree the local commit
			// names. Checking here catches a divergence before the ref moves;
			// the tip check in syncLocalRef would only catch it afterwards.
			if tree != ch.tree.String() {
				return Result{}, fmt.Errorf(
					"tree GitHub built for %s is %s, not the local %s; nothing was published",
					ch.oid, tree, ch.tree)
			}
			newOID, err := c.CreateCommit(ctx, owner, repoName, ch.message, tree, parents)
			if err != nil {
				return Result{}, fmt.Errorf("create commit %s: %w", ch.oid, err)
			}
			created = append(created, Pair{Local: ch.oid, Remote: newOID})
			remote[plumbing.NewHash(ch.oid)] = newOID
			treeOf[newOID] = tree
			last = newOID
		}

		// One ref call publishes the whole range. Creating a new branch at the
		// tip rather than at a fork point keeps that true: a failure here
		// leaves no branch behind for a caller to open a PR from.
		publish := c.UpdateRef
		if head == "" {
			publish = c.CreateRef
		}
		if err := publish(ctx, owner, repoName, o.Branch, last); err != nil {
			var mismatch *HeadMismatchError
			if errors.As(err, &mismatch) {
				return Result{}, &SyncError{Reason: fmt.Sprintf(
					"remote %s/%s moved before the ref update (%s); nothing landed on the branch. The commits built for this push (%s) are unreachable and GitHub will collect them; re-run to replay onto the new head",
					o.Remote, o.Branch, mismatch.Message, joinRemotes(created))}
			}
			return Result{}, fmt.Errorf("point %s at %s: %w", o.Branch, last, err)
		}
		pairs = append(pairs, created...)
		remoteHead = last
	}

	finalHead, err := syncLocalRef(ctx, repo, o, remoteURL, localTip.Hash(), remoteHead)
	if err != nil {
		var sync *SyncError
		if errors.As(err, &sync) {
			sync.Replayed = pairs
			return Result{Pairs: pairs}, sync
		}
		return Result{Pairs: pairs}, err
	}
	return Result{Pairs: pairs, Head: finalHead}, nil
}

// pinnedRemote builds a remote bound to the canonical https URL for the
// validated owner/repo rather than re-reading .git/config, which go-git parses
// afresh on every access. Transport is always https because Auth is an
// installation token, whatever scheme the configured remote uses.
func pinnedRemote(repo *git.Repository, name, url string) *git.Remote {
	return git.NewRemote(repo.Storer, &config.RemoteConfig{Name: name, URLs: []string{url}})
}

// fetchRefs brings the base and, when it exists on the remote, the branch into
// refs/remotes/<remote>/. It reports whether the remote has the branch and
// whether it has the base.
func fetchRefs(ctx context.Context, repo *git.Repository, o Options, remoteURL string) (bool, bool, error) {
	rem := pinnedRemote(repo, o.Remote, remoteURL)
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: o.Auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		// A repository with no refs at all: the whole branch is the range.
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("list %s: %w", o.Remote, err)
	}
	branchRef := plumbing.NewBranchReferenceName(o.Branch)
	baseRef := plumbing.NewBranchReferenceName(o.Base)
	var hasBranch, hasBase bool
	for _, r := range refs {
		switch r.Name() {
		case branchRef:
			hasBranch = true
		case baseRef:
			hasBase = true
		}
	}
	// With neither ref present there is nothing to fetch: the remote has no
	// history for this branch, so the range is the branch's whole history.
	var spec config.RefSpec
	switch {
	case hasBranch:
		spec = config.RefSpec(fmt.Sprintf(
			"+refs/heads/%s:refs/remotes/%s/%s", o.Branch, o.Remote, o.Branch))
	case hasBase:
		spec = config.RefSpec(fmt.Sprintf(
			"+refs/heads/%s:refs/remotes/%s/%s", o.Base, o.Remote, o.Base))
	default:
		return false, false, nil
	}
	err = rem.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{spec}, Auth: o.Auth, Tags: git.NoTags, Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, false, fmt.Errorf("fetch %s: %w", o.Remote, err)
	}
	return hasBranch, hasBase, nil
}

// syncLocalRef re-fetches the branch, requires the fetched head to carry the
// same tree as the local tip and the local ref to be unmoved, then points
// refs/heads/<branch> at the remote OID. The index and worktree are untouched
// because the trees are identical.
func syncLocalRef(ctx context.Context, repo *git.Repository, o Options, remoteURL string,
	startedFrom plumbing.Hash, expectedHead string) (string, error) {
	rem := pinnedRemote(repo, o.Remote, remoteURL)
	spec := config.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", o.Branch, o.Remote, o.Branch))
	err := rem.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: []config.RefSpec{spec}, Auth: o.Auth, Tags: git.NoTags, Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", fmt.Errorf("fetch %s: %w", o.Remote, err)
	}

	trackedName := plumbing.NewRemoteReferenceName(o.Remote, o.Branch)
	tracked, err := repo.Reference(trackedName, true)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", trackedName, err)
	}

	if tracked.Hash().String() != expectedHead {
		return "", &SyncError{Reason: fmt.Sprintf("%s is at %s, not the %s the replay produced",
			trackedName, tracked.Hash(), expectedHead)}
	}

	localCommit, err := repo.CommitObject(startedFrom)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", startedFrom, err)
	}
	remoteCommit, err := repo.CommitObject(tracked.Hash())
	if err != nil {
		return "", fmt.Errorf("read %s: %w", tracked.Hash(), err)
	}
	if localCommit.TreeHash != remoteCommit.TreeHash {
		return "", &SyncError{Reason: fmt.Sprintf("%s differs from %s after replay",
			startedFrom, trackedName)}
	}

	localName := plumbing.NewBranchReferenceName(o.Branch)
	current, err := repo.Reference(localName, true)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", localName, err)
	}
	if current.Hash() != startedFrom {
		return "", &SyncError{Reason: fmt.Sprintf("%s moved to %s, expected %s",
			localName, current.Hash(), startedFrom)}
	}

	if err := repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(localName, tracked.Hash()), current); err != nil {
		return "", fmt.Errorf("move %s: %w", localName, err)
	}
	return tracked.Hash().String(), nil
}

// matchReplayed adopts the remote-only commits that are already replays of the
// oldest local commits, comparing tree and message oldest first.
func matchReplayed(local, remoteOnly []*object.Commit) ([]Pair, []*object.Commit, error) {
	if len(remoteOnly) == 0 {
		return nil, local, nil
	}
	behind := &RefusedError{Reason: "local branch is behind: the remote holds commits that are not replays of it"}
	if len(remoteOnly) > len(local) {
		return nil, nil, behind
	}
	pairs := make([]Pair, 0, len(remoteOnly))
	adoptedAs := make(map[plumbing.Hash]plumbing.Hash, len(remoteOnly))
	for i, rc := range remoteOnly {
		lc := local[i]
		if rc.TreeHash != lc.TreeHash || normaliseMessage(rc.Message) != normaliseMessage(lc.Message) {
			return nil, nil, behind
		}
		// Each adopted commit must sit where a replay would have put it: every
		// parent already adopted resolves to its remote OID, and every parent
		// from before the range keeps the OID it has locally. For a linear
		// range this is the chain; for a merge it also pins the second parent.
		if len(rc.ParentHashes) != len(lc.ParentHashes) {
			return nil, nil, behind
		}
		for j, lp := range lc.ParentHashes {
			want := lp
			if mapped, ok := adoptedAs[lp]; ok {
				want = mapped
			}
			if rc.ParentHashes[j] != want {
				return nil, nil, behind
			}
		}
		adoptedAs[lc.Hash] = rc.Hash
		pairs = append(pairs, Pair{Local: lc.Hash.String(), Remote: rc.Hash.String()})
	}
	return pairs, local[len(remoteOnly):], nil
}

func normaliseMessage(m string) string {
	return strings.TrimRight(m, "\n")
}

// remoteParents maps every parent of a local commit to the OID it has on the
// remote: a parent inside the range is one this run replayed, and a parent
// outside it must already be on the remote, where its OID is unchanged.
func remoteParents(ctx context.Context, c Client, owner, repo string, ch *commitChange,
	remote map[plumbing.Hash]string) ([]string, error) {
	out := make([]string, 0, len(ch.parents))
	for _, ph := range ch.parents {
		if oid, ok := remote[ph]; ok {
			out = append(out, oid)
			continue
		}
		if _, err := c.CommitTree(ctx, owner, repo, ph.String()); err != nil {
			return nil, &RefusedError{OID: ch.oid, Reason: fmt.Sprintf(
				"parent %s is neither in the range being pushed nor already on the remote", ph)}
		}
		out = append(out, ph.String())
	}
	return out, nil
}

// treeEntries renders a commit's changed paths for the Git Data tree call; a
// deletion carries no SHA.
func treeEntries(ch *commitChange) []TreeEntry {
	out := make([]TreeEntry, 0, len(ch.entries))
	for _, e := range ch.entries {
		te := TreeEntry{Path: e.path, Mode: e.mode}
		if !e.hash.IsZero() {
			sha := e.hash.String()
			te.SHA = &sha
		}
		out = append(out, te)
	}
	return out
}

func joinRemotes(pairs []Pair) string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.Remote)
	}
	return strings.Join(out, ", ")
}
