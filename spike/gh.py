"""GitHub App auth and GraphQL client for the push_verified spike.

Auth: RS256 JWT minted from the App's private key, exchanged for a cached
installation token. GraphQL: a thin urllib client plus the mutations/queries
push_verified needs (repo id, branch head, create branch, create commit,
delete branch).
"""

from __future__ import annotations

import base64
import calendar
import json
import os
import time
import urllib.error
import urllib.request
from typing import Callable, Optional


class AppAuthError(RuntimeError):
    pass


class GraphQLError(RuntimeError):
    def __init__(self, errors: list):
        self.errors = errors
        super().__init__(json.dumps(errors))


class GraphQLHTTPError(RuntimeError):
    def __init__(self, status: int, body_prefix: str):
        self.status = status
        self.body_prefix = body_prefix
        super().__init__(f"GraphQL request failed: HTTP {status}: {body_prefix}")


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _require_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        raise AppAuthError(f"missing required environment variable: {name}")
    return value


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Refuses redirects so an Authorization header is never replayed cross-host."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)
_REQUEST_TIMEOUT = 30


def _default_urlopen(req):
    return _OPENER.open(req, timeout=_REQUEST_TIMEOUT)


class AppAuth:
    """Mints and caches a GitHub App installation token.

    Reads `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, and
    `GITHUB_APP_PRIVATE_KEY_PATH` at construction time. `clock` returns the
    current unix time (injectable for tests). `urlopen` performs the
    token-exchange HTTP request (injectable for tests); defaults to a
    redirect-refusing opener with a request timeout.
    """

    def __init__(
        self,
        clock: Callable[[], float] = time.time,
        urlopen: Callable = _default_urlopen,
    ):
        self.app_id = _require_env("GITHUB_APP_ID")
        self.installation_id = _require_env("GITHUB_APP_INSTALLATION_ID")
        if not self.app_id.isdigit() or not self.installation_id.isdigit():
            raise AppAuthError("GITHUB_APP_ID and GITHUB_APP_INSTALLATION_ID must be numeric")
        self.private_key_path = _require_env("GITHUB_APP_PRIVATE_KEY_PATH")
        self.clock = clock
        self.urlopen = urlopen
        self._token: Optional[str] = None
        self._expires_at: float = 0.0

    def _mint_jwt(self) -> str:
        from cryptography.hazmat.primitives import hashes, serialization
        from cryptography.hazmat.primitives.asymmetric import padding

        with open(self.private_key_path, "rb") as f:
            pem = f.read()
        private_key = serialization.load_pem_private_key(pem, password=None)

        now = int(self.clock())
        header = {"alg": "RS256", "typ": "JWT"}
        claims = {
            "iat": now - 60,
            "exp": now + 540,
            "iss": self.app_id,
        }
        signing_input = (
            _b64url(json.dumps(header, separators=(",", ":")).encode("utf-8"))
            + "."
            + _b64url(json.dumps(claims, separators=(",", ":")).encode("utf-8"))
        )
        signature = private_key.sign(
            signing_input.encode("ascii"),
            padding.PKCS1v15(),
            hashes.SHA256(),
        )
        return signing_input + "." + _b64url(signature)

    def _exchange(self, jwt: str) -> tuple[str, str]:
        url = f"https://api.github.com/app/installations/{self.installation_id}/access_tokens"
        req = urllib.request.Request(
            url,
            method="POST",
            data=b"",
            headers={
                "Authorization": f"Bearer {jwt}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
            },
        )
        try:
            with self.urlopen(req) as resp:
                body = json.loads(resp.read().decode("utf-8"))
            return body["token"], body["expires_at"]
        except urllib.error.HTTPError as e:
            with e:
                detail = e.read()[:500].decode("utf-8", errors="replace")
            raise AppAuthError(f"installation token exchange failed: HTTP {e.code}: {detail}") from None
        except (urllib.error.URLError, ValueError, KeyError) as e:
            raise AppAuthError(f"installation token exchange failed: {e}") from None

    @staticmethod
    def _parse_expiry(expires_at: str) -> float:
        # ISO 8601 Z, e.g. "2026-09-03T12:00:00Z"
        return calendar.timegm(time.strptime(expires_at, "%Y-%m-%dT%H:%M:%SZ"))

    def token(self) -> str:
        now = self.clock()
        if self._token is None or now >= self._expires_at - 300:
            jwt = self._mint_jwt()
            token, expires_at = self._exchange(jwt)
            expiry = self._parse_expiry(expires_at)
            self._token, self._expires_at = token, expiry
        return self._token


class GraphQL:
    def __init__(
        self,
        auth: AppAuth,
        endpoint: str = "https://api.github.com/graphql",
        urlopen: Optional[Callable] = None,
    ):
        self.auth = auth
        self.endpoint = endpoint
        self.urlopen = urlopen or _default_urlopen

    def query(self, query: str, variables: dict) -> dict:
        payload = json.dumps({"query": query, "variables": variables}).encode("utf-8")
        req = urllib.request.Request(
            self.endpoint,
            method="POST",
            data=payload,
            headers={
                "Authorization": f"Bearer {self.auth.token()}",
                "Content-Type": "application/json",
                "Accept": "application/vnd.github+json",
            },
        )
        try:
            with self.urlopen(req) as resp:
                status = resp.status
                body = resp.read()
        except urllib.error.HTTPError as e:
            with e:
                status = e.code
                body = e.read()

        if status != 200:
            raise GraphQLHTTPError(status, body[:500].decode("utf-8", errors="replace"))

        parsed = json.loads(body.decode("utf-8"))
        if parsed.get("errors"):
            raise GraphQLError(parsed["errors"])
        if parsed.get("data") is None:
            raise GraphQLHTTPError(status, body[:500].decode("utf-8", errors="replace"))
        return parsed["data"]

    def repo_id(self, owner: str, repo: str) -> str:
        q = """
        query($owner: String!, $repo: String!) {
          repository(owner: $owner, name: $repo) { id }
        }
        """
        data = self.query(q, {"owner": owner, "repo": repo})
        return data["repository"]["id"]

    def branch_head(self, owner: str, repo: str, branch: str) -> Optional[str]:
        q = """
        query($owner: String!, $repo: String!, $qualifiedName: String!) {
          repository(owner: $owner, name: $repo) {
            ref(qualifiedName: $qualifiedName) { target { oid } }
          }
        }
        """
        data = self.query(
            q,
            {"owner": owner, "repo": repo, "qualifiedName": f"refs/heads/{branch}"},
        )
        ref = data["repository"]["ref"]
        if ref is None:
            return None
        return ref["target"]["oid"]

    def branch_ref_id(self, owner: str, repo: str, branch: str) -> Optional[str]:
        q = """
        query($owner: String!, $repo: String!, $qualifiedName: String!) {
          repository(owner: $owner, name: $repo) {
            ref(qualifiedName: $qualifiedName) { id }
          }
        }
        """
        data = self.query(
            q,
            {"owner": owner, "repo": repo, "qualifiedName": f"refs/heads/{branch}"},
        )
        ref = data["repository"]["ref"]
        if ref is None:
            return None
        return ref["id"]

    def create_branch(self, repo_id: str, branch: str, from_oid: str) -> str:
        m = """
        mutation($repositoryId: ID!, $name: String!, $oid: GitObjectID!) {
          createRef(input: {repositoryId: $repositoryId, name: $name, oid: $oid}) {
            ref { target { oid } }
          }
        }
        """
        data = self.query(
            m,
            {
                "repositoryId": repo_id,
                "name": f"refs/heads/{branch}",
                "oid": from_oid,
            },
        )
        return data["createRef"]["ref"]["target"]["oid"]

    def create_commit(
        self,
        owner: str,
        repo: str,
        branch: str,
        expected_head_oid: str,
        message: str,
        additions: list,
        deletions: list,
    ) -> str:
        lines = message.split("\n", 1)
        headline = lines[0]
        body = lines[1].strip() if len(lines) > 1 else ""

        m = """
        mutation($input: CreateCommitOnBranchInput!) {
          createCommitOnBranch(input: $input) {
            commit { oid }
          }
        }
        """
        variables = {
            "input": {
                "branch": {
                    "repositoryNameWithOwner": f"{owner}/{repo}",
                    "branchName": branch,
                },
                "expectedHeadOid": expected_head_oid,
                "message": {"headline": headline, "body": body},
                "fileChanges": {
                    "additions": [
                        {
                            "path": path,
                            "contents": base64.b64encode(
                                contents if isinstance(contents, (bytes, bytearray)) else contents.encode("utf-8")
                            ).decode("ascii"),
                        }
                        for path, contents in additions
                    ],
                    "deletions": [{"path": path} for path in deletions],
                },
            }
        }
        data = self.query(m, variables)
        return data["createCommitOnBranch"]["commit"]["oid"]

    def delete_branch(self, ref_id: str) -> None:
        m = """
        mutation($refId: ID!) {
          deleteRef(input: {refId: $refId}) {
            clientMutationId
          }
        }
        """
        self.query(m, {"refId": ref_id})


def expected_head_mismatch(err: GraphQLError) -> bool:
    for e in err.errors:
        msg = e.get("message", "") if isinstance(e, dict) else ""
        if "Expected branch to point to" in msg:
            return True
    return False
