// Package replay replays local commits onto GitHub through createCommitOnBranch
// and then moves the local branch ref onto the returned OIDs. See docs/design.md
// and docs/decisions/0001-0004.
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

// DefaultMaxCommitBytes is the ADR 0002 ceiling: the largest payload that
// succeeded on every spike attempt, minus 10%.
const DefaultMaxCommitBytes int64 = 7_200_000

// RefusedError reports a commit or remote that createCommitOnBranch cannot
// represent. Nothing has been mutated when it is returned.
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

// Result is the outcome of a Push. Pairs is populated even when Push returns an
// error, so a caller can record the commits that reached GitHub; Head is set
// only when the replay completed and the local ref was moved.
type Result struct {
	Pairs []Pair
	Head  string
}

// Options configures Push. Auth is passed to every go-git network operation;
// callers use &http.BasicAuth{Username: "x-access-token", Password: token}.
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
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	return u.String()
}

var scpRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@([^:/]+):(.+)$`)

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
	return owner, name, urls[0], nil
}

var repoSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

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
		host, path = m[1], m[2]
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return refuse("is not a recognised git url")
		}
		if u.Scheme != "ssh" && u.Scheme != "https" {
			return refuse("uses unsupported scheme " + u.Scheme)
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
	if o.MaxCommitBytes <= 0 {
		o.MaxCommitBytes = DefaultMaxCommitBytes
	}
	return o
}

// Push replays the commits the remote branch lacks onto GitHub, one
// createCommitOnBranch per commit, then moves refs/heads/<branch> onto the
// remote OIDs. On any refusal nothing is mutated.
func Push(ctx context.Context, c Client, o Options) (Result, error) {
	o = o.normalise()
	for _, v := range []struct{ name, label string }{
		{o.Branch, "branch"}, {o.Base, "base"}, {o.Remote, "remote"},
	} {
		if err := validateRefName(v.name, v.label); err != nil {
			return Result{}, err
		}
	}

	repo, err := git.PlainOpen(o.RepoPath)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", o.RepoPath, err)
	}
	owner, repoName, remoteURL, err := ownerRepoURL(repo, o.Remote)
	if err != nil {
		return Result{}, err
	}

	remoteHasBranch, err := fetchRefs(ctx, repo, o, remoteURL)
	if err != nil {
		return Result{}, err
	}

	localTip, err := repo.Reference(plumbing.NewBranchReferenceName(o.Branch), true)
	if err != nil {
		return Result{}, fmt.Errorf("resolve refs/heads/%s: %w", o.Branch, err)
	}

	var trackedName plumbing.ReferenceName
	if remoteHasBranch {
		trackedName = plumbing.NewRemoteReferenceName(o.Remote, o.Branch)
	} else {
		trackedName = plumbing.NewRemoteReferenceName(o.Remote, o.Base)
	}
	tracked, err := repo.Reference(trackedName, true)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s: %w", trackedName, err)
	}

	local, remoteOnly, err := diverged(repo, tracked.Hash(), localTip.Hash())
	if err != nil {
		return Result{}, err
	}
	if !remoteHasBranch {
		// remoteOnly here is base-only history, which is not a replay concern.
		remoteOnly = nil
	}

	for _, cm := range local {
		if len(cm.ParentHashes) == 0 {
			return Result{}, &RefusedError{OID: cm.Hash.String(), Reason: "commit has zero parents"}
		}
		if len(cm.ParentHashes) > 1 {
			return Result{}, &RefusedError{OID: cm.Hash.String(), Reason: "commit is a merge commit"}
		}
	}

	adopted, pending, err := matchReplayed(local, remoteOnly)
	if err != nil {
		return Result{}, err
	}

	if remoteHasBranch && len(pending) == 0 && len(adopted) == 0 {
		return Result{Pairs: nil, Head: tracked.Hash().String()}, nil
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
	if head == "" {
		if remoteHasBranch {
			return Result{}, &SyncError{Reason: fmt.Sprintf(
				"%s/%s exists locally as %s but not on GitHub", o.Remote, o.Branch, tracked.Hash())}
		}
		forkPoint, err := mergeBase(repo, tracked.Hash(), localTip.Hash())
		if err != nil {
			return Result{}, err
		}
		head, err = c.CreateBranch(ctx, owner, repoName, o.Branch, forkPoint.String())
		if err != nil {
			return Result{}, fmt.Errorf("create branch %s: %w", o.Branch, err)
		}
	} else if remoteHasBranch && head != tracked.Hash().String() {
		return Result{}, &SyncError{Reason: fmt.Sprintf(
			"%s/%s is at %s, not the fetched %s; nothing was sent, re-run to recompute the range",
			o.Remote, o.Branch, head, tracked.Hash())}
	} else if !remoteHasBranch {
		return Result{}, &SyncError{Reason: fmt.Sprintf(
			"%s/%s appeared on GitHub at %s after the fetch; nothing was sent, re-run to recompute the range",
			o.Remote, o.Branch, head)}
	}

	pairs := make([]Pair, 0, len(adopted)+len(changes))
	pairs = append(pairs, adopted...)

	for _, ch := range changes {
		newOID, err := c.CreateCommit(ctx, owner, repoName, o.Branch, head,
			ch.message, ch.additions, ch.deletions)
		if err != nil {
			var mismatch *HeadMismatchError
			if errors.As(err, &mismatch) {
				return Result{Pairs: pairs}, &SyncError{
					Reason: fmt.Sprintf("remote %s/%s head moved during replay of %s: %s",
						o.Remote, o.Branch, ch.oid, mismatch.Message),
					Replayed: pairs,
				}
			}
			return Result{Pairs: pairs}, fmt.Errorf("create commit %s: %w", ch.oid, err)
		}
		pairs = append(pairs, Pair{Local: ch.oid, Remote: newOID})
		head = newOID
	}

	finalHead, err := syncLocalRef(ctx, repo, o, remoteURL, localTip.Hash(), pairs[len(pairs)-1].Remote)
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

// pinnedRemote builds a remote bound to the URL OwnerRepo validated rather than
// re-reading .git/config, which go-git parses afresh on every access.
func pinnedRemote(repo *git.Repository, name, url string) *git.Remote {
	return git.NewRemote(repo.Storer, &config.RemoteConfig{Name: name, URLs: []string{url}})
}

// fetchRefs brings the base and, when it exists on the remote, the branch into
// refs/remotes/<remote>/. It reports whether the remote has the branch.
func fetchRefs(ctx context.Context, repo *git.Repository, o Options, remoteURL string) (bool, error) {
	rem := pinnedRemote(repo, o.Remote, remoteURL)
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: o.Auth})
	if err != nil {
		return false, fmt.Errorf("list %s: %w", o.Remote, err)
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
	var specs []config.RefSpec
	if hasBranch {
		specs = append(specs, config.RefSpec(fmt.Sprintf(
			"+refs/heads/%s:refs/remotes/%s/%s", o.Branch, o.Remote, o.Branch)))
	} else {
		// The base is only needed to place a branch the remote does not have.
		if !hasBase {
			return false, &RefusedError{Reason: fmt.Sprintf("%s has no branch %s", o.Remote, o.Base)}
		}
		specs = append(specs, config.RefSpec(fmt.Sprintf(
			"+refs/heads/%s:refs/remotes/%s/%s", o.Base, o.Remote, o.Base)))
	}
	err = rem.FetchContext(ctx, &git.FetchOptions{
		RefSpecs: specs, Auth: o.Auth, Tags: git.NoTags, Force: true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, fmt.Errorf("fetch %s: %w", o.Remote, err)
	}
	return hasBranch, nil
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
	for i, rc := range remoteOnly {
		lc := local[i]
		if rc.TreeHash != lc.TreeHash || storedMessage(rc.Message) != storedMessage(lc.Message) {
			return nil, nil, behind
		}
		// The adopted commits must form the chain a replay would have built,
		// starting where the first local commit does.
		want := lc.ParentHashes[0]
		if i > 0 {
			want = remoteOnly[i-1].Hash
		}
		if len(rc.ParentHashes) != 1 || rc.ParentHashes[0] != want {
			return nil, nil, behind
		}
		pairs = append(pairs, Pair{Local: lc.Hash.String(), Remote: rc.Hash.String()})
	}
	return pairs, local[len(remoteOnly):], nil
}

func normaliseMessage(m string) string {
	return strings.TrimRight(m, "\n")
}

// storedMessage renders a commit message the way createCommitOnBranch stores
// it: a headline and a body joined by a blank line. Comparing raw messages
// would refuse to resume a replay of a message with no blank line after the
// subject.
func storedMessage(m string) string {
	headline, body, _ := strings.Cut(normaliseMessage(m), "\n")
	body = strings.TrimSpace(body)
	if body == "" {
		return headline
	}
	return headline + "\n\n" + body
}
