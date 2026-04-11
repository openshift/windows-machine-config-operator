#!/usr/bin/env python3
"""
create-release-tag.py — Create an annotated release tag for WMCO.

The commit SHA is resolved from the bundle image in the Red Hat Container
Catalog, preferring the org.opencontainers.image.revision label (full SHA)
and falling back to the short-SHA image tag.  The tag date is set to the
operator image's push_date so the timestamp reflects the actual release date.

Usage:
    python3 hack/create-release-tag.py <version>
    python3 hack/create-release-tag.py <version> --commit <SHA>
    python3 hack/create-release-tag.py <version> --date YYYY-MM-DD
    python3 hack/create-release-tag.py <version> --commit <SHA> \
        --date YYYY-MM-DD

    --commit is required when the version has no entry in the bundle catalog
    (e.g. backport releases that were never shipped as a container image).

Note:
    Pushing tags to the upstream openshift/windows-machine-config-operator
    repository requires write access granted by the repository administrators.
    Contact the WMCO team to have your GitHub account added before pushing.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
DATA SOURCES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Red Hat Container Catalog  (catalog.redhat.com)
    Bundle image:    openshift4-wincw/windows-machine-config-operator-bundle
    Provides: build commit SHA.  Newer images carry the full 40-character SHA
    in the org.opencontainers.image.revision OCI label.  Older images that
    predate the label are handled by falling back to the short (7-character)
    hex SHA that Konflux publishes as an image tag alongside the version tag.

    Operator image:  openshift4-wincw/windows-machine-config-rhel9-operator
    Provides: push_date — the UTC timestamp when the image was published to
    the catalog.  This is used as the tag date so the annotated tag timestamp
    reflects the actual release date rather than the date the tag was created.

  Local git repository
    Provides: existing tag detection (prevents accidental overwrites), full
    SHA expansion from short SHAs via git rev-parse, commit existence
    verification to catch catalog/repo mismatches before the tag is written,
    and upstream remote detection for the post-creation push instruction.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
LOGIC
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

1. COMMIT RESOLUTION
   Source:   Bundle image catalog
   Data:     org.opencontainers.image.revision label (preferred) or short hex
             image tag (fallback for images that predate the OCI label).
   Logic:
     • Scans the bundle catalog for the image whose tags include vX.Y.Z.
     • Prefers the full SHA from the OCI label; falls back to the short SHA
       tag.
     • Any SHA (full or short) is expanded to a full 40-character commit via
       git rev-parse and verified to exist locally.  This catches catalog/repo
       mismatches (e.g. if the local clone is stale) before writing anything.
     • --commit skips the catalog lookup entirely and uses the provided value.
       Required for backport releases that were never shipped as a container
       image and therefore have no bundle catalog entry.

2. TAG DATE RESOLUTION
   Source:   Operator image catalog
   Data:     push_date field from the repository entry for the matching
             version.
   Logic:
     • The push_date is the UTC timestamp when the operator image was
       published.
     • Only the date portion (YYYY-MM-DD) is used; the time is fixed to noon
       UTC (12:00:00+0000) to produce an unambiguous, timezone-neutral
       timestamp.
     • The fixed noon time is set via GIT_COMMITTER_DATE when git tag is
       invoked, so it appears as the tag's creation date in git log and GitHub.
     • --date skips the catalog lookup and uses the provided date instead.

3. TAG CREATION
   Logic:
     • Tag name:    vX.Y.Z
     • Tag message: "Windows Machine Config Operator vX.Y.Z"
     • Tag type:    annotated (git tag -a), never lightweight.
     • The user is shown all resolved details — tag, message, full commit SHA
       with its source, and date with its source — and must confirm before the
       tag is written.  This surfaces any unexpected catalog values before they
       become permanent.

4. PUSH INSTRUCTION
   Logic:
     • After creation, scans git remote -v for a remote whose URL contains
       "openshift/windows-machine-config-operator" and uses its configured
       name (commonly "upstream") in the suggested push command.
     • Falls back to the canonical SSH URL if no matching remote is found, so
       the command is always complete and immediately usable.
"""

# pylint: disable=invalid-name  # hyphenated script name is intentional

import argparse
from datetime import date
import os
import re
import shutil
import subprocess
import sys

import requests

OPERATOR_CATALOG_API = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/"
    "openshift4-wincw/windows-machine-config-rhel9-operator/images"
)
BUNDLE_CATALOG_API = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/"
    "openshift4-wincw/windows-machine-config-operator-bundle/images"
)

_VERSION_TAG_RE = re.compile(r"^v(\d+\.\d+\.\d+)$")
_HEX_RE = re.compile(r"^[0-9a-f]{7,40}$")


# ---------------------------------------------------------------------------
# Catalog helpers (shared pattern with verify-release.py)
# ---------------------------------------------------------------------------

def _fetch_pages(api_url: str) -> list:
    """Fetch all image records from a catalog API endpoint,
    handling pagination."""
    images, page = [], 0
    while True:
        resp = requests.get(
            api_url,
            params={
                "page_size": 100,
                "page": page,
                "sort_by": "creation_date[desc]",
            },
            timeout=30,
        )
        resp.raise_for_status()
        batch = resp.json().get("data", [])
        if not batch:
            break
        images.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return images


def _version_from_tags(repos: list) -> str:
    """Return the x.y.z version string from a repository's tag list, or ''."""
    for repo in repos:
        for tag in repo.get("tags", []):
            m = _VERSION_TAG_RE.match(tag.get("name", ""))
            if m:
                return m.group(1)
    return ""


def _labels(img: dict) -> dict:
    return {lbl["name"]: lbl["value"]
            for lbl in img.get("parsed_data", {}).get("labels", [])}


def fetch_bundle_info(version: str) -> tuple[str, str]:
    """
    Return (commit, source_description) for the given version from the
    bundle catalog.  Prefers the org.opencontainers.image.revision label
    (full SHA); falls back to the short hex SHA image tag.
    Returns ('', '') if the version is not found.
    """
    for img in _fetch_pages(BUNDLE_CATALOG_API):
        repos = img.get("repositories", [])
        if _version_from_tags(repos) != version:
            continue
        all_tags = [
            t.get("name", "") for repo in repos for t in repo.get("tags", [])
        ]
        commit = _labels(img).get("org.opencontainers.image.revision", "")
        if commit:
            return commit, "bundle image OCI label"
        sha_tags = [
            t for t in all_tags
            if _HEX_RE.match(t) and not _VERSION_TAG_RE.match(t)
        ]
        if sha_tags:
            return sha_tags[0], "bundle image tag (short SHA)"
        return "", ""
    return "", ""


def fetch_operator_push_date(version: str) -> str:
    """
    Return the push_date (YYYY-MM-DD) for the given version from the
    operator catalog, or '' if not found.
    """
    for img in _fetch_pages(OPERATOR_CATALOG_API):
        for repo in img.get("repositories", []):
            for tag in repo.get("tags", []):
                m = _VERSION_TAG_RE.match(tag.get("name", ""))
                if m and m.group(1) == version:
                    push_date = repo.get("push_date", "")
                    return push_date[:10] if push_date else ""
    return ""


# ---------------------------------------------------------------------------
# Git helpers
# ---------------------------------------------------------------------------

_GIT_BIN = shutil.which("git")


def git(*args, env=None) -> str:
    """Run a git command and return stdout, raising on failure."""
    if not _GIT_BIN:
        raise RuntimeError("git executable not found in PATH")
    try:
        result = subprocess.run(
            [_GIT_BIN, *args],
            capture_output=True,
            text=True,
            env=env,
            check=False,
        )
    except OSError as exc:
        raise RuntimeError(f"failed to execute git: {exc}") from exc
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip())
    return result.stdout.strip()


def tag_exists(tag: str) -> bool:
    """Return True if the git tag already exists in this repository."""
    try:
        git("rev-parse", "-q", "--verify", f"refs/tags/{tag}")
        return True
    except RuntimeError:
        return False


def resolve_commit(ref: str) -> str:
    """Return the full commit SHA for ref, or raise RuntimeError."""
    try:
        return git("rev-parse", f"{ref}^{{commit}}")
    except RuntimeError as exc:
        raise RuntimeError(
            f"Commit '{ref}' not found in this repository.\n"
            "Ensure your local repo is up to date: git fetch origin"
        ) from exc


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def _find_upstream_remote() -> str:
    """
    Return the name of the remote pointing to
    openshift/windows-machine-config-operator, or '' if none is configured.
    """
    try:
        lines = git("remote", "-v").splitlines()
    except RuntimeError:
        return ""
    for line in lines:
        # Each line: "<name>\t<url> (fetch|push)"
        parts = line.split()
        if (len(parts) >= 2
                and "openshift/windows-machine-config-operator" in parts[1]):
            return parts[0]
    return ""


# pylint: disable=too-many-locals,too-many-branches,too-many-statements
def main():
    """Parse args, resolve tag details, confirm with user, create the tag."""
    parser = argparse.ArgumentParser(
        description="Create an annotated WMCO release tag.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""\
Examples:
  python3 hack/create-release-tag.py 10.21.1
  python3 hack/create-release-tag.py 10.17.2 --commit abc1234 --date 2025-06-03

Note:
  Pushing tags to the upstream openshift/windows-machine-config-operator
  repository requires write access granted by the repository administrators.
  Contact the WMCO team to have your GitHub account added before pushing.
""",
    )
    parser.add_argument("version", metavar="X.Y.Z",
                        help="Release version without 'v' prefix")
    parser.add_argument(
        "--commit", metavar="SHA",
        help="Override commit SHA (required if version is not in catalog)",
    )
    parser.add_argument("--date", metavar="YYYY-MM-DD",
                        help="Override published date")
    args = parser.parse_args()

    # Validate inputs
    if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
        parser.error(f"version must be X.Y.Z, got: {args.version!r}")

    if args.date:
        try:
            date.fromisoformat(args.date)
        except ValueError:
            parser.error(
                f"--date must be a valid YYYY-MM-DD date, got: {args.date!r}"
            )

    version = args.version
    tag = f"v{version}"
    message = f"Windows Machine Config Operator {tag}"

    if tag_exists(tag):
        print(
            f"ERROR: Tag '{tag}' already exists. "
            "Delete it first if you intend to recreate it.",
            file=sys.stderr,
        )
        sys.exit(1)

    need_catalog = not args.commit or not args.date
    if need_catalog:
        print(
            f"Fetching release details for {tag} "
            "from Red Hat Container Catalog...",
            flush=True,
        )

    if args.commit:
        commit_sha = args.commit
        commit_source = "provided manually"
    else:
        try:
            commit_sha, commit_source = fetch_bundle_info(version)
        except Exception as err:  # pylint: disable=broad-except
            print(
                f"\nERROR: fetch_bundle_info failed: {err}",
                file=sys.stderr,
            )
            sys.exit(1)
        if not commit_sha:
            print(
                f"\nERROR: Could not resolve commit SHA for {tag}.",
                file=sys.stderr,
            )
            print(
                "       The bundle image for this version "
                "may not be in the catalog.",
                file=sys.stderr,
            )
            print(
                "       Provide the commit manually: --commit <SHA>",
                file=sys.stderr,
            )
            sys.exit(1)

    if args.date:
        published_date = args.date
        date_source = "provided manually"
    else:
        try:
            published_date = fetch_operator_push_date(version)
        except Exception as err:  # pylint: disable=broad-except
            print(
                f"\nERROR: fetch_operator_push_date failed: {err}",
                file=sys.stderr,
            )
            sys.exit(1)
        date_source = "operator image push date"
        if not published_date:
            print(
                f"\nERROR: Could not resolve published date for {tag}.",
                file=sys.stderr,
            )
            print(
                "       The operator image for this version "
                "may not be in the catalog.",
                file=sys.stderr,
            )
            print(
                "       Provide the date manually: --date YYYY-MM-DD",
                file=sys.stderr,
            )
            sys.exit(1)

    # Expand to full commit SHA and verify it exists locally
    try:
        full_commit = resolve_commit(commit_sha)
    except RuntimeError as exc:
        print(f"\nERROR: {exc}", file=sys.stderr)
        sys.exit(1)

    # Use noon UTC on the published date for an unambiguous timestamp
    tag_date = f"{published_date}T12:00:00+0000"

    # Confirm with user
    print()
    print("Tag details:")
    print()
    print(f"  {'Tag:':<10} {tag}")
    print(f"  {'Message:':<10} {message}")
    print(f"  {'Commit:':<10} {full_commit}  ({commit_source})")
    print(f"  {'Date:':<10} {tag_date}  ({date_source})")
    print()
    try:
        answer = input("Proceed? [y/N] ")
    except (KeyboardInterrupt, EOFError):
        print("\nAborted.")
        sys.exit(0)

    if answer.strip().lower() != "y":
        print("Aborted.")
        sys.exit(0)

    # Create the annotated tag with the catalog publish date
    env = {**os.environ, "GIT_COMMITTER_DATE": tag_date}
    try:
        git("tag", "-a", tag, full_commit, "-m", message, env=env)
    except RuntimeError as exc:
        print(f"\nERROR: Failed to create tag: {exc}", file=sys.stderr)
        sys.exit(1)

    upstream_remote = _find_upstream_remote()
    upstream_url = (
        "git@github.com:openshift/windows-machine-config-operator.git"
    )
    push_target = upstream_remote if upstream_remote else upstream_url

    print()
    print(f"Tag '{tag}' created. To push to the upstream repository:")
    print(f"  git push {push_target} {tag}")


if __name__ == "__main__":
    main()
