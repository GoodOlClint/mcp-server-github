import base64
import json

import pytest
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding, rsa

from gh import (
    AppAuth,
    AppAuthError,
    GraphQL,
    GraphQLError,
    GraphQLHTTPError,
    expected_head_mismatch,
)


# ---------- fixtures ----------


@pytest.fixture()
def rsa_key(tmp_path):
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    pem = key.private_bytes(
        encoding=serialization.Encoding.PEM,
        format=serialization.PrivateFormat.PKCS8,
        encryption_algorithm=serialization.NoEncryption(),
    )
    key_path = tmp_path / "app.pem"
    key_path.write_bytes(pem)
    return key, str(key_path)


class FakeResponse:
    def __init__(self, status, body: bytes):
        self.status = status
        self._body = body

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def app_auth_env(monkeypatch, key_path, app_id="1234", install_id="5678"):
    monkeypatch.setenv("GITHUB_APP_ID", app_id)
    monkeypatch.setenv("GITHUB_APP_INSTALLATION_ID", install_id)
    monkeypatch.setenv("GITHUB_APP_PRIVATE_KEY_PATH", key_path)


def make_token_urlopen(token="tok-abc", expires_at="2026-09-03T12:10:00Z", captured=None):
    def urlopen(req):
        if captured is not None:
            captured.append(req)
        body = json.dumps({"token": token, "expires_at": expires_at}).encode("utf-8")
        return FakeResponse(200, body)

    return urlopen


# ---------- AppAuth: env ----------


def test_app_auth_missing_env_names_variable(monkeypatch):
    monkeypatch.delenv("GITHUB_APP_ID", raising=False)
    monkeypatch.delenv("GITHUB_APP_INSTALLATION_ID", raising=False)
    monkeypatch.delenv("GITHUB_APP_PRIVATE_KEY_PATH", raising=False)
    with pytest.raises(AppAuthError, match="GITHUB_APP_ID"):
        AppAuth()


def test_app_auth_missing_one_env_var(monkeypatch, rsa_key):
    _, key_path = rsa_key
    monkeypatch.setenv("GITHUB_APP_ID", "1234")
    monkeypatch.delenv("GITHUB_APP_INSTALLATION_ID", raising=False)
    monkeypatch.setenv("GITHUB_APP_PRIVATE_KEY_PATH", key_path)
    with pytest.raises(AppAuthError, match="GITHUB_APP_INSTALLATION_ID"):
        AppAuth()


# ---------- AppAuth: JWT shape + signature ----------


def test_mint_jwt_header_and_claims_and_signature(monkeypatch, rsa_key):
    key, key_path = rsa_key
    app_auth_env(monkeypatch, key_path, app_id="99")

    clock = lambda: 1000.0
    auth = AppAuth(clock=clock)
    jwt = auth._mint_jwt()

    header_b64, claims_b64, sig_b64 = jwt.split(".")

    def _decode(b64):
        pad = "=" * (-len(b64) % 4)
        return json.loads(base64.urlsafe_b64decode(b64 + pad))

    header = _decode(header_b64)
    claims = _decode(claims_b64)
    assert header == {"alg": "RS256", "typ": "JWT"}
    assert claims == {"iat": 940, "exp": 1540, "iss": "99"}

    signing_input = (header_b64 + "." + claims_b64).encode("ascii")
    sig_pad = "=" * (-len(sig_b64) % 4)
    signature = base64.urlsafe_b64decode(sig_b64 + sig_pad)

    public_key = key.public_key()
    # raises on failure; verifies the token was signed with the matching key
    public_key.verify(signature, signing_input, padding.PKCS1v15(), hashes.SHA256())


def test_mint_jwt_bad_signature_fails_verification(monkeypatch, rsa_key):
    # mutation-test evidence: flip a byte of the signature, verification must fail
    key, key_path = rsa_key
    app_auth_env(monkeypatch, key_path)
    auth = AppAuth(clock=lambda: 1000.0)
    jwt = auth._mint_jwt()
    header_b64, claims_b64, sig_b64 = jwt.split(".")
    pad = "=" * (-len(sig_b64) % 4)
    sig = bytearray(base64.urlsafe_b64decode(sig_b64 + pad))
    sig[0] ^= 0xFF
    signing_input = (header_b64 + "." + claims_b64).encode("ascii")
    with pytest.raises(Exception):
        key.public_key().verify(bytes(sig), signing_input, padding.PKCS1v15(), hashes.SHA256())


# ---------- AppAuth: caching + refresh boundary ----------


def test_token_cached_no_refresh_when_far_from_expiry(monkeypatch, rsa_key):
    _, key_path = rsa_key
    app_auth_env(monkeypatch, key_path)
    calls = []
    now = [1000.0]
    auth = AppAuth(clock=lambda: now[0], urlopen=make_token_urlopen(captured=calls, expires_at="2026-09-03T12:10:00Z"))

    t1 = auth.token()
    assert t1 == "tok-abc"
    assert len(calls) == 1

    t2 = auth.token()
    assert t2 == "tok-abc"
    assert len(calls) == 1  # not re-minted


def test_token_refreshed_within_five_minutes_of_expiry(monkeypatch, rsa_key):
    _, key_path = rsa_key
    app_auth_env(monkeypatch, key_path)
    calls = []

    import calendar
    import time as time_mod

    expires_at_str = "2026-09-03T12:10:00Z"
    expires_epoch = calendar.timegm(time_mod.strptime(expires_at_str, "%Y-%m-%dT%H:%M:%SZ"))

    now = [expires_epoch - 3600]
    auth = AppAuth(clock=lambda: now[0], urlopen=make_token_urlopen(captured=calls, expires_at=expires_at_str))
    auth.token()
    assert len(calls) == 1

    # still outside the 5-minute window
    now[0] = expires_epoch - 301
    auth.token()
    assert len(calls) == 1

    # inside the 5-minute window: must remint
    now[0] = expires_epoch - 300
    auth.token()
    assert len(calls) == 2

    now[0] = expires_epoch - 100
    auth.token()
    assert len(calls) == 3


def test_exchange_http_error_raises_app_auth_error(monkeypatch, rsa_key):
    import urllib.error

    _, key_path = rsa_key
    app_auth_env(monkeypatch, key_path)

    def urlopen(req):
        raise urllib.error.HTTPError(
            req.full_url, 401, "Bad credentials", {}, io_body(b'{"message": "Bad credentials"}')
        )

    auth = AppAuth(clock=lambda: 1000.0, urlopen=urlopen)
    with pytest.raises(AppAuthError, match="401"):
        auth.token()


def io_body(data: bytes):
    import io

    return io.BytesIO(data)


def test_token_never_logged_or_printed(monkeypatch, rsa_key, capsys):
    _, key_path = rsa_key
    app_auth_env(monkeypatch, key_path)
    auth = AppAuth(clock=lambda: 1000.0, urlopen=make_token_urlopen(token="super-secret-token"))
    auth.token()
    captured = capsys.readouterr()
    assert "super-secret-token" not in captured.out
    assert "super-secret-token" not in captured.err


# ---------- GraphQL: error / status handling ----------


class FakeAuth:
    def token(self):
        return "faketoken"


def test_graphql_raises_on_errors_array():
    def urlopen(req):
        body = json.dumps({"errors": [{"message": "boom"}]}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    with pytest.raises(GraphQLError) as exc:
        gql.query("query { x }", {})
    assert exc.value.errors == [{"message": "boom"}]


def test_graphql_raises_on_non_200():
    def urlopen(req):
        return FakeResponse(500, b"x" * 600)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    with pytest.raises(GraphQLHTTPError) as exc:
        gql.query("query { x }", {})
    assert exc.value.status == 500
    assert len(exc.value.body_prefix) == 500


def test_graphql_raises_on_missing_data_and_no_errors():
    def urlopen(req):
        body = json.dumps({"message": "abuse detection triggered"}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    with pytest.raises(GraphQLHTTPError):
        gql.query("query { x }", {})


def test_graphql_success_returns_data():
    def urlopen(req):
        body = json.dumps({"data": {"x": 1}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    data = gql.query("query { x }", {})
    assert data == {"x": 1}


def test_graphql_request_carries_bearer_token_and_json_body():
    captured = {}

    def urlopen(req):
        captured["headers"] = req.headers
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"ok": True}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    gql.query("query { x }", {"a": 1})
    assert captured["headers"]["Authorization"] == "Bearer faketoken"
    assert captured["data"] == {"query": "query { x }", "variables": {"a": 1}}


# ---------- GraphQL helpers ----------


def test_repo_id():
    captured = {}

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"repository": {"id": "R_kg123"}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    result = gql.repo_id("octo", "hello-world")
    assert result == "R_kg123"
    assert captured["data"]["variables"] == {"owner": "octo", "repo": "hello-world"}
    assert "repository(owner: $owner, name: $repo)" in captured["data"]["query"]


def test_branch_head_present():
    def urlopen(req):
        data = json.loads(req.data.decode("utf-8"))
        assert data["variables"]["qualifiedName"] == "refs/heads/main"
        body = json.dumps(
            {"data": {"repository": {"ref": {"target": {"oid": "abc123"}}}}}
        ).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    assert gql.branch_head("octo", "hello-world", "main") == "abc123"


def test_branch_head_missing_returns_none():
    def urlopen(req):
        body = json.dumps({"data": {"repository": {"ref": None}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    assert gql.branch_head("octo", "hello-world", "nope") is None


def test_branch_ref_id_present():
    def urlopen(req):
        data = json.loads(req.data.decode("utf-8"))
        assert data["variables"]["qualifiedName"] == "refs/heads/feature"
        body = json.dumps({"data": {"repository": {"ref": {"id": "REF_1"}}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    assert gql.branch_ref_id("octo", "hello-world", "feature") == "REF_1"


def test_branch_ref_id_missing_returns_none():
    def urlopen(req):
        body = json.dumps({"data": {"repository": {"ref": None}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    assert gql.branch_ref_id("octo", "hello-world", "nope") is None


def test_create_branch():
    captured = {}

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps(
            {"data": {"createRef": {"ref": {"target": {"oid": "newoid1"}}}}}
        ).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    result = gql.create_branch("R_repo1", "feature", "baseoid")
    assert result == "newoid1"
    assert captured["data"]["variables"] == {
        "repositoryId": "R_repo1",
        "name": "refs/heads/feature",
        "oid": "baseoid",
    }


def test_create_commit_request_shape_and_utf8_message():
    captured = {}

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"createCommitOnBranch": {"commit": {"oid": "commitoid1"}}}}).encode(
            "utf-8"
        )
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    result = gql.create_commit(
        owner="octo",
        repo="hello-world",
        branch="feature",
        expected_head_oid="headoid1",
        message="Add file\n\nSome body\nmore body",
        additions=[("a.txt", b"hello")],
        deletions=["b.txt"],
    )
    assert result == "commitoid1"
    inp = captured["data"]["variables"]["input"]
    assert inp["branch"] == {"repositoryNameWithOwner": "octo/hello-world", "branchName": "feature"}
    assert inp["expectedHeadOid"] == "headoid1"
    assert inp["message"] == {"headline": "Add file", "body": "Some body\nmore body"}
    assert inp["fileChanges"]["additions"] == [
        {"path": "a.txt", "contents": base64.b64encode(b"hello").decode("ascii")}
    ]
    assert inp["fileChanges"]["deletions"] == [{"path": "b.txt"}]


def test_create_commit_message_single_line_has_empty_body():
    captured = {}

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"createCommitOnBranch": {"commit": {"oid": "x"}}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    gql.create_commit(
        owner="o",
        repo="r",
        branch="b",
        expected_head_oid="h",
        message="Only headline",
        additions=[],
        deletions=[],
    )
    assert captured["data"]["variables"]["input"]["message"] == {
        "headline": "Only headline",
        "body": "",
    }


def test_create_commit_base64_of_non_utf8_bytes():
    captured = {}
    non_utf8 = bytes([0xFF, 0xFE, 0x00, 0x80, 0x81])

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"createCommitOnBranch": {"commit": {"oid": "x"}}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    gql.create_commit(
        owner="o",
        repo="r",
        branch="b",
        expected_head_oid="h",
        message="binary file",
        additions=[("bin.dat", non_utf8)],
        deletions=[],
    )
    encoded = captured["data"]["variables"]["input"]["fileChanges"]["additions"][0]["contents"]
    assert base64.b64decode(encoded) == non_utf8


def test_delete_branch():
    captured = {}

    def urlopen(req):
        captured["data"] = json.loads(req.data.decode("utf-8"))
        body = json.dumps({"data": {"deleteRef": {"clientMutationId": None}}}).encode("utf-8")
        return FakeResponse(200, body)

    gql = GraphQL(FakeAuth(), urlopen=urlopen)
    gql.delete_branch("REF_xyz")
    assert captured["data"]["variables"] == {"refId": "REF_xyz"}


# ---------- expected_head_mismatch ----------


def test_expected_head_mismatch_positive():
    err = GraphQLError(
        [{"message": "Expected branch to point to 'abc123' but it points to 'def456'"}]
    )
    assert expected_head_mismatch(err) is True


def test_expected_head_mismatch_negative():
    err = GraphQLError([{"message": "Something else went wrong"}])
    assert expected_head_mismatch(err) is False


def test_expected_head_mismatch_empty_errors():
    err = GraphQLError([])
    assert expected_head_mismatch(err) is False
