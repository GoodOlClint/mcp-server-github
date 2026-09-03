import os
import subprocess
import urllib.error
from dataclasses import dataclass
from pathlib import Path

import pytest

import push_verified as pv

_GIT_ENV = {
    "GIT_AUTHOR_NAME": "Test",
    "GIT_AUTHOR_EMAIL": "test@example.com",
    "GIT_COMMITTER_NAME": "Test",
    "GIT_COMMITTER_EMAIL": "test@example.com",
}


def _git(repo_path, *args, env=None, check=True):
    full_env = {**os.environ, **_GIT_ENV, **(env or {})}
    return subprocess.run(
        ["git", "-C", str(repo_path), *args],
        check=check,
        capture_output=True,
        text=True,
        env=full_env,
    )


def _commit(repo_path, files, message, env=None):
    for path, content in files.items():
        full = Path(repo_path) / path
        if content is None:
            full.unlink()
            _git(repo_path, "rm", path)
        else:
            full.parent.mkdir(parents=True, exist_ok=True)
            if isinstance(content, bytes):
                full.write_bytes(content)
            else:
                full.write_text(content)
            _git(repo_path, "add", path)
    _git(repo_path, "commit", "-m", message, env=env)
    return _git(repo_path, "rev-parse", "HEAD").stdout.strip()


@dataclass
class RepoFixture:
    clone: Path
    remote: Path


@pytest.fixture
def repo(tmp_path):
    remote_dir = tmp_path / "remote.git"
    clone_dir = tmp_path / "clone"
    subprocess.run(
        ["git", "init", "--bare", "-b", "main", str(remote_dir)], check=True, capture_output=True
    )
    subprocess.run(["git", "clone", str(remote_dir), str(clone_dir)], check=True, capture_output=True)
    _git(clone_dir, "config", "user.name", "Test")
    _git(clone_dir, "config", "user.email", "test@example.com")
    (clone_dir / "README.md").write_text("seed\n")
    _git(clone_dir, "add", "README.md")
    _git(clone_dir, "commit", "-m", "seed")
    _git(clone_dir, "push", "origin", "main")
    return RepoFixture(clone=clone_dir, remote=remote_dir)


def _extra_clone(repo_fixture, name):
    clone = repo_fixture.remote.parent / name
    subprocess.run(["git", "clone", str(repo_fixture.remote), str(clone)], check=True, capture_output=True)
    _git(clone, "config", "user.name", "Test")
    _git(clone, "config", "user.email", "test@example.com")
    return clone


# -- commit_range -------------------------------------------------------


def test_commit_range_two_commits_oldest_first(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    oid1 = _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    oid2 = _commit(repo.clone, {"b.txt": "b\n"}, "add b")
    assert pv.commit_range(str(repo.clone), "feature", "main", "origin") == [oid1, oid2]


def test_commit_range_remote_has_first_commit(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    _git(repo.clone, "push", "origin", "feature")
    oid2 = _commit(repo.clone, {"b.txt": "b\n"}, "add b")
    assert pv.commit_range(str(repo.clone), "feature", "main", "origin") == [oid2]


def test_commit_range_refuses_when_local_behind_remote(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    _git(repo.clone, "push", "origin", "feature")

    other = _extra_clone(repo, "other_behind")
    _git(other, "checkout", "feature")
    _commit(other, {"b.txt": "b\n"}, "add b")
    _git(other, "push", "origin", "feature")

    with pytest.raises(pv.RefusedError):
        pv.commit_range(str(repo.clone), "feature", "main", "origin")


def test_commit_range_rejects_option_like_branch(repo):
    with pytest.raises(ValueError):
        pv.commit_range(str(repo.clone), "--upload-pack=x", "main", "origin")


# -- commit_changes -------------------------------------------------------


def test_commit_changes_rejects_option_like_oid(repo):
    with pytest.raises(ValueError):
        pv.commit_changes(str(repo.clone), "--output=/tmp/pwned")


def test_commit_changes_additions_and_message(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    oid = _commit(repo.clone, {"a.txt": "hello\n", "dir/b.txt": "world\n"}, "add files\n\nbody line")
    change = pv.commit_changes(str(repo.clone), oid)
    assert change.oid == oid
    assert change.message == "add files\n\nbody line\n"
    assert change.deletions == []
    content = dict(change.additions)
    assert content["a.txt"] == b"hello\n"
    assert content["dir/b.txt"] == b"world\n"


def test_commit_changes_deletion(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "hello\n"}, "add a")
    oid = _commit(repo.clone, {"a.txt": None}, "remove a")
    change = pv.commit_changes(str(repo.clone), oid)
    assert change.additions == []
    assert change.deletions == ["a.txt"]


def test_commit_changes_non_utf8_bytes(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    payload = b"\xff\xfe\x00\x01binary"
    oid = _commit(repo.clone, {"bin.dat": payload}, "add binary")
    change = pv.commit_changes(str(repo.clone), oid)
    assert dict(change.additions)["bin.dat"] == payload


def test_commit_changes_refuses_executable_mode(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    path = repo.clone / "run.sh"
    path.write_text("#!/bin/sh\necho hi\n")
    os.chmod(path, 0o755)
    _git(repo.clone, "add", "run.sh")
    _git(repo.clone, "commit", "-m", "add script")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    with pytest.raises(pv.RefusedError):
        pv.commit_changes(str(repo.clone), oid)


def test_commit_changes_refuses_symlink(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"target.txt": "data\n"}, "add target")
    link = repo.clone / "link.txt"
    os.symlink("target.txt", link)
    _git(repo.clone, "add", "link.txt")
    _git(repo.clone, "commit", "-m", "add link")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    with pytest.raises(pv.RefusedError):
        pv.commit_changes(str(repo.clone), oid)


def test_commit_changes_refuses_merge_commit(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    _git(repo.clone, "checkout", "main")
    _git(repo.clone, "checkout", "-b", "other")
    _commit(repo.clone, {"b.txt": "b\n"}, "add b")
    _git(repo.clone, "checkout", "feature")
    _git(repo.clone, "merge", "--no-ff", "-m", "merge other", "other")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    with pytest.raises(pv.RefusedError):
        pv.commit_changes(str(repo.clone), oid)


def test_commit_changes_refuses_empty_commit(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _git(repo.clone, "commit", "--allow-empty", "-m", "empty")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    with pytest.raises(pv.RefusedError):
        pv.commit_changes(str(repo.clone), oid)


# -- reset_local_to_remote -------------------------------------------------------


def test_reset_local_to_remote_checked_out(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    old_tip = _commit(repo.clone, {"a.txt": "a\n"}, "add a")

    other = _extra_clone(repo, "other_checked_out")
    _git(other, "checkout", "-b", "feature")
    replay_env = {"GIT_AUTHOR_DATE": "2020-01-01T00:00:00", "GIT_COMMITTER_DATE": "2020-01-01T00:00:00"}
    _commit(other, {"a.txt": "a\n"}, "add a", env=replay_env)
    _git(other, "push", "origin", "feature")

    pv.reset_local_to_remote(str(repo.clone), "feature", "origin", old_tip)

    head = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    remote_head = _git(repo.clone, "rev-parse", "origin/feature").stdout.strip()
    assert head == remote_head
    assert head != old_tip


def test_reset_local_to_remote_not_checked_out(repo):
    _git(repo.clone, "checkout", "-b", "feature2")
    old_tip = _commit(repo.clone, {"c.txt": "c\n"}, "add c")
    _git(repo.clone, "checkout", "main")

    other = _extra_clone(repo, "other_not_checked_out")
    _git(other, "checkout", "-b", "feature2")
    replay_env = {"GIT_AUTHOR_DATE": "2020-01-01T00:00:00", "GIT_COMMITTER_DATE": "2020-01-01T00:00:00"}
    _commit(other, {"c.txt": "c\n"}, "add c", env=replay_env)
    _git(other, "push", "origin", "feature2")

    pv.reset_local_to_remote(str(repo.clone), "feature2", "origin", old_tip)

    feature2_ref = _git(repo.clone, "rev-parse", "feature2").stdout.strip()
    remote_ref = _git(repo.clone, "rev-parse", "origin/feature2").stdout.strip()
    current_branch = _git(repo.clone, "rev-parse", "--abbrev-ref", "HEAD").stdout.strip()
    assert current_branch == "main"
    assert feature2_ref == remote_ref
    assert feature2_ref != old_tip


def test_reset_local_to_remote_rejects_option_like_old_tip(repo):
    _git(repo.clone, "checkout", "-b", "feature4")
    _commit(repo.clone, {"e.txt": "e\n"}, "add e")
    with pytest.raises(ValueError):
        pv.reset_local_to_remote(str(repo.clone), "feature4", "origin", "--output=/tmp/pwned")


def test_reset_local_to_remote_refuses_when_branch_moved_past_old_tip(repo):
    _git(repo.clone, "checkout", "-b", "feature5")
    old_tip = _commit(repo.clone, {"f.txt": "f\n"}, "add f")

    other = _extra_clone(repo, "other_moved_tip")
    _git(other, "checkout", "-b", "feature5")
    replay_env = {"GIT_AUTHOR_DATE": "2020-01-01T00:00:00", "GIT_COMMITTER_DATE": "2020-01-01T00:00:00"}
    _commit(other, {"f.txt": "f\n"}, "add f", env=replay_env)
    _git(other, "push", "origin", "feature5")

    # a new local commit lands after old_tip was captured, before the reset runs
    _commit(repo.clone, {"g.txt": "g\n"}, "add g")
    before = _git(repo.clone, "rev-parse", "feature5").stdout.strip()

    with pytest.raises(pv.SyncError):
        pv.reset_local_to_remote(str(repo.clone), "feature5", "origin", old_tip)

    after = _git(repo.clone, "rev-parse", "feature5").stdout.strip()
    assert before == after


def test_reset_local_to_remote_sync_error_leaves_refs_alone(repo):
    _git(repo.clone, "checkout", "-b", "feature3")
    old_tip = _commit(repo.clone, {"d.txt": "d\n"}, "add d")

    other = _extra_clone(repo, "other_sync_error")
    _git(other, "checkout", "-b", "feature3")
    _commit(other, {"d.txt": "different\n"}, "add different d")
    _git(other, "push", "origin", "feature3")

    before = _git(repo.clone, "rev-parse", "feature3").stdout.strip()
    with pytest.raises(pv.SyncError):
        pv.reset_local_to_remote(str(repo.clone), "feature3", "origin", old_tip)
    after = _git(repo.clone, "rev-parse", "feature3").stdout.strip()
    assert before == after


# -- owner_repo -------------------------------------------------------


def _init_with_remote(tmp_path, name, url):
    repo_dir = tmp_path / name
    subprocess.run(["git", "init", str(repo_dir)], check=True, capture_output=True)
    _git(repo_dir, "remote", "add", "origin", url)
    return repo_dir


def test_owner_repo_ssh_shorthand(tmp_path):
    repo_dir = _init_with_remote(tmp_path, "r1", "git@github.com:someorg/somerepo.git")
    assert pv.owner_repo(str(repo_dir)) == ("someorg", "somerepo")


def test_owner_repo_ssh_url(tmp_path):
    repo_dir = _init_with_remote(tmp_path, "r2", "ssh://git@github.com/someorg/somerepo.git")
    assert pv.owner_repo(str(repo_dir)) == ("someorg", "somerepo")


def test_owner_repo_https_url(tmp_path):
    repo_dir = _init_with_remote(tmp_path, "r3", "https://github.com/someorg/somerepo")
    assert pv.owner_repo(str(repo_dir)) == ("someorg", "somerepo")


def test_owner_repo_refuses_non_github_host(tmp_path):
    repo_dir = _init_with_remote(tmp_path, "r4", "git@gitlab.com:someorg/somerepo.git")
    with pytest.raises(pv.RefusedError):
        pv.owner_repo(str(repo_dir))


# -- main -------------------------------------------------------


def test_main_prints_usage_and_exits_2(capsys):
    assert pv.main([]) == 2
    captured = capsys.readouterr()
    assert "usage" in captured.err


def test_commit_changes_allows_deleting_executable(repo):
    _git(repo.clone, "checkout", "-b", "feature")
    path = repo.clone / "run.sh"
    path.write_text("#!/bin/sh\necho hi\n")
    os.chmod(path, 0o755)
    _git(repo.clone, "add", "run.sh")
    _git(repo.clone, "commit", "-m", "add script")
    path.unlink()
    _git(repo.clone, "rm", "run.sh")
    _git(repo.clone, "commit", "-m", "drop script")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()
    change = pv.commit_changes(str(repo.clone), oid)
    assert change.additions == []
    assert change.deletions == ["run.sh"]


# -- push -------------------------------------------------------

import gh  # noqa: E402


_REPLAY_ENV = {
    "GIT_AUTHOR_DATE": "2020-01-01T00:00:00",
    "GIT_COMMITTER_DATE": "2020-01-01T00:00:00",
}


class FakeGraphQL:
    """Replays mutations into a scratch clone of the bare remote.

    Mirrors the gh.GraphQL surface push() uses, so the local reset and the
    tree equality it asserts are exercised for real.
    """

    def __init__(
        self, remote_path, work_dir, advance_at=None, advance_before_head=False, corrupt=False
    ):
        self.remote_path = remote_path
        self.work_dir = work_dir
        self.corrupt = corrupt
        self.advance_at = advance_at
        self.advance_before_head = advance_before_head
        self.calls = []
        self._clone = None
        self._advanced = 0

    def _advance_remote(self, branch):
        """Lands a foreign commit on the remote branch, as a third party would."""
        clone = self._mirror()
        _git(clone, "checkout", "-B", branch, f"origin/{branch}")
        self._advanced += 1
        (clone / f"foreign{self._advanced}.txt").write_text("foreign\n")
        _git(clone, "add", f"foreign{self._advanced}.txt")
        _git(clone, "commit", "-m", "foreign commit", env=_REPLAY_ENV)
        _git(clone, "push", "origin", branch)

    def _mirror(self):
        if self._clone is None:
            self._clone = self.work_dir / "fake_remote_clone"
            subprocess.run(
                ["git", "clone", str(self.remote_path), str(self._clone)],
                check=True,
                capture_output=True,
            )
            _git(self._clone, "config", "user.name", "Bot")
            _git(self._clone, "config", "user.email", "bot@example.com")
        _git(self._clone, "fetch", "origin")
        return self._clone

    def repo_id(self, owner, repo):
        self.calls.append(("repo_id", owner, repo))
        return "R_fake"

    def branch_head(self, owner, repo, branch):
        self.calls.append(("branch_head", branch))
        if self.advance_before_head:
            self.advance_before_head = False
            self._advance_remote(branch)
        result = subprocess.run(
            ["git", "-C", str(self.remote_path), "rev-parse", "--verify", f"refs/heads/{branch}"],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            return None
        return result.stdout.strip()

    def create_branch(self, repo_id, branch, from_oid):
        self.calls.append(("create_branch", branch, from_oid))
        clone = self._mirror()
        _git(clone, "branch", branch, from_oid)
        _git(clone, "push", "origin", branch)
        return from_oid

    def create_commit(self, owner, repo, branch, expected_head_oid, message, additions, deletions):
        self.calls.append(("create_commit", branch, expected_head_oid, message))
        create_calls = sum(1 for c in self.calls if c[0] == "create_commit")
        if self.advance_at == create_calls:
            self._advance_remote(branch)
        clone = self._mirror()
        _git(clone, "checkout", "-B", branch, f"origin/{branch}")
        head = _git(clone, "rev-parse", "HEAD").stdout.strip()
        if head != expected_head_oid:
            raise gh.GraphQLError(
                [{"message": f"Expected branch to point to {expected_head_oid} but it did not"}]
            )
        for path in deletions:
            (clone / path).unlink()
            _git(clone, "rm", "--cached", path)
        for path, content in additions:
            full = clone / path
            full.parent.mkdir(parents=True, exist_ok=True)
            full.write_bytes(content + b"corrupt\n" if self.corrupt else content)
            _git(clone, "add", path)
        _git(clone, "commit", "-m", message, env=_REPLAY_ENV)
        _git(clone, "push", "origin", branch)
        return _git(clone, "rev-parse", "HEAD").stdout.strip()


@pytest.fixture
def fake_owner_repo(monkeypatch):
    monkeypatch.setattr(pv, "owner_repo", lambda repo_path, remote="origin": ("someorg", "somerepo"))


def _log_range(clone, spec):
    return _git(clone, "log", "--format=%H", spec).stdout.split()


def test_push_happy_path_two_commits(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "seed feature")
    _git(repo.clone, "push", "origin", "feature")
    oid1 = _commit(repo.clone, {"a.txt": "a2\n"}, "change a")
    oid2 = _commit(repo.clone, {"b.txt": "b\n"}, "add b\n\nbody")

    fake = FakeGraphQL(repo.remote, tmp_path)
    pairs = pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert [p[0] for p in pairs] == [oid1, oid2]
    assert all(local != remote_oid for local, remote_oid in pairs)
    assert not any(c[0] == "create_branch" for c in fake.calls)
    assert sum(1 for c in fake.calls if c[0] == "create_commit") == 2

    head = _git(repo.clone, "rev-parse", "feature").stdout.strip()
    assert head == pairs[-1][1]
    assert head == _git(repo.clone, "rev-parse", "origin/feature").stdout.strip()
    assert _log_range(repo.clone, "origin/feature..feature") == []
    assert _git(repo.clone, "diff", "origin/feature", check=False).stdout == ""


def test_push_creates_remote_branch_when_absent(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    oid1 = _commit(repo.clone, {"a.txt": "a\n"}, "add a")

    fake = FakeGraphQL(repo.remote, tmp_path)
    pairs = pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert [p[0] for p in pairs] == [oid1]
    created = [c for c in fake.calls if c[0] == "create_branch"]
    assert len(created) == 1
    main_oid = _git(repo.clone, "rev-parse", "origin/main").stdout.strip()
    assert created[0][1:] == ("feature", main_oid)
    assert _log_range(repo.clone, "origin/feature..feature") == []


def test_push_head_mismatch_on_second_commit_reports_first_pair(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    oid1 = _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    _commit(repo.clone, {"b.txt": "b\n"}, "add b")
    before = _git(repo.clone, "rev-parse", "feature").stdout.strip()

    fake = FakeGraphQL(repo.remote, tmp_path, advance_at=2)
    with pytest.raises(pv.SyncError) as excinfo:
        pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert len(excinfo.value.pairs) == 1
    assert excinfo.value.pairs[0][0] == oid1
    assert excinfo.value.pairs[0][1] in str(excinfo.value)
    assert sum(1 for c in fake.calls if c[0] == "create_commit") == 2
    assert _git(repo.clone, "rev-parse", "feature").stdout.strip() == before


def test_push_refusal_on_later_commit_sends_no_mutations(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    script = repo.clone / "run.sh"
    script.write_text("#!/bin/sh\n")
    os.chmod(script, 0o755)
    _git(repo.clone, "add", "run.sh")
    _git(repo.clone, "commit", "-m", "add script")
    before = _git(repo.clone, "rev-parse", "feature").stdout.strip()

    fake = FakeGraphQL(repo.remote, tmp_path)
    with pytest.raises(pv.RefusedError) as excinfo:
        pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert "run.sh" in str(excinfo.value)
    assert [c for c in fake.calls if c[0] in ("create_commit", "create_branch")] == []
    assert _git(repo.clone, "rev-parse", "feature").stdout.strip() == before


def test_push_rerun_after_success_sends_no_mutations(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")

    fake = FakeGraphQL(repo.remote, tmp_path)
    pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)
    mutations = [c for c in fake.calls if c[0] in ("create_commit", "create_branch")]

    assert pv.push(str(repo.clone), "feature", "main", "origin", gql=fake) == []
    assert [c for c in fake.calls if c[0] in ("create_commit", "create_branch")] == mutations


def test_push_accepts_deletion_of_executable(repo, tmp_path, fake_owner_repo):
    script = repo.clone / "run.sh"
    script.write_text("#!/bin/sh\necho hi\n")
    os.chmod(script, 0o755)
    _git(repo.clone, "add", "run.sh")
    _git(repo.clone, "commit", "-m", "add script")
    _git(repo.clone, "push", "origin", "main")

    _git(repo.clone, "checkout", "-b", "feature")
    script.unlink()
    _git(repo.clone, "rm", "run.sh")
    _git(repo.clone, "commit", "-m", "drop script")
    oid = _git(repo.clone, "rev-parse", "HEAD").stdout.strip()

    fake = FakeGraphQL(repo.remote, tmp_path)
    pairs = pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert [p[0] for p in pairs] == [oid]
    assert _log_range(repo.clone, "origin/feature..feature") == []
    listing = _git(repo.clone, "ls-tree", "--name-only", "origin/feature").stdout.split()
    assert "run.sh" not in listing


def test_commit_changes_refuses_oversize_additions(repo, monkeypatch):
    _git(repo.clone, "checkout", "-b", "feature")
    oid = _commit(repo.clone, {"big.bin": b"x" * 4096}, "add big")
    monkeypatch.setattr(pv, "MAX_COMMIT_BYTES", 1024)
    with pytest.raises(pv.RefusedError) as excinfo:
        pv.commit_changes(str(repo.clone), oid)
    assert "MAX_COMMIT_BYTES" in str(excinfo.value)


def test_main_reports_github_request_failure_as_5(repo, monkeypatch, capsys):
    def boom(*args, **kwargs):
        raise urllib.error.URLError("write operation timed out")

    monkeypatch.setattr(pv, "push", boom)
    assert pv.main([str(repo.clone), "feature"]) == 5
    assert "timed out" in capsys.readouterr().err


def test_push_refuses_when_remote_moved_before_first_mutation(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    _commit(repo.clone, {"a.txt": "a\n"}, "seed feature")
    _git(repo.clone, "push", "origin", "feature")
    _commit(repo.clone, {"b.txt": "b\n"}, "add b")
    before = _git(repo.clone, "rev-parse", "feature").stdout.strip()

    fake = FakeGraphQL(repo.remote, tmp_path, advance_before_head=True)
    with pytest.raises(pv.SyncError):
        pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert [c for c in fake.calls if c[0] == "create_commit"] == []
    assert _git(repo.clone, "rev-parse", "feature").stdout.strip() == before


def test_push_creates_remote_branch_at_the_fork_point(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    fork_point = _git(repo.clone, "rev-parse", "origin/main").stdout.strip()
    _commit(repo.clone, {"a.txt": "a\n"}, "add a")

    other = _extra_clone(repo, "other_main_moved")
    _commit(other, {"m.txt": "m\n"}, "main moves")
    _git(other, "push", "origin", "main")

    fake = FakeGraphQL(repo.remote, tmp_path)
    pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    created = [c for c in fake.calls if c[0] == "create_branch"]
    assert created[0][2] == fork_point
    assert _git(repo.clone, "diff", "origin/feature", check=False).stdout == ""
    assert _log_range(repo.clone, "origin/feature..feature") == []


def test_main_exit_codes_for_refusal_and_sync(repo, monkeypatch, capsys):
    def refuse(*args, **kwargs):
        raise pv.RefusedError("path 'run.sh' has disallowed dst mode 100755")

    monkeypatch.setattr(pv, "push", refuse)
    assert pv.main([str(repo.clone), "feature"]) == 3
    assert "run.sh" in capsys.readouterr().err

    def desync(*args, **kwargs):
        raise pv.SyncError("head moved", [("local", "remote")])

    monkeypatch.setattr(pv, "push", desync)
    assert pv.main([str(repo.clone), "feature"]) == 4
    assert "head moved" in capsys.readouterr().err


def test_main_reports_git_failure_without_traceback(repo, capsys, fake_owner_repo):
    assert pv.main([str(repo.clone), "no-such-branch"]) == 2
    assert "git command failed" in capsys.readouterr().err


def test_main_reports_missing_repo_path(tmp_path, capsys):
    assert pv.main([str(tmp_path / "nope"), "feature"]) == 2
    assert "cannot run git" in capsys.readouterr().err


def test_main_refuses_non_github_remote_before_fetching(repo, capsys):
    assert pv.main([str(repo.clone), "feature"]) == 3
    assert "cannot parse remote url" in capsys.readouterr().err


def test_push_reports_pairs_when_the_local_reset_fails(repo, tmp_path, fake_owner_repo):
    _git(repo.clone, "checkout", "-b", "feature")
    oid = _commit(repo.clone, {"a.txt": "a\n"}, "add a")
    before = _git(repo.clone, "rev-parse", "feature").stdout.strip()

    fake = FakeGraphQL(repo.remote, tmp_path, corrupt=True)
    with pytest.raises(pv.SyncError) as excinfo:
        pv.push(str(repo.clone), "feature", "main", "origin", gql=fake)

    assert [p[0] for p in excinfo.value.pairs] == [oid]
    assert _git(repo.clone, "rev-parse", "feature").stdout.strip() == before
