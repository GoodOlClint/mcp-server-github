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
	"io"
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
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
	Raw    string
}

// newTestServer records every request and answers with what handler returns.
func newTestServer(t *testing.T, handler func(req capturedRequest) (int, string)) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	var captured []capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		req := capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Raw: string(raw)}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.Body); err != nil {
				t.Errorf("decode request body %q: %v", raw, err)
				return
			}
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

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return New(http.DefaultTransport, WithEndpoint(srv.URL))
}

// ---------- headers ----------

func TestEveryRequestCarriesAcceptAndAPIVersion(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 200, `{"object":{"sha":"aa"}}`
	})
	if _, err := newClient(t, srv).BranchHead(context.Background(), "o", "r", "b"); err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	h := (*calls)[0].Header
	if got := h.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := h.Get("X-GitHub-Api-Version"); got != apiVersion {
		t.Fatalf("X-GitHub-Api-Version = %q", got)
	}
}

// ---------- BranchHead ----------

func TestBranchHeadPresent(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 200, `{"ref":"refs/heads/topic","object":{"sha":"abc123","type":"commit"}}`
	})
	got, err := newClient(t, srv).BranchHead(context.Background(), "o", "r", "feature/x")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("head = %q, want abc123", got)
	}
	c := (*calls)[0]
	if c.Method != http.MethodGet {
		t.Fatalf("method = %s", c.Method)
	}
	if c.Path != "/repos/o/r/git/ref/heads/feature/x" {
		t.Fatalf("path = %q", c.Path)
	}
}

func TestBranchHeadMissingReturnsEmpty(t *testing.T) {
	srv := fixedResponseServer(t, 404, `{"message":"Not Found"}`)
	got, err := newClient(t, srv).BranchHead(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if got != "" {
		t.Fatalf("head = %q, want empty", got)
	}
}

func TestBranchHeadOtherStatusIsAnError(t *testing.T) {
	srv := fixedResponseServer(t, 500, `{"message":"boom"}`)
	_, err := newClient(t, srv).BranchHead(context.Background(), "o", "r", "b")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 500 {
		t.Fatalf("expected APIError 500, got %v", err)
	}
}

// ---------- CreateRef / UpdateRef / DeleteBranch ----------

func TestCreateRef(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"ref":"refs/heads/topic","object":{"sha":"base1"}}`
	})
	if err := newClient(t, srv).CreateRef(context.Background(), "o", "r", "topic", "base1"); err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	c := (*calls)[0]
	if c.Method != http.MethodPost || c.Path != "/repos/o/r/git/refs" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	if c.Body["ref"] != "refs/heads/topic" || c.Body["sha"] != "base1" {
		t.Fatalf("body = %v", c.Body)
	}
}

func TestUpdateRefIsNonForce(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 200, `{"object":{"sha":"new1"}}`
	})
	if err := newClient(t, srv).UpdateRef(context.Background(), "o", "r", "spike/a", "new1"); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	c := (*calls)[0]
	if c.Method != http.MethodPatch || c.Path != "/repos/o/r/git/refs/heads/spike/a" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	if c.Body["sha"] != "new1" {
		t.Fatalf("sha = %v", c.Body["sha"])
	}
	if force, ok := c.Body["force"].(bool); !ok || force {
		t.Fatalf("force = %v, want false and present", c.Body["force"])
	}
}

// The message is the one the live API returned on 2026-09-03; the mapping is
// what tells replay a head race from an ordinary rejection.
func TestUpdateRefNonFastForwardMapsToHeadMismatch(t *testing.T) {
	srv := fixedResponseServer(t, 422, `{"message":"Update is not a fast forward","status":"422"}`)
	err := newClient(t, srv).UpdateRef(context.Background(), "o", "r", "b", "new1")
	var mismatch *replay.HeadMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected *replay.HeadMismatchError, got %T: %v", err, err)
	}
	if !strings.Contains(mismatch.Message, "Update is not a fast forward") {
		t.Fatalf("message = %q", mismatch.Message)
	}
}

func TestUpdateRefOther422IsNotAHeadMismatch(t *testing.T) {
	srv := fixedResponseServer(t, 422, `{"message":"Reference does not exist"}`)
	err := newClient(t, srv).UpdateRef(context.Background(), "o", "r", "b", "new1")
	var mismatch *replay.HeadMismatchError
	if errors.As(err, &mismatch) {
		t.Fatalf("unrelated 422 must not map to a head mismatch: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("expected APIError 422, got %v", err)
	}
}

func TestUpdateRefNonFastForwardOnAnotherStatusIsNotAHeadMismatch(t *testing.T) {
	srv := fixedResponseServer(t, 409, `{"message":"Update is not a fast forward"}`)
	err := newClient(t, srv).UpdateRef(context.Background(), "o", "r", "b", "new1")
	var mismatch *replay.HeadMismatchError
	if errors.As(err, &mismatch) {
		t.Fatalf("409 must not map to a head mismatch: %v", err)
	}
}

func TestDeleteBranch(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) { return 204, "" })
	if err := newClient(t, srv).DeleteBranch(context.Background(), "o", "r", "spike/x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	c := (*calls)[0]
	if c.Method != http.MethodDelete || c.Path != "/repos/o/r/git/refs/heads/spike/x" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
}

func TestDeleteBranchMissingRefErrors(t *testing.T) {
	srv := fixedResponseServer(t, 422, `{"message":"Reference does not exist"}`)
	if err := newClient(t, srv).DeleteBranch(context.Background(), "o", "r", "b"); err == nil {
		t.Fatal("expected an error deleting a missing ref")
	}
}

// A "." or ".." segment would be resolved by GitHub's edge and retarget the
// request at another repository under the same installation token.
func TestDotSegmentsAreRefusedBeforeAnyRequest(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		t.Error("a request must not be sent for a rejected path")
		return 200, `{}`
	})
	c := newClient(t, srv)
	ctx := context.Background()
	evil := "../../../../repos/victim/repo/git/refs/heads/main"
	cases := map[string]error{
		"BranchHead":   func() error { _, err := c.BranchHead(ctx, "o", "r", evil); return err }(),
		"UpdateRef":    c.UpdateRef(ctx, "o", "r", evil, "sha"),
		"DeleteBranch": c.DeleteBranch(ctx, "o", "r", evil),
		"CommitTree":   func() error { _, err := c.CommitTree(ctx, "o", "r", ".."); return err }(),
		"owner":        func() error { _, err := c.BranchHead(ctx, "..", "r", "b"); return err }(),
		"repo":         func() error { _, err := c.BranchHead(ctx, "o", "..", "b"); return err }(),
		"empty branch": func() error { _, err := c.BranchHead(ctx, "o", "r", ""); return err }(),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s accepted a dot segment", name)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("sent %d requests for rejected paths", len(*calls))
	}
}

func TestCreateRefCollisionMapsToHeadMismatch(t *testing.T) {
	srv := fixedResponseServer(t, 422, `{"message":"Reference already exists"}`)
	err := newClient(t, srv).CreateRef(context.Background(), "o", "r", "b", "sha1")
	var mismatch *replay.HeadMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expected *replay.HeadMismatchError, got %T: %v", err, err)
	}
}

func TestCreateRefOther422IsNotAHeadMismatch(t *testing.T) {
	srv := fixedResponseServer(t, 422, `{"message":"Invalid request"}`)
	err := newClient(t, srv).CreateRef(context.Background(), "o", "r", "b", "sha1")
	var mismatch *replay.HeadMismatchError
	if errors.As(err, &mismatch) {
		t.Fatalf("unrelated 422 must not map to a head mismatch: %v", err)
	}
	if err == nil {
		t.Fatal("expected an error")
	}
}

// Guessing "blob" for a mode the API does not pair that way would send a tree
// entry GitHub accepts and stores wrong.
func TestCreateTreeRefusesAnUnknownMode(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		t.Error("a request must not be sent for an unknown mode")
		return 201, `{"sha":"t"}`
	})
	sha := "abc"
	_, err := newClient(t, srv).CreateTree(context.Background(), "o", "r", "base",
		[]replay.TreeEntry{{Path: "d", Mode: "040000", SHA: &sha}})
	if err == nil {
		t.Fatal("expected an error for mode 040000")
	}
	if !strings.Contains(err.Error(), "040000") || !strings.Contains(err.Error(), `"d"`) {
		t.Fatalf("want the mode and path named, got %v", err)
	}
	if len(*calls) != 0 {
		t.Fatal("a request was sent despite the unknown mode")
	}
}

// ---------- CommitTree ----------

func TestCommitTree(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 200, `{"sha":"c1","tree":{"sha":"t1"}}`
	})
	got, err := newClient(t, srv).CommitTree(context.Background(), "o", "r", "c1")
	if err != nil {
		t.Fatalf("CommitTree: %v", err)
	}
	if got != "t1" {
		t.Fatalf("tree = %q, want t1", got)
	}
	if (*calls)[0].Path != "/repos/o/r/git/commits/c1" {
		t.Fatalf("path = %q", (*calls)[0].Path)
	}
}

func TestCommitTreeWithoutTreeIsAnError(t *testing.T) {
	srv := fixedResponseServer(t, 200, `{"sha":"c1"}`)
	if _, err := newClient(t, srv).CommitTree(context.Background(), "o", "r", "c1"); err == nil {
		t.Fatal("expected an error when the response carries no tree")
	}
}

// ---------- CreateBlob ----------

func TestCreateBlobSendsBase64(t *testing.T) {
	content := []byte{0x00, 0xff, 'h', 'i', 0xfe}
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"blob1"}`
	})
	got, err := newClient(t, srv).CreateBlob(context.Background(), "o", "r", content)
	if err != nil {
		t.Fatalf("CreateBlob: %v", err)
	}
	if got != "blob1" {
		t.Fatalf("sha = %q", got)
	}
	c := (*calls)[0]
	if c.Method != http.MethodPost || c.Path != "/repos/o/r/git/blobs" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	if c.Body["encoding"] != "base64" {
		t.Fatalf("encoding = %v", c.Body["encoding"])
	}
	decoded, err := base64.StdEncoding.DecodeString(c.Body["content"].(string))
	if err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if string(decoded) != string(content) {
		t.Fatalf("content round-trip = %q, want %q", decoded, content)
	}
}

func TestCreateBlobWithoutShaIsAnError(t *testing.T) {
	srv := fixedResponseServer(t, 201, `{}`)
	if _, err := newClient(t, srv).CreateBlob(context.Background(), "o", "r", []byte("x")); err == nil {
		t.Fatal("expected an error when the blob response carries no sha")
	}
}

// ---------- CreateTree ----------

func TestCreateTreeSendsModesTypesAndNullShaForDeletion(t *testing.T) {
	exe, link, sub := "sha755", "shalink", "shasub"
	entries := []replay.TreeEntry{
		{Path: "a.sh", Mode: "100755", SHA: &exe},
		{Path: "link", Mode: "120000", SHA: &link},
		{Path: "vendor/dep", Mode: "160000", SHA: &sub},
		{Path: "gone.txt", Mode: "100644"},
	}
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"tree1"}`
	})
	got, err := newClient(t, srv).CreateTree(context.Background(), "o", "r", "base1", entries)
	if err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	if got != "tree1" {
		t.Fatalf("tree = %q", got)
	}
	c := (*calls)[0]
	if c.Method != http.MethodPost || c.Path != "/repos/o/r/git/trees" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	if c.Body["base_tree"] != "base1" {
		t.Fatalf("base_tree = %v", c.Body["base_tree"])
	}
	var parsed struct {
		Tree []struct {
			Path string  `json:"path"`
			Mode string  `json:"mode"`
			Type string  `json:"type"`
			SHA  *string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.Unmarshal([]byte(c.Raw), &parsed); err != nil {
		t.Fatalf("decode tree body: %v", err)
	}
	want := []struct {
		path, mode, typ string
		sha             *string
	}{
		{"a.sh", "100755", "blob", &exe},
		{"link", "120000", "blob", &link},
		{"vendor/dep", "160000", "commit", &sub},
		{"gone.txt", "100644", "blob", nil},
	}
	if len(parsed.Tree) != len(want) {
		t.Fatalf("sent %d entries, want %d", len(parsed.Tree), len(want))
	}
	for i, w := range want {
		e := parsed.Tree[i]
		if e.Path != w.path || e.Mode != w.mode || e.Type != w.typ {
			t.Fatalf("entry %d = %+v, want %s %s %s", i, e, w.path, w.mode, w.typ)
		}
		if (e.SHA == nil) != (w.sha == nil) {
			t.Fatalf("entry %d sha = %v, want nil? %v", i, e.SHA, w.sha == nil)
		}
		if w.sha != nil && *e.SHA != *w.sha {
			t.Fatalf("entry %d sha = %q, want %q", i, *e.SHA, *w.sha)
		}
	}
	// A deletion must send an explicit null, not an omitted key.
	if !strings.Contains(c.Raw, `"path":"gone.txt","mode":"100644","type":"blob","sha":null`) {
		t.Fatalf("deletion entry did not send an explicit null sha: %s", c.Raw)
	}
}

// ---------- CreateCommit ----------
// A root commit starts from no tree, and an empty base_tree would name a tree
// that does not exist.
func TestCreateTreeOmitsAnEmptyBaseTree(t *testing.T) {
	sha := "b1"
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"tree1"}`
	})
	if _, err := newClient(t, srv).CreateTree(context.Background(), "o", "r", "",
		[]replay.TreeEntry{{Path: "a", Mode: "100644", SHA: &sha}}); err != nil {
		t.Fatalf("CreateTree: %v", err)
	}
	if strings.Contains((*calls)[0].Raw, "base_tree") {
		t.Fatalf("base_tree must be omitted when empty, got %s", (*calls)[0].Raw)
	}
}

func TestCreateCommitCarriesEveryParent(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"m1"}`
	})
	if _, err := newClient(t, srv).CreateCommit(context.Background(), "o", "r",
		"merge", "t1", []string{"p1", "p2"}); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	parents, ok := (*calls)[0].Body["parents"].([]any)
	if !ok || len(parents) != 2 || parents[0] != "p1" || parents[1] != "p2" {
		t.Fatalf("parents = %v, want both in order", (*calls)[0].Body["parents"])
	}
}

func TestCreateCommitSendsNoAuthorOrCommitter(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"commit1"}`
	})
	got, err := newClient(t, srv).CreateCommit(context.Background(), "o", "r",
		"subject\n\nbody line\n", "tree1", []string{"parent1"})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if got != "commit1" {
		t.Fatalf("sha = %q", got)
	}
	c := (*calls)[0]
	if c.Method != http.MethodPost || c.Path != "/repos/o/r/git/commits" {
		t.Fatalf("call = %s %s", c.Method, c.Path)
	}
	// The whole message goes in one field: splitting it would drop the blank
	// line, and any author or committer field makes GitHub leave it unsigned.
	if c.Body["message"] != "subject\n\nbody line\n" {
		t.Fatalf("message = %q", c.Body["message"])
	}
	if _, ok := c.Body["author"]; ok {
		t.Fatalf("author must never be sent: %v", c.Body)
	}
	if _, ok := c.Body["committer"]; ok {
		t.Fatalf("committer must never be sent: %v", c.Body)
	}
	parents, ok := c.Body["parents"].([]any)
	if !ok || len(parents) != 1 || parents[0] != "parent1" {
		t.Fatalf("parents = %v", c.Body["parents"])
	}
}

func TestCreateCommitEmptyParentsSendsEmptyArray(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 201, `{"sha":"commit1"}`
	})
	if _, err := newClient(t, srv).CreateCommit(context.Background(), "o", "r", "m", "t", nil); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	if !strings.Contains((*calls)[0].Raw, `"parents":[]`) {
		t.Fatalf("nil parents must serialise as [], got %s", (*calls)[0].Raw)
	}
}

func TestCreateCommitWithoutShaIsAnError(t *testing.T) {
	srv := fixedResponseServer(t, 201, `{}`)
	if _, err := newClient(t, srv).CreateCommit(context.Background(), "o", "r", "m", "t", nil); err == nil {
		t.Fatal("expected an error when the commit response carries no sha")
	}
}

// ---------- transport behaviour ----------

func TestNon2xxResponseIncludesStatusAndBodyPrefix(t *testing.T) {
	srv := fixedResponseServer(t, 503, `{"message":"upstream down"}`)
	_, err := newClient(t, srv).CommitTree(context.Background(), "o", "r", "c1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "upstream down") {
		t.Fatalf("error = %v", err)
	}
}

func TestNonJSONErrorBodyStillSurfaces(t *testing.T) {
	srv := fixedResponseServer(t, 502, "<html>bad gateway</html>")
	_, err := newClient(t, srv).CommitTree(context.Background(), "o", "r", "c1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 502 {
		t.Fatalf("expected APIError 502, got %v", err)
	}
	if !strings.Contains(apiErr.Error(), "bad gateway") {
		t.Fatalf("error = %v", apiErr)
	}
}

func TestErrorBodyIsTruncated(t *testing.T) {
	srv := fixedResponseServer(t, 500, strings.Repeat("x", 5000))
	_, err := newClient(t, srv).CommitTree(context.Background(), "o", "r", "c1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if len(apiErr.Body) != 500 {
		t.Fatalf("body length = %d, want 500", len(apiErr.Body))
	}
}

func TestRedirectRefused(t *testing.T) {
	target := fixedResponseServer(t, 200, `{"object":{"sha":"leaked"}}`)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)

	c := New(http.DefaultTransport, WithEndpoint(redirecting.URL))
	got, err := c.BranchHead(context.Background(), "o", "r", "b")
	if err == nil {
		t.Fatalf("expected error on redirect, got %q", got)
	}
	if !strings.Contains(err.Error(), "302") {
		t.Fatalf("expected 302 status surfaced, got %v", err)
	}
}

func TestEndpointOptionTrimsTrailingSlash(t *testing.T) {
	srv, calls := newTestServer(t, func(capturedRequest) (int, string) {
		return 200, `{"object":{"sha":"a"}}`
	})
	c := New(http.DefaultTransport, WithEndpoint(srv.URL+"/"))
	if _, err := c.BranchHead(context.Background(), "o", "r", "b"); err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if (*calls)[0].Path != "/repos/o/r/git/ref/heads/b" {
		t.Fatalf("path = %q", (*calls)[0].Path)
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
