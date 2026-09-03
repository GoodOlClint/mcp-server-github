import os
import subprocess
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
