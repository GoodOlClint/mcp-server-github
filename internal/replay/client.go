package replay

import "context"

// FileAddition carries a whole file; Contents is the raw blob, not base64.
type FileAddition struct {
	Path     string
	Contents []byte
}

// Client is the GitHub surface replay needs. internal/github implements it;
// tests use a fake.
type Client interface {
	// BranchHead returns the OID of refs/heads/branch, or "" when the branch does not exist.
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	// CreateBranch creates refs/heads/branch at fromOID and returns fromOID.
	CreateBranch(ctx context.Context, owner, repo, branch, fromOID string) (string, error)
	// CreateCommit issues one createCommitOnBranch and returns the new commit OID.
	// A stale expectedHeadOID must surface as *HeadMismatchError.
	CreateCommit(ctx context.Context, owner, repo, branch, expectedHeadOID, message string, additions []FileAddition, deletions []string) (string, error)
}

// HeadMismatchError is returned when the remote branch no longer points at expectedHeadOID.
type HeadMismatchError struct {
	Expected string
	Message  string
}

func (e *HeadMismatchError) Error() string { return "remote head is not " + e.Expected + ": " + e.Message }
