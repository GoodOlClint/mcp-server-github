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
	"strings"
	"sync"
	"time"

	"github.com/GoodOlClint/mcp-server-github/internal/replay"
)

const defaultEndpoint = "https://api.github.com/graphql"
const defaultTimeout = 120 * time.Second
const maxResponseBytes = 10 << 20 // GraphQL responses here are small; caps a misdirected or hostile endpoint.

// GraphQLError carries the "errors" array of a GraphQL response.
type GraphQLError struct {
	Errors []struct {
		Message string
		Type    string
		Path    []any
	}
}

func (e *GraphQLError) Error() string {
	if len(e.Errors) == 0 {
		return "github: graphql error"
	}
	msgs := make([]string, len(e.Errors))
	for i, ge := range e.Errors {
		msgs[i] = ge.Message
	}
	return "github: graphql error: " + strings.Join(msgs, "; ")
}

// Client is a GitHub GraphQL client satisfying replay.Client.
type Client struct {
	httpClient *http.Client
	endpoint   string

	mu      sync.Mutex
	repoIDs map[string]string
}

// Option configures a Client.
type Option func(*Client)

// WithEndpoint overrides the GraphQL endpoint, for tests.
func WithEndpoint(url string) Option {
	return func(c *Client) { c.endpoint = url }
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
		repoIDs: make(map[string]string),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Path    []any  `json:"path"`
}

type envelope struct {
	Data   json.RawMessage `json:"data"`
	Errors []wireError     `json:"errors"`
}

func (c *Client) do(ctx context.Context, query string, variables map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("github: encode graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("github: build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: graphql request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("github: read graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		prefix := respBody
		if len(prefix) > 500 {
			prefix = prefix[:500]
		}
		return nil, fmt.Errorf("github: graphql request failed: HTTP %d: %s", resp.StatusCode, prefix)
	}

	var env envelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("github: decode graphql response: %w", err)
	}

	if len(env.Errors) > 0 {
		gqlErr := &GraphQLError{}
		for _, e := range env.Errors {
			gqlErr.Errors = append(gqlErr.Errors, struct {
				Message string
				Type    string
				Path    []any
			}{Message: e.Message, Type: e.Type, Path: e.Path})
		}
		return nil, gqlErr
	}

	if len(env.Data) == 0 || string(env.Data) == "null" {
		prefix := respBody
		if len(prefix) > 500 {
			prefix = prefix[:500]
		}
		return nil, fmt.Errorf("github: graphql response had no data and no errors: %s", prefix)
	}

	return env.Data, nil
}

func (c *Client) repoID(ctx context.Context, owner, repo string) (string, error) {
	key := owner + "/" + repo
	c.mu.Lock()
	id, ok := c.repoIDs[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}

	const q = `
	query($owner: String!, $repo: String!) {
	  repository(owner: $owner, name: $repo) { id }
	}
	`
	data, err := c.do(ctx, q, map[string]any{"owner": owner, "repo": repo})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Repository struct {
			ID string `json:"id"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("github: decode repository id: %w", err)
	}
	if parsed.Repository.ID == "" {
		return "", fmt.Errorf("github: repository %s not found or not accessible to this installation", key)
	}

	c.mu.Lock()
	c.repoIDs[key] = parsed.Repository.ID
	c.mu.Unlock()
	return parsed.Repository.ID, nil
}

// BranchHead returns the OID of refs/heads/branch, or "" when the branch does not exist.
func (c *Client) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	const q = `
	query($owner: String!, $repo: String!, $qualifiedName: String!) {
	  repository(owner: $owner, name: $repo) {
	    ref(qualifiedName: $qualifiedName) { target { oid } }
	  }
	}
	`
	data, err := c.do(ctx, q, map[string]any{
		"owner":         owner,
		"repo":          repo,
		"qualifiedName": "refs/heads/" + branch,
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Repository struct {
			Ref *struct {
				Target struct {
					Oid string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("github: decode branch head: %w", err)
	}
	if parsed.Repository.Ref == nil {
		return "", nil
	}
	return parsed.Repository.Ref.Target.Oid, nil
}

func (c *Client) branchRefID(ctx context.Context, owner, repo, branch string) (string, error) {
	const q = `
	query($owner: String!, $repo: String!, $qualifiedName: String!) {
	  repository(owner: $owner, name: $repo) {
	    ref(qualifiedName: $qualifiedName) { id }
	  }
	}
	`
	data, err := c.do(ctx, q, map[string]any{
		"owner":         owner,
		"repo":          repo,
		"qualifiedName": "refs/heads/" + branch,
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		Repository struct {
			Ref *struct {
				ID string `json:"id"`
			} `json:"ref"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("github: decode branch ref id: %w", err)
	}
	if parsed.Repository.Ref == nil {
		return "", nil
	}
	return parsed.Repository.Ref.ID, nil
}

// CreateBranch creates refs/heads/branch at fromOID and returns fromOID.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, branch, fromOID string) (string, error) {
	repoID, err := c.repoID(ctx, owner, repo)
	if err != nil {
		return "", err
	}

	const m = `
	mutation($repositoryId: ID!, $name: String!, $oid: GitObjectID!) {
	  createRef(input: {repositoryId: $repositoryId, name: $name, oid: $oid}) {
	    ref { target { oid } }
	  }
	}
	`
	data, err := c.do(ctx, m, map[string]any{
		"repositoryId": repoID,
		"name":         "refs/heads/" + branch,
		"oid":          fromOID,
	})
	if err != nil {
		return "", err
	}
	var parsed struct {
		CreateRef struct {
			Ref struct {
				Target struct {
					Oid string `json:"oid"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"createRef"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("github: decode createRef response: %w", err)
	}
	return parsed.CreateRef.Ref.Target.Oid, nil
}

// CreateCommit issues one createCommitOnBranch and returns the new commit OID.
func (c *Client) CreateCommit(ctx context.Context, owner, repo, branch, expectedHeadOID, message string, additions []replay.FileAddition, deletions []string) (string, error) {
	headline, body := splitMessage(message)

	type fileAddition struct {
		Path     string `json:"path"`
		Contents string `json:"contents"`
	}
	type fileDeletion struct {
		Path string `json:"path"`
	}

	wireAdditions := make([]fileAddition, 0, len(additions))
	for _, a := range additions {
		wireAdditions = append(wireAdditions, fileAddition{
			Path:     a.Path,
			Contents: base64.StdEncoding.EncodeToString(a.Contents),
		})
	}
	wireDeletions := make([]fileDeletion, 0, len(deletions))
	for _, p := range deletions {
		wireDeletions = append(wireDeletions, fileDeletion{Path: p})
	}

	const m = `
	mutation($input: CreateCommitOnBranchInput!) {
	  createCommitOnBranch(input: $input) {
	    commit { oid }
	  }
	}
	`
	input := map[string]any{
		"branch": map[string]any{
			"repositoryNameWithOwner": owner + "/" + repo,
			"branchName":              branch,
		},
		"expectedHeadOid": expectedHeadOID,
		"message": map[string]any{
			"headline": headline,
			"body":     body,
		},
		"fileChanges": map[string]any{
			"additions": wireAdditions,
			"deletions": wireDeletions,
		},
	}

	data, err := c.do(ctx, m, map[string]any{"input": input})
	if err != nil {
		var gqlErr *GraphQLError
		if errors.As(err, &gqlErr) {
			for _, e := range gqlErr.Errors {
				if strings.Contains(e.Message, "Expected branch to point to") {
					return "", &replay.HeadMismatchError{Expected: expectedHeadOID, Message: e.Message}
				}
			}
		}
		return "", err
	}

	var parsed struct {
		CreateCommitOnBranch struct {
			Commit struct {
				Oid string `json:"oid"`
			} `json:"commit"`
		} `json:"createCommitOnBranch"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("github: decode createCommitOnBranch response: %w", err)
	}
	return parsed.CreateCommitOnBranch.Commit.Oid, nil
}

// DeleteBranch deletes refs/heads/branch.
func (c *Client) DeleteBranch(ctx context.Context, owner, repo, branch string) error {
	refID, err := c.branchRefID(ctx, owner, repo, branch)
	if err != nil {
		return err
	}
	if refID == "" {
		return fmt.Errorf("github: branch %s/%s@%s does not exist", owner, repo, branch)
	}

	const m = `
	mutation($refId: ID!) {
	  deleteRef(input: {refId: $refId}) {
	    clientMutationId
	  }
	}
	`
	_, err = c.do(ctx, m, map[string]any{"refId": refID})
	return err
}

func splitMessage(message string) (headline, body string) {
	parts := strings.SplitN(message, "\n", 2)
	headline = parts[0]
	if len(parts) > 1 {
		body = strings.TrimSpace(parts[1])
	}
	return headline, body
}

var _ replay.Client = (*Client)(nil)
