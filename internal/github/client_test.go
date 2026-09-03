package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/mcp-server-github/internal/replay"
)

// ---------- test server helpers ----------

type capturedRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func newTestServer(t *testing.T, handler func(req capturedRequest) (int, string)) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
			return
		}
		captured = append(captured, req)
		status, body := handler(req)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func fixedResponseServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------- BranchHead ----------

func TestBranchHeadPresent(t *testing.T) {
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		if req.Variables["qualifiedName"] != "refs/heads/main" {
			t.Errorf("unexpected qualifiedName: %v", req.Variables["qualifiedName"])
		}
		return 200, `{"data":{"repository":{"ref":{"target":{"oid":"abc123"}}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	oid, err := c.BranchHead(context.Background(), "octo", "hello-world", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oid != "abc123" {
		t.Fatalf("got oid %q", oid)
	}
	if len(*captured) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*captured))
	}
}

func TestBranchHeadMissingReturnsEmpty(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":{"repository":{"ref":null}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	oid, err := c.BranchHead(context.Background(), "octo", "hello-world", "nope")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oid != "" {
		t.Fatalf("expected empty oid, got %q", oid)
	}
}

// ---------- CreateBranch ----------

func TestCreateBranch(t *testing.T) {
	calls := 0
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		calls++
		if calls == 1 {
			// repo id lookup
			return 200, `{"data":{"repository":{"id":"R_repo1"}}}`
		}
		return 200, `{"data":{"createRef":{"ref":{"target":{"oid":"newoid1"}}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	oid, err := c.CreateBranch(context.Background(), "octo", "hello-world", "feature", "baseoid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oid != "newoid1" {
		t.Fatalf("got oid %q", oid)
	}
	req2 := (*captured)[1]
	want := map[string]any{"repositoryId": "R_repo1", "name": "refs/heads/feature", "oid": "baseoid"}
	for k, v := range want {
		if req2.Variables[k] != v {
			t.Fatalf("variable %s: got %v want %v", k, req2.Variables[k], v)
		}
	}
}

func TestCreateBranchCachesRepoID(t *testing.T) {
	calls := 0
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		calls++
		if strings.Contains(req.Query, "repository(owner: $owner, name: $repo) { id }") {
			return 200, `{"data":{"repository":{"id":"R_repo1"}}}`
		}
		return 200, `{"data":{"createRef":{"ref":{"target":{"oid":"newoid1"}}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	if _, err := c.CreateBranch(context.Background(), "octo", "hello-world", "f1", "base1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := c.CreateBranch(context.Background(), "octo", "hello-world", "f2", "base2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// repo id fetched once (call 1), each CreateBranch does one createRef call: total 3 requests, not 4
	if calls != 3 {
		t.Fatalf("expected 3 requests (1 repo id + 2 createRef), got %d", calls)
	}
	if got := c.repoIDs["octo/hello-world"]; got != "R_repo1" {
		t.Fatalf("cached repo id = %q, want R_repo1", got)
	}
}

func TestRepoIDNotFoundIsNotCached(t *testing.T) {
	calls := 0
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		calls++
		return 200, `{"data":{"repository":null}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	if _, err := c.CreateBranch(context.Background(), "octo", "ghost", "f1", "base1"); err == nil {
		t.Fatalf("expected error for missing repository")
	}
	if _, err := c.CreateBranch(context.Background(), "octo", "ghost", "f2", "base2"); err == nil {
		t.Fatalf("expected error for missing repository")
	}
	if calls != 2 {
		t.Fatalf("expected the failed lookup to be retried, not cached; got %d calls", calls)
	}
	if _, cached := c.repoIDs["octo/ghost"]; cached {
		t.Fatalf("a failed repository lookup must not be cached")
	}
}

func TestGraphQLNullDataWithNoErrorsIsAnError(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":null}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	oid, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error, got oid=%q err=nil", oid)
	}
}

func TestGraphQLNonGraphQLBodyWithNoDataOrErrorsIsAnError(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"message":"Bad credentials","documentation_url":"https://docs.github.com"}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	_, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Fatalf("expected the diagnostic body to surface in the error, got %v", err)
	}
}

// ---------- CreateCommit ----------

func TestCreateCommitRequestShapeAndMultilineMessage(t *testing.T) {
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":{"createCommitOnBranch":{"commit":{"oid":"commitoid1"}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	oid, err := c.CreateCommit(context.Background(), "octo", "hello-world", "feature", "headoid1",
		"Add file\n\nSome body\nmore body",
		[]replay.FileAddition{{Path: "a.txt", Contents: []byte("hello")}},
		[]string{"b.txt"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if oid != "commitoid1" {
		t.Fatalf("got oid %q", oid)
	}
	req := (*captured)[0]
	input, ok := req.Variables["input"].(map[string]any)
	if !ok {
		t.Fatalf("input not a map: %#v", req.Variables["input"])
	}
	branch, ok := input["branch"].(map[string]any)
	if !ok {
		t.Fatalf("branch not a map: %#v", input["branch"])
	}
	if branch["repositoryNameWithOwner"] != "octo/hello-world" || branch["branchName"] != "feature" {
		t.Fatalf("unexpected branch field: %#v", branch)
	}
	if input["expectedHeadOid"] != "headoid1" {
		t.Fatalf("unexpected expectedHeadOid: %v", input["expectedHeadOid"])
	}
	message, ok := input["message"].(map[string]any)
	if !ok {
		t.Fatalf("message not a map: %#v", input["message"])
	}
	if message["headline"] != "Add file" || message["body"] != "Some body\nmore body" {
		t.Fatalf("unexpected message: %#v", message)
	}
	fileChanges, ok := input["fileChanges"].(map[string]any)
	if !ok {
		t.Fatalf("fileChanges not a map: %#v", input["fileChanges"])
	}
	additions, ok := fileChanges["additions"].([]any)
	if !ok || len(additions) != 1 {
		t.Fatalf("unexpected additions: %#v", fileChanges["additions"])
	}
	addition := additions[0].(map[string]any)
	if addition["path"] != "a.txt" {
		t.Fatalf("unexpected addition path: %v", addition["path"])
	}
	wantContents := base64.StdEncoding.EncodeToString([]byte("hello"))
	if addition["contents"] != wantContents {
		t.Fatalf("unexpected addition contents: %v want %v", addition["contents"], wantContents)
	}
	deletions, ok := fileChanges["deletions"].([]any)
	if !ok || len(deletions) != 1 {
		t.Fatalf("unexpected deletions: %#v", fileChanges["deletions"])
	}
	if deletions[0].(map[string]any)["path"] != "b.txt" {
		t.Fatalf("unexpected deletion path: %v", deletions[0])
	}
}

func TestCreateCommitSingleLineMessageHasEmptyBody(t *testing.T) {
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":{"createCommitOnBranch":{"commit":{"oid":"x"}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	if _, err := c.CreateCommit(context.Background(), "o", "r", "b", "h", "Only headline", nil, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := (*captured)[0]
	input := req.Variables["input"].(map[string]any)
	message := input["message"].(map[string]any)
	if message["headline"] != "Only headline" || message["body"] != "" {
		t.Fatalf("unexpected message: %#v", message)
	}
}

func TestCreateCommitBase64OfNonUTF8Bytes(t *testing.T) {
	nonUTF8 := []byte{0xFF, 0xFE, 0x00, 0x80, 0x81}
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":{"createCommitOnBranch":{"commit":{"oid":"x"}}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	if _, err := c.CreateCommit(context.Background(), "o", "r", "b", "h", "binary file",
		[]replay.FileAddition{{Path: "bin.dat", Contents: nonUTF8}}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := (*captured)[0]
	input := req.Variables["input"].(map[string]any)
	fileChanges := input["fileChanges"].(map[string]any)
	additions := fileChanges["additions"].([]any)
	encoded := additions[0].(map[string]any)["contents"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != string(nonUTF8) {
		t.Fatalf("got %v want %v", decoded, nonUTF8)
	}
}

func TestCreateCommitHeadMismatchMapsToTypedError(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"errors":[{"message":"Expected branch to point to 'abc123' but it points to 'def456'","type":"UNPROCESSABLE"}]}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	_, err := c.CreateCommit(context.Background(), "o", "r", "b", "abc123", "msg", nil, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	var mismatch *replay.HeadMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected *replay.HeadMismatchError, got %T: %v", err, err)
	}
	if mismatch.Expected != "abc123" {
		t.Fatalf("unexpected Expected: %q", mismatch.Expected)
	}
	if !strings.Contains(mismatch.Message, "Expected branch to point to") {
		t.Fatalf("unexpected Message: %q", mismatch.Message)
	}
}

func TestCreateCommitOtherGraphQLErrorSurfaced(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"errors":[{"message":"something else went wrong","type":"FORBIDDEN"}]}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	_, err := c.CreateCommit(context.Background(), "o", "r", "b", "h", "msg", nil, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	var mismatch *replay.HeadMismatchError
	if errors.As(err, &mismatch) {
		t.Fatalf("did not expect HeadMismatchError")
	}
	var gqlErr *GraphQLError
	if !errors.As(err, &gqlErr) {
		t.Fatalf("expected *GraphQLError, got %T: %v", err, err)
	}
	if gqlErr.Errors[0].Message != "something else went wrong" {
		t.Fatalf("unexpected message: %q", gqlErr.Errors[0].Message)
	}
	if gqlErr.Errors[0].Type != "FORBIDDEN" {
		t.Fatalf("unexpected type: %q", gqlErr.Errors[0].Type)
	}
}

// ---------- DeleteBranch ----------

func TestDeleteBranch(t *testing.T) {
	calls := 0
	srv, captured := newTestServer(t, func(req capturedRequest) (int, string) {
		calls++
		if calls == 1 {
			if req.Variables["qualifiedName"] != "refs/heads/feature" {
				t.Errorf("unexpected qualifiedName: %v", req.Variables["qualifiedName"])
			}
			return 200, `{"data":{"repository":{"ref":{"id":"REF_xyz"}}}}`
		}
		return 200, `{"data":{"deleteRef":{"clientMutationId":null}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	if err := c.DeleteBranch(context.Background(), "octo", "hello-world", "feature"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req2 := (*captured)[1]
	if req2.Variables["refId"] != "REF_xyz" {
		t.Fatalf("unexpected refId: %v", req2.Variables["refId"])
	}
}

func TestDeleteBranchMissingRefErrors(t *testing.T) {
	srv, _ := newTestServer(t, func(req capturedRequest) (int, string) {
		return 200, `{"data":{"repository":{"ref":null}}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	err := c.DeleteBranch(context.Background(), "octo", "hello-world", "nope")
	if err == nil {
		t.Fatalf("expected error")
	}
}

// ---------- HTTP-level error handling ----------

func TestNon200ResponseIncludesStatusAndBodyPrefix(t *testing.T) {
	srv := fixedResponseServer(t, 500, strings.Repeat("x", 600))
	c := New(http.DefaultTransport, WithEndpoint(srv.URL))
	_, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected status in error, got %v", err)
	}
	// exactly 500 bytes of body prefix, not the whole 600
	if strings.Contains(err.Error(), strings.Repeat("x", 600)) {
		t.Fatalf("expected truncated body, got full body in error")
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 500)) {
		t.Fatalf("expected 500-byte body prefix in error")
	}
}

func TestRedirectRefused(t *testing.T) {
	target := fixedResponseServer(t, 200, `{"data":{"repository":{"ref":null}}}`)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)

	c := New(http.DefaultTransport, WithEndpoint(redirecting.URL))
	_, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error on redirect, got nil")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Fatalf("expected 302 status surfaced, got %v", err)
	}
}

// ---------- FromEnv ----------

func TestFromEnvMissingVariable(t *testing.T) {
	t.Setenv("GITHUB_APP_ID", "")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "")
	os.Unsetenv("GITHUB_APP_ID")
	os.Unsetenv("GITHUB_APP_INSTALLATION_ID")
	os.Unsetenv("GITHUB_APP_PRIVATE_KEY_PATH")

	_, _, _, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Fatalf("expected error naming GITHUB_APP_ID, got %v", err)
	}
}

func TestFromEnvNonNumeric(t *testing.T) {
	t.Setenv("GITHUB_APP_ID", "not-a-number")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "5678")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "/tmp/key.pem")

	_, _, _, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Fatalf("expected error naming GITHUB_APP_ID, got %v", err)
	}
}

func TestFromEnvSuccess(t *testing.T) {
	t.Setenv("GITHUB_APP_ID", "1234")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "5678")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "/tmp/key.pem")

	appID, installationID, keyPath, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if appID != 1234 || installationID != 5678 || keyPath != "/tmp/key.pem" {
		t.Fatalf("got %d %d %q", appID, installationID, keyPath)
	}
}

func TestFromEnvRejectsNonPositiveID(t *testing.T) {
	t.Setenv("GITHUB_APP_ID", "0")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "5678")
	t.Setenv("GITHUB_APP_PRIVATE_KEY_PATH", "/tmp/key.pem")

	_, _, _, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Fatalf("expected error naming GITHUB_APP_ID for a zero id, got %v", err)
	}
}

// ---------- token redaction ----------

type sentinelTokenTransport struct {
	token string
	inner http.RoundTripper
}

func (rt *sentinelTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "token "+rt.token)
	return rt.inner.RoundTrip(req)
}

func TestTokenNeverAppearsInErrorText(t *testing.T) {
	const sentinel = "super-secret-installation-token"
	srv := fixedResponseServer(t, 500, "server exploded")
	rt := &sentinelTokenTransport{token: sentinel, inner: http.DefaultTransport}
	c := New(rt, WithEndpoint(srv.URL))

	_, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("token leaked into error text: %v", err)
	}
}

// ---------- NewAppTransport (no network) ----------

func TestNewAppTransportBuilds(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	rt, err := NewAppTransport(1234, 5678, keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rt == nil {
		t.Fatalf("expected non-nil transport")
	}
}

func TestNewAppTransportBadKeyPath(t *testing.T) {
	_, err := NewAppTransport(1234, 5678, "/does/not/exist.pem")
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestTokenRejectsUnrelatedTransport(t *testing.T) {
	_, err := Token(context.Background(), http.DefaultTransport)
	if err == nil {
		t.Fatalf("expected error for non-ghinstallation transport")
	}
}
