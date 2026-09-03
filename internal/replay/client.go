package replay

import "context"

// TreeEntry is one changed path in a tree. SHA nil deletes the path; Mode is
// the git mode string ("100644", "100755", "120000", "160000").
type TreeEntry struct {
	Path string
	Mode string
	SHA  *string
}

// Client is the GitHub Git Data surface replay needs. internal/github
// implements it; tests use a fake.
type Client interface {
	// BranchHead returns the OID of refs/heads/branch, or "" when the branch does not exist.
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	// CreateRef creates refs/heads/branch at sha.
	CreateRef(ctx context.Context, owner, repo, branch, sha string) error
	// UpdateRef moves refs/heads/branch to sha without force. A rejected
	// non-fast-forward must surface as *HeadMismatchError.
	UpdateRef(ctx context.Context, owner, repo, branch, sha string) error
	// CommitTree returns the tree OID of a commit that exists on the remote.
	CommitTree(ctx context.Context, owner, repo, commitSHA string) (string, error)
	// CreateBlob uploads raw bytes and returns the blob OID GitHub stored them under.
	CreateBlob(ctx context.Context, owner, repo string, content []byte) (string, error)
	// CreateTree builds a tree from baseTree with entries applied over it.
	CreateTree(ctx context.Context, owner, repo, baseTree string, entries []TreeEntry) (string, error)
	// CreateCommit creates a commit with no author or committer field, so
	// GitHub signs it. It returns the new commit OID.
	CreateCommit(ctx context.Context, owner, repo, message, tree string, parents []string) (string, error)
}

// HeadMismatchError is returned when a non-force ref update is rejected because
// the remote branch has moved.
type HeadMismatchError struct {
	Expected string
	Message  string
}

func (e *HeadMismatchError) Error() string {
	if e.Expected == "" {
		return "remote head moved: " + e.Message
	}
	return "remote head is not " + e.Expected + ": " + e.Message
}
