package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GoodOlClint/mcp-server-github/internal/replay"
)

const defaultEndpoint = "https://api.github.com"
const defaultTimeout = 120 * time.Second

// maxResponseBytes caps a misdirected or hostile endpoint; Git Data responses
// are metadata, the largest being a tree listing.
const maxResponseBytes = 10 << 20

const apiVersion = "2022-11-28"

// notFastForward is the message GitHub returns when a non-force ref update
// would not be a fast forward. Confirmed against the live API on 2026-09-03.
const notFastForward = "not a fast forward"

// referenceAlreadyExists is what a ref create collides with when someone else
// created the branch first. Confirmed against the live API on 2026-09-03.
const referenceAlreadyExists = "reference already exists"

// APIError carries a non-2xx REST response.
type APIError struct {
	Status  int
	Message string
	Body    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("github: HTTP %d: %s", e.Status, e.Body)
}

// Client is a GitHub Git Data REST client satisfying replay.Client.
type Client struct {
	httpClient *http.Client
	endpoint   string
}

// Option configures a Client.
type Option func(*Client)

// WithEndpoint overrides the REST API root, for tests.
func WithEndpoint(u string) Option {
	return func(c *Client) { c.endpoint = strings.TrimSuffix(u, "/") }
}

// WithTimeout overrides the request timeout, default 120s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// New builds a Client whose requests are authenticated by rt.
func New(rt http.RoundTripper, opts ...Option) *Client {
	c := &Client{
		endpoint: defaultEndpoint,
		httpClient: &http.Client{
			Transport: rt,
			Timeout:   defaultTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// pathEscape escapes each segment of a ref or repository name while keeping the
// separators, so a slash in a branch name stays a path separator. url.PathEscape
// leaves dot segments alone and GitHub's edge resolves them, so a "." or ".."
// segment would retarget the request at another repository under the same
// installation token; reject them here rather than trust the caller.
func pathEscape(s string) (string, error) {
	if s == "" {
		return "", errors.New("github: empty path segment")
	}
	parts := strings.Split(s, "/")
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("github: %q contains the path segment %q", s, p)
		}
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/"), nil
}

func (c *Client) repoURL(owner, repo, suffix string) (string, error) {
	o, err := pathEscape(owner)
	if err != nil {
		return "", err
	}
	r, err := pathEscape(repo)
	if err != nil {
		return "", err
	}
	return c.endpoint + "/repos/" + o + "/" + r + "/" + suffix, nil
}

// refURL builds a git/ref or git/refs path for a branch. prefix is the segment
// before the ref, which differs between reading and writing a ref.
func (c *Client) refURL(owner, repo, prefix, branch string) (string, error) {
	b, err := pathEscape(branch)
	if err != nil {
		return "", err
	}
	return c.repoURL(owner, repo, prefix+"/heads/"+b)
}

// do issues one request and decodes a JSON body into out, which may be nil.
func (c *Client) do(ctx context.Context, method, url string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("github: encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("github: request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("github: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("github: decode response: %w", err)
	}
	return nil
}

func newAPIError(status int, body []byte) *APIError {
	var parsed struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)
	prefix := body
	if len(prefix) > 500 {
		prefix = prefix[:500]
	}
	return &APIError{Status: status, Message: parsed.Message, Body: string(prefix)}
}

type objectRef struct {
	SHA string `json:"sha"`
}

// BranchHead returns the OID of refs/heads/branch, or "" when the branch does not exist.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	var parsed struct {
		Object objectRef `json:"object"`
	}
	u, err := c.refURL(owner, repo, "git/ref", branch)
	if err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodGet, u, nil, &parsed); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return "", nil
		}
		return "", err
	}
	return parsed.Object.SHA, nil
}

// CreateRef creates refs/heads/branch at sha.
func (c *Client) CreateRef(ctx context.Context, owner, repo, branch, sha string) error {
	u, err := c.repoURL(owner, repo, "git/refs")
	if err != nil {
		return err
	}
	body := map[string]any{"ref": "refs/heads/" + branch, "sha": sha}
	err = c.do(ctx, http.MethodPost, u, body, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(apiErr.Message), referenceAlreadyExists) {
		// Someone created the branch between the head read and this call: the
		// new-branch form of the head race, so the caller can simply re-run.
		return &replay.HeadMismatchError{Message: apiErr.Message}
	}
	return err
}

// UpdateRef moves refs/heads/branch to sha without force; a rejected
// non-fast-forward surfaces as *replay.HeadMismatchError.
func (c *Client) UpdateRef(ctx context.Context, owner, repo, branch, sha string) error {
	u, err := c.refURL(owner, repo, "git/refs", branch)
	if err != nil {
		return err
	}
	body := map[string]any{"sha": sha, "force": false}
	err = c.do(ctx, http.MethodPatch, u, body, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(apiErr.Message), notFastForward) {
		return &replay.HeadMismatchError{Message: apiErr.Message}
	}
	return err
}

// CommitTree returns the tree OID of a commit that exists on the remote.
func (c *Client) CommitTree(ctx context.Context, owner, repo, commitSHA string) (string, error) {
	var parsed struct {
		Tree objectRef `json:"tree"`
	}
	sha, err := pathEscape(commitSHA)
	if err != nil {
		return "", err
	}
	u, err := c.repoURL(owner, repo, "git/commits/"+sha)
	if err != nil {
		return "", err
	}
	if err := c.do(ctx, http.MethodGet, u, nil, &parsed); err != nil {
		return "", err
	}
	if parsed.Tree.SHA == "" {
		return "", fmt.Errorf("github: commit %s carried no tree", commitSHA)
	}
	return parsed.Tree.SHA, nil
}

// CreateBlob uploads raw bytes and returns the blob OID GitHub stored them under.
func (c *Client) CreateBlob(ctx context.Context, owner, repo string, content []byte) (string, error) {
	body := map[string]any{
		"content":  base64.StdEncoding.EncodeToString(content),
		"encoding": "base64",
	}
	u, err := c.repoURL(owner, repo, "git/blobs")
	if err != nil {
		return "", err
	}
	var parsed objectRef
	if err := c.do(ctx, http.MethodPost, u, body, &parsed); err != nil {
		return "", err
	}
	if parsed.SHA == "" {
		return "", errors.New("github: blob response carried no sha")
	}
	return parsed.SHA, nil
}

// entryType is the object type the Git Data API pairs with a mode. An unknown
// mode is an error rather than a "blob" guess: guessing would send a tree entry
// that GitHub accepts and stores wrong.
func entryType(mode string) (string, error) {
	switch mode {
	case "100644", "100755", "120000":
		return "blob", nil
	case "160000":
		return "commit", nil
	}
	return "", fmt.Errorf("github: unsupported tree entry mode %q", mode)
}

// CreateTree builds a tree from baseTree with entries applied over it.
func (c *Client) CreateTree(ctx context.Context, owner, repo, baseTree string,
	entries []replay.TreeEntry) (string, error) {
	type wireEntry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		// A deletion is an explicit null. Tagging this omitempty would drop the
		// key and silently turn every deletion into a no-op.
		SHA *string `json:"sha"`
	}
	wire := make([]wireEntry, 0, len(entries))
	for _, e := range entries {
		typ, err := entryType(e.Mode)
		if err != nil {
			return "", fmt.Errorf("%w (path %q)", err, e.Path)
		}
		wire = append(wire, wireEntry{Path: e.Path, Mode: e.Mode, Type: typ, SHA: e.SHA})
	}
	u, err := c.repoURL(owner, repo, "git/trees")
	if err != nil {
		return "", err
	}
	body := map[string]any{"tree": wire}
	// A root commit starts from no tree at all; sending an empty base_tree
	// would be a reference to a tree that does not exist.
	if baseTree != "" {
		body["base_tree"] = baseTree
	}
	var parsed objectRef
	if err := c.do(ctx, http.MethodPost, u, body, &parsed); err != nil {
		return "", err
	}
	if parsed.SHA == "" {
		return "", errors.New("github: tree response carried no sha")
	}
	return parsed.SHA, nil
}

// CreateCommit creates a commit carrying no author or committer field, which is
// what makes GitHub sign it (ADR 0006).
func (c *Client) CreateCommit(ctx context.Context, owner, repo, message, tree string,
	parents []string) (string, error) {
	if parents == nil {
		parents = []string{}
	}
	u, err := c.repoURL(owner, repo, "git/commits")
	if err != nil {
		return "", err
	}
	body := map[string]any{"message": message, "tree": tree, "parents": parents}
	var parsed objectRef
	if err := c.do(ctx, http.MethodPost, u, body, &parsed); err != nil {
		return "", err
	}
	if parsed.SHA == "" {
		return "", errors.New("github: commit response carried no sha")
	}
	return parsed.SHA, nil
}

// DeleteBranch deletes refs/heads/branch.
func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	u, err := c.refURL(owner, repo, "git/refs", branch)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, u, nil, nil)
}

var _ replay.Client = (*Client)(nil)
