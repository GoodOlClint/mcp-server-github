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

// commitChange is one local commit rendered as a createCommitOnBranch payload.
type commitChange struct {
	oid       string
	message   string
	additions []FileAddition
	deletions []string
}

func mergeBase(repo *git.Repository, a, b plumbing.Hash) (plumbing.Hash, error) {
	ca, err := repo.CommitObject(a)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read %s: %w", a, err)
	}
	cb, err := repo.CommitObject(b)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read %s: %w", b, err)
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("merge base %s %s: %w", a, b, err)
	}
	if len(bases) == 0 {
		return plumbing.ZeroHash, &RefusedError{Reason: fmt.Sprintf(
			"%s and %s have no common ancestor", a, b)}
	}
	return bases[0].Hash, nil
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

// changesFor renders one commit as additions and deletions, reading blobs from
// the object store and never from the working tree. Every entry the mutation
// cannot represent is a refusal (ADR 0002).
func changesFor(ctx context.Context, repo *git.Repository, c *object.Commit, maxBytes int64) (*commitChange, error) {
	oid := c.Hash.String()
	parent, err := repo.CommitObject(c.ParentHashes[0])
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", c.ParentHashes[0], err)
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, fmt.Errorf("tree of %s: %w", parent.Hash, err)
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
		if perr := validateTreePaths(repo, oid, parentTree.Hash, "", seen); perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("diff %s..%s: %w", parent.Hash, c.Hash, err)
	}
	if len(changes) == 0 {
		return nil, &RefusedError{OID: oid, Reason: "commit is empty"}
	}

	message := normaliseMessage(c.Message)
	if !utf8.ValidString(message) {
		return nil, &RefusedError{OID: oid, Reason: "commit message is not valid UTF-8"}
	}
	total := int64(len(message))
	out := &commitChange{oid: oid, message: message}

	sort.Sort(changes)
	for _, ch := range changes {
		path := ch.To.Name
		if path == "" {
			path = ch.From.Name
		}
		if err := checkPath(oid, path); err != nil {
			return nil, err
		}
		srcMode, dstMode := ch.From.TreeEntry.Mode, ch.To.TreeEntry.Mode
		if srcMode == filemode.Submodule || dstMode == filemode.Submodule {
			return nil, &RefusedError{OID: oid, Path: path, Reason: "entry is a submodule"}
		}
		if dstMode == filemode.Empty {
			out.deletions = append(out.deletions, path)
			continue
		}
		if dstMode != filemode.Regular {
			return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
				"resulting mode %s is not a regular file", dstMode)}
		}
		if srcMode != filemode.Empty && srcMode != filemode.Regular {
			return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
				"source mode %s is not a regular file", srcMode)}
		}
		size, err := blobSize(repo, ch.To.TreeEntry.Hash)
		if err != nil {
			return nil, err
		}
		total += size
		if total > maxBytes {
			return nil, &RefusedError{OID: oid, Path: path, Reason: fmt.Sprintf(
				"commit exceeds MaxCommitBytes (%d)", maxBytes)}
		}
		content, err := readBlob(repo, ch.To.TreeEntry.Hash)
		if err != nil {
			return nil, err
		}
		out.additions = append(out.additions, FileAddition{Path: path, Contents: content})
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
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", h, err)
	}
	return content, nil
}
