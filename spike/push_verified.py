"""Local-git half of the push_verified spike (P1): range walk, refusal set, local reset.

GitHub/auth/GraphQL live in spike/gh.py (P2); spike/push_verified.py main() is
wired up by P3. See docs/design.md and docs/decisions/0001-0004.
"""

from __future__ import annotations

import re
import subprocess
import sys
import urllib.parse
from dataclasses import dataclass

MAX_COMMIT_BYTES = 40_000_000

_REF_RE = re.compile(r"^[A-Za-z0-9._/-]+$")
_OID_RE = re.compile(r"^[0-9a-fA-F]{4,40}$")
_ALLOWED_MODES = {"100644", "000000"}
_MODE_MISSING = "000000"
_GITHUB_HOSTS = {"github.com"}


class RefusedError(Exception):
    """A commit or remote cannot be represented; nothing was mutated."""


class SyncError(Exception):
    """The local and remote tips disagree after a fetch; refs left untouched."""


@dataclass
class Change:
    oid: str
    message: str
    additions: list[tuple[str, bytes]]
    deletions: list[str]


def _validate_ref_name(name: str, label: str) -> None:
    if not _REF_RE.match(name) or name.startswith("-"):
        raise ValueError(f"invalid {label}: {name!r}")


def _validate_oid(value: str, label: str) -> None:
    if not _OID_RE.match(value):
        raise ValueError(f"invalid {label}: {value!r}")


def _redact_url(url: str) -> str:
    if "://" not in url:
        return url
    parsed = urllib.parse.urlsplit(url)
    if not (parsed.username or parsed.password):
        return url
    netloc = parsed.hostname or ""
    if parsed.port:
        netloc = f"{netloc}:{parsed.port}"
    return urllib.parse.urlunsplit(parsed._replace(netloc=netloc))


def _run(args: list[str], cwd: str, check: bool = True, binary: bool = False):
    if binary:
        return subprocess.run(args, cwd=cwd, check=check, capture_output=True)
    return subprocess.run(args, cwd=cwd, check=check, capture_output=True, text=True)


def _parse_remote_url(url: str) -> tuple[str, str]:
    if "://" not in url:
        m = re.match(r"^[\w.-]+@([^:/]+):(.+)$", url)
        if m:
            return m.group(1), m.group(2)
    parsed = urllib.parse.urlsplit(url)
    if parsed.hostname:
        return parsed.hostname, parsed.path
    raise RefusedError(f"cannot parse remote url {url!r}")


def owner_repo(repo_path: str, remote: str = "origin") -> tuple[str, str]:
    _validate_ref_name(remote, "remote")
    result = _run(["git", "remote", "get-url", remote], cwd=repo_path)
    url = result.stdout.strip()
    host, path = _parse_remote_url(url)
    safe_url = _redact_url(url)
    if host.lower() not in _GITHUB_HOSTS:
        raise RefusedError(f"remote {remote} url {safe_url!r} is not github.com (host {host!r})")
    path = path.strip("/")
    if path.endswith(".git"):
        path = path[: -len(".git")]
    segments = path.split("/")
    if len(segments) != 2 or not all(segments):
        raise RefusedError(f"remote {remote} url {safe_url!r} does not look like owner/repo")
    owner, repo = segments
    return owner, repo


def _remote_branch_exists(repo_path: str, remote: str, branch: str) -> bool:
    result = _run(["git", "ls-remote", "--heads", remote, branch], cwd=repo_path)
    target = f"refs/heads/{branch}"
    for line in result.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) == 2 and parts[1] == target:
            return True
    return False


def commit_range(repo_path: str, branch: str, base: str, remote: str = "origin") -> list[str]:
    _validate_ref_name(branch, "branch")
    _validate_ref_name(base, "base")
    _validate_ref_name(remote, "remote")

    exists = _remote_branch_exists(repo_path, remote, branch)
    refs_to_fetch = [base, branch] if exists else [base]
    _run(["git", "fetch", remote, *refs_to_fetch], cwd=repo_path)

    local_ref = f"refs/heads/{branch}"
    remote_branch_ref = f"refs/remotes/{remote}/{branch}"
    remote_base_ref = f"refs/remotes/{remote}/{base}"

    if exists:
        behind = _run(
            ["git", "rev-list", f"{local_ref}..{remote_branch_ref}"], cwd=repo_path
        ).stdout.split()
        if behind:
            raise RefusedError(f"local branch {branch} is behind {remote}/{branch}")
        range_spec = f"{remote_branch_ref}..{local_ref}"
    else:
        range_spec = f"{remote_base_ref}..{local_ref}"

    result = _run(["git", "rev-list", "--reverse", range_spec], cwd=repo_path)
    return result.stdout.split()


def commit_changes(repo_path: str, oid: str) -> Change:
    _validate_oid(oid, "oid")

    parents_result = _run(["git", "rev-list", "--parents", "-n", "1", oid], cwd=repo_path)
    tokens = parents_result.stdout.split()
    parents = tokens[1:]
    if len(parents) == 0:
        raise RefusedError(f"commit {oid} has zero parents")
    if len(parents) > 1:
        raise RefusedError(f"commit {oid} is a merge commit")
    parent = parents[0]

    message_result = _run(["git", "log", "--format=%B", "-n", "1", oid], cwd=repo_path)
    message = message_result.stdout
    if message.endswith("\n"):
        message = message[:-1]

    diff_result = _run(
        ["git", "diff-tree", "-r", "--no-commit-id", "--no-renames", "--raw", "-z", parent, oid],
        cwd=repo_path,
        binary=True,
    )
    raw_parts = diff_result.stdout.split(b"\0")
    if raw_parts and raw_parts[-1] == b"":
        raw_parts.pop()
    if not raw_parts:
        raise RefusedError(f"commit {oid} is empty")

    additions: list[tuple[str, bytes]] = []
    deletions: list[str] = []
    total_bytes = 0
    it = iter(raw_parts)
    for entry_bytes in it:
        try:
            path_bytes = next(it)
        except StopIteration:
            raise RefusedError(f"commit {oid} has a truncated diff-tree entry") from None

        entry = entry_bytes.decode("ascii")
        try:
            path = path_bytes.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise RefusedError(f"commit {oid} has a non-UTF-8 path: {path_bytes!r}") from exc
        if path.startswith("/") or path == ".git" or path.startswith(".git/"):
            raise RefusedError(f"commit {oid} path {path!r} is unsafe")
        if any(segment == ".." for segment in path.split("/")):
            raise RefusedError(f"commit {oid} path {path!r} is unsafe")

        fields = entry.lstrip(":").split(" ")
        src_mode, dst_mode, src_sha, dst_sha, status = fields[:5]
        if dst_mode not in _ALLOWED_MODES:
            raise RefusedError(f"commit {oid} path {path!r} has disallowed dst mode {dst_mode}")
        if src_mode not in _ALLOWED_MODES:
            raise RefusedError(f"commit {oid} path {path!r} has disallowed src mode {src_mode}")

        if dst_mode == _MODE_MISSING:
            deletions.append(path)
            continue

        blob_result = _run(["git", "cat-file", "blob", dst_sha], cwd=repo_path, binary=True)
        content = blob_result.stdout
        total_bytes += len(content)
        if total_bytes > MAX_COMMIT_BYTES:
            raise RefusedError(
                f"commit {oid} additions exceed MAX_COMMIT_BYTES ({MAX_COMMIT_BYTES})"
            )
        additions.append((path, content))

    return Change(oid=oid, message=message, additions=additions, deletions=deletions)


def reset_local_to_remote(repo_path: str, branch: str, remote: str, old_tip: str) -> None:
    _validate_ref_name(branch, "branch")
    _validate_ref_name(remote, "remote")
    _validate_oid(old_tip, "old_tip")

    local_ref = f"refs/heads/{branch}"

    _run(["git", "fetch", remote, branch], cwd=repo_path)
    remote_ref = f"refs/remotes/{remote}/{branch}"

    current_tip = _run(["git", "rev-parse", local_ref], cwd=repo_path).stdout.strip()
    if current_tip != old_tip:
        raise SyncError(f"{local_ref} moved to {current_tip}, expected {old_tip}")

    diff = _run(["git", "diff", "--quiet", old_tip, remote_ref], cwd=repo_path, check=False)
    if diff.returncode == 1:
        raise SyncError(f"{old_tip} differs from {remote_ref} after replay")
    if diff.returncode != 0:
        raise SyncError(f"could not compare {old_tip} to {remote_ref}: {diff.stderr.strip()}")

    current_branch = _run(["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd=repo_path).stdout.strip()
    if current_branch == branch:
        _run(["git", "reset", "--soft", remote_ref], cwd=repo_path)
    else:
        _run(["git", "update-ref", local_ref, remote_ref, old_tip], cwd=repo_path)


def main(argv: list[str] | None = None) -> int:
    print(
        "usage: push_verified.py <repo_path> <branch> [--base main] [--remote origin]",
        file=sys.stderr,
    )
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
