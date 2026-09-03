package replay

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// maxBlobBytes is the largest blob the API actually accepts. GitHub documents
// 100 MB, but the blobs endpoint rejects a request whose base64 content exceeds
// 32 MiB with HTTP 401 "Bad credentials", which is 24 MiB of file. Measured
// against api.github.com on 2026-09-03: 24 MiB uploads, 25 MB does not.
const maxBlobBytes int64 = 24 << 20

// submoduleMode is a gitlink: the OID names a commit in another repository, so
// it is referenced in the tree and never uploaded.
const submoduleMode = "160000"

// entryChange is one changed path of a commit. hash is zero for a deletion and
// a gitlink OID for a submodule; otherwise it names a blob to upload.
type entryChange struct {
	path string
	mode string
	hash plumbing.Hash
}

// commitChange is one local commit rendered as a tree patch. tree is the OID
// the remote tree must come back as.
type commitChange struct {
	oid     string
	message string
	tree    plumbing.Hash
	parents []plumbing.Hash
	entries []entryChange
}

// modeString renders a git file mode the way the Git Data API spells it.
func modeString(m filemode.FileMode) (string, bool) {
	switch m {
	case filemode.Regular:
		return "100644", true
	case filemode.Executable:
		return "100755", true
	case filemode.Symlink:
		return "120000", true
	case filemode.Submodule:
		return "160000", true
	}
	return "", false
}

// diverged returns the commits reachable from b but not a, and from a but not
// b, each oldest first in topological order.
func diverged(repo *git.Repository, a, b plumbing.Hash) (onlyB, onlyA []*object.Commit, err error) {
	ca, err := repo.CommitObject(a)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", a, err)
	}
	cb, err := repo.CommitObject(b)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", b, err)
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return nil, nil, fmt.Errorf("merge base %s %s: %w", a, b, err)
	}
	if len(bases) == 0 {
		return nil, nil, &RefusedError{Reason: fmt.Sprintf(
			"%s and %s have no common ancestor", a, b)}
	}
	stop := make(map[plumbing.Hash]bool, len(bases))
	for _, base := range bases {
		stop[base.Hash] = true
	}
	onlyB, err = walkUntil(repo, cb, stop)
	if err != nil {
		return nil, nil, err
	}
	onlyA, err = walkUntil(repo, ca, stop)
	if err != nil {
		return nil, nil, err
	}
	if onlyB, err = dropReachable(onlyB, ca); err != nil {
		return nil, nil, err
	}
	if onlyA, err = dropReachable(onlyA, cb); err != nil {
		return nil, nil, err
	}
	return onlyB, onlyA, nil
}

// dropReachable removes commits that are ancestors of tip. A merge below the
// merge-base frontier can reach a common ancestor by a path around every base,
// so walkUntil alone is exact only for a merge-free region.
func dropReachable(commits []*object.Commit, tip *object.Commit) ([]*object.Commit, error) {
	merged := false
	for _, c := range commits {
		if len(c.ParentHashes) > 1 {
			merged = true
			break
		}
	}
	if !merged {
		return commits, nil
	}
	out := commits[:0]
	for _, c := range commits {
		ancestor, err := c.IsAncestor(tip)
		if err != nil {
			return nil, fmt.Errorf("reachability of %s: %w", c.Hash, err)
		}
		if !ancestor {
			out = append(out, c)
		}
	}
	return out, nil
}

func walkUntil(repo *git.Repository, tip *object.Commit, stop map[plumbing.Hash]bool) ([]*object.Commit, error) {
	found := map[plumbing.Hash]*object.Commit{}
	if stop[tip.Hash] {
		return nil, nil
	}
	queue := []*object.Commit{tip}
	found[tip.Hash] = tip
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ph := range cur.ParentHashes {
			if stop[ph] || found[ph] != nil {
				continue
			}
			p, err := repo.CommitObject(ph)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", ph, err)
			}
			found[ph] = p
			queue = append(queue, p)
		}
	}
	return topoOldestFirst(found), nil
}

func topoOldestFirst(commits map[plumbing.Hash]*object.Commit) []*object.Commit {
	indegree := map[plumbing.Hash]int{}
	children := map[plumbing.Hash][]plumbing.Hash{}
	for h, c := range commits {
		for _, ph := range c.ParentHashes {
			if _, ok := commits[ph]; ok {
				indegree[h]++
				children[ph] = append(children[ph], h)
			}
		}
	}
	ready := make([]*object.Commit, 0, len(commits))
	for h, c := range commits {
		if indegree[h] == 0 {
			ready = append(ready, c)
		}
	}
	byAge := func(s []*object.Commit) {
		sort.Slice(s, func(i, j int) bool {
			ti, tj := s[i].Committer.When, s[j].Committer.When
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return s[i].Hash.String() < s[j].Hash.String()
		})
	}
	byAge(ready)

	out := make([]*object.Commit, 0, len(commits))
	for len(ready) > 0 {
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		var unlocked []*object.Commit
		for _, ch := range children[cur.Hash] {
			indegree[ch]--
			if indegree[ch] == 0 {
				unlocked = append(unlocked, commits[ch])
			}
		}
		if len(unlocked) > 0 {
			ready = append(ready, unlocked...)
			byAge(ready)
		}
	}
	return out
}

// changesFor renders one commit as a tree patch, taking modes and blob OIDs
// from the object store and never from the working tree. Blob contents are not
// read here; Push uploads each unique blob once.
func changesFor(ctx context.Context, repo *git.Repository, c *object.Commit, maxBytes int64) (*commitChange, error) {
	oid := c.Hash.String()
	// A root commit has no parent tree; go-git diffs a nil tree as empty.
	var parentTree *object.Tree
	if len(c.ParentHashes) > 0 {
		parent, err := repo.CommitObject(c.ParentHashes[0])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", c.ParentHashes[0], err)
		}
		parentTree, err = parent.Tree()
		if err != nil {
			return nil, fmt.Errorf("tree of %s: %w", parent.Hash, err)
		}
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree of %s: %w", c.Hash, err)
	}
	changes, err := object.DiffTreeWithOptions(ctx, parentTree, tree, &object.DiffTreeOptions{})
	if err != nil {
		// go-git refuses to walk a tree holding a path git will not check out,
		// so name the offending path before surfacing the failure.
		seen := map[plumbing.Hash]bool{}
		if perr := validateTreePaths(repo, oid, tree.Hash, "", seen); perr != nil {
			return nil, perr
		}
		if parentTree != nil {
			if perr := validateTreePaths(repo, oid, parentTree.Hash, "", seen); perr != nil {
				return nil, perr
			}
		}
		return nil, fmt.Errorf("diff into %s: %w", c.Hash, err)
	}
	if len(changes) == 0 {
		return nil, &RefusedError{OID: oid, Reason: "commit is empty"}
	}

	message := normaliseMessage(c.Message)
	if !utf8.ValidString(message) {
		return nil, &RefusedError{OID: oid, Reason: "commit message is not valid UTF-8"}
	}
	// message is required by the commit endpoint, so an empty one is a
	// pre-flight refusal rather than a failure after the blobs are uploaded.
	if message == "" {
		return nil, &RefusedError{OID: oid, Reason: "commit message is empty"}
	}
	total := int64(len(message))
	out := &commitChange{oid: oid, message: message, tree: c.TreeHash, parents: c.ParentHashes}

	sort.Sort(changes)
	for _, ch := range changes {
		path := ch.To.Name
		if path == "" {
			path = ch.From.Name
		}
		if err := checkPath(oid, path); err != nil {
			return nil, err
		}
		dst := ch.To.TreeEntry
		if dst.Mode == filemode.Empty {
			mode, ok := modeString(ch.From.TreeEntry.Mode)
			if !ok {
				return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
					"mode %s is not a file, symlink or submodule", ch.From.TreeEntry.Mode)}
			}
			out.entries = append(out.entries, entryChange{path: path, mode: mode})
			continue
		}
		mode, ok := modeString(dst.Mode)
		if !ok {
			return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
				"mode %s is not a file, symlink or submodule", dst.Mode)}
		}
		if dst.Mode != filemode.Submodule {
			size, err := blobSize(repo, dst.Hash)
			if err != nil {
				return nil, err
			}
			if size > maxBlobBytes {
				return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
					"blob is %d bytes, over the %d byte per-blob upload limit", size, maxBlobBytes)}
			}
			total += size
			if total > maxBytes {
				return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
					"commit exceeds MaxCommitBytes (%d)", maxBytes)}
			}
		}
		out.entries = append(out.entries, entryChange{path: path, mode: mode, hash: dst.Hash})
	}
	return out, nil
}

// validateTreePaths walks a tree's entries directly, without the path-checking
// iterators, so an entry git itself rejects can be reported as a refusal.
func validateTreePaths(repo *git.Repository, oid string, h plumbing.Hash, prefix string,
	seen map[plumbing.Hash]bool) error {
	if seen[h] {
		return nil
	}
	seen[h] = true
	tree, err := repo.TreeObject(h)
	if err != nil {
		return nil
	}
	for _, e := range tree.Entries {
		path := prefix + e.Name
		if err := checkPath(oid, path); err != nil {
			return err
		}
		if e.Mode == filemode.Dir {
			if err := validateTreePaths(repo, oid, e.Hash, path+"/", seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkPath(oid, path string) error {
	refuse := func(reason string) error {
		return &RefusedError{OID: oid, Path: path, Reason: reason}
	}
	if path == "" {
		return refuse("path is empty")
	}
	if !utf8.ValidString(path) {
		return refuse("path is not valid UTF-8")
	}
	if strings.HasPrefix(path, "/") {
		return refuse("path is absolute")
	}
	for _, seg := range strings.Split(path, "/") {
		switch {
		case seg == "":
			return refuse("path has an empty segment")
		case seg == "." || seg == "..":
			return refuse("path contains a " + seg + " segment")
		case strings.EqualFold(seg, ".git"):
			return refuse("path contains a .git segment")
		}
	}
	return nil
}

// blobSize reads the size from the object header, so an oversize blob is
// refused before it is loaded into memory.
func blobSize(repo *git.Repository, h plumbing.Hash) (int64, error) {
	blob, err := repo.BlobObject(h)
	if err != nil {
		return 0, fmt.Errorf("read blob %s: %w", h, err)
	}
	return blob.Size, nil
}

func readBlob(repo *git.Repository, h plumbing.Hash) ([]byte, error) {
	blob, err := repo.BlobObject(h)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", h, err)
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", h, err)
	}
	defer r.Close()
	content, err := io.ReadAll(io.LimitReader(r, maxBlobBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", h, err)
	}
	// The size check in changesFor reads the object header; a stream longer
	// than the header declared would otherwise be uploaded unchecked.
	if int64(len(content)) > maxBlobBytes {
		return nil, &RefusedError{Reason: fmt.Sprintf(
			"blob %s is longer than the %d byte per-blob upload limit", h, maxBlobBytes)}
	}
	return content, nil
}

// uploadBlobs sends every blob the range needs exactly once. Blob OIDs are
// content addressed, so a returned OID that differs from the local one means
// the bytes GitHub stored are not the bytes the commit names.
func uploadBlobs(ctx context.Context, c Client, repo *git.Repository, owner, repoName string,
	changes []*commitChange) error {
	seen := make(map[plumbing.Hash]bool)
	for _, ch := range changes {
		for _, e := range ch.entries {
			if e.hash.IsZero() || e.mode == submoduleMode || seen[e.hash] {
				continue
			}
			seen[e.hash] = true
			content, err := readBlob(repo, e.hash)
			if err != nil {
				return err
			}
			got, err := c.CreateBlob(ctx, owner, repoName, content)
			if err != nil {
				return fmt.Errorf("upload blob %s for %s in %s: %w", e.hash, e.path, ch.oid, err)
			}
			if got != e.hash.String() {
				return &RefusedError{OID: ch.oid, Path: e.path, Reason: fmt.Sprintf(
					"GitHub stored the blob as %s, not the local %s", got, e.hash)}
			}
		}
	}
	return nil
}
