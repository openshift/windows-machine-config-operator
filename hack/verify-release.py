#!/usr/bin/env python3
"""
verify-release.py — Verify WMCO release quality against the Red Hat Container
Catalog, Errata, GitHub, GitLab CEE advisory repo, Jira, and the Red Hat
support policy page.

Run this after a release ships to confirm all release artifacts and tracking items
are in a consistent, correct state. Each check is independently pass/fail so
partial failures are easy to identify and act on.

Requirements:
    Python 3.10 or later
    pip install requests pyyaml

Usage:
    python3 hack/verify-release.py                     Check the latest shipped version
    python3 hack/verify-release.py --version 10.21.1   Check a specific version
    python3 hack/verify-release.py --all               Check all shipped versions

Optional environment variables:
    GITHUB_TOKEN   — GitHub personal access token (avoids rate limiting; 5000 req/hr vs 60)
    GITLAB_TOKEN   — GitLab CEE personal access token (read_api scope; for advisory YAML fetch)
    JIRA_URL       — Jira instance base URL (e.g. https://redhat.atlassian.net)
    JIRA_EMAIL     — Jira account email
    JIRA_TOKEN     — Jira API token

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
DATA SOURCES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Red Hat Container Catalog  (catalog.redhat.com)
    Operator image:  openshift4-wincw/windows-machine-config-rhel9-operator
    Bundle image:    openshift4-wincw/windows-machine-config-operator-bundle
    Provides: version strings, advisory IDs, image published dates, build commit
    SHAs (via org.opencontainers.image.revision label), bundle image digests.
    Git tags are pushed by a developer once the images are published — so the
    bundle image's build commit is used when comparing against GitHub tags, not
    the operator image's.

  Red Hat Errata  (access.redhat.com/errata)
    HTML-scraped advisory page for each version's advisory ID.
    Provides: version strings present in the advisory body, errata issued date.

  GitHub API  (api.github.com/repos/openshift/windows-machine-config-operator)
    Provides: git tag SHAs, commit existence verification.

  Red Hat Support Policy Page  (access.redhat.com/support/policy/updates/openshift_operators)
    HTML-scraped "Red Hat OpenShift support for Windows Containers" version table.
    Provides: GA date per x.y minor version.

  GitLab CEE  (gitlab.cee.redhat.com/releng/advisories)
    advisory.yaml files under data/advisories/windows-machine-conf-tenant/{year}/{number}/.
    Provides: advisory metadata fields for schema consistency validation.

  Jira  (JIRA_URL — e.g. redhat.atlassian.net)
    WINC project Epics with fixVersion = "WMCO x.y.z" and their child tasks.
    Provides: epic status, child issue completion, changelog transition history.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CHECKS, DATA POINTS, AND LOGIC
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

1. CATALOG TAG FORMAT
   Source:    Red Hat Container Catalog tag list for the operator image
   Data:      The x.y.z version tag name applied to the image in the catalog
   Logic:
     • The catalog tag must use the 'v' prefix (e.g. 'v10.17.0', not '10.17.0').
       Images tagged without the prefix are still checked but flagged.
   Decision:  FAIL if the matching catalog tag is missing the 'v' prefix.

2. ADVISORY VERSION MATCH
   Source:    Red Hat Errata HTML (access.redhat.com/errata/{advisory_id})
   Data:      Full text of the errata page body
   Logic:
     • The x.y.z version string must appear somewhere in the advisory text.
     • No OTHER known WMCO version (from the catalog) may appear in the text.
       This catches advisories that were incorrectly generated with the wrong
       version or that reference a prior release.
     • The errata issued date is scraped and reported alongside the result.
   Decision:  FAIL if the correct version is absent, or if any other WMCO
              version string is found in the advisory body.

3. GIT TAG EXISTS
   Source:    GitHub API — GET /git/refs/tags/v{version}
              GitHub API — GET /commits/{sha}  (for mismatch verification)
   Data:      Tag SHA from the GitHub repo; build commit SHA from the bundle
              image's org.opencontainers.image.revision label
   Logic:
     • Resolves annotated tags to their underlying commit SHA.
     • Compares the resolved tag SHA against the bundle image's build commit.
       Git tags are pushed by a developer once the images are published, so the
       bundle commit is the authoritative reference — not the operator image commit.
     • On mismatch, verifies both SHAs exist in the repo to rule out the image
       having been built from a fork or different repository.
   Decision:  FAIL if the tag does not exist, if the commit SHA cannot be
              resolved, or if the tag points to a different commit than the
              bundle image was built from.

4. SUPPORT PAGE GA  [x.y.0 releases only; SKIP for patch releases]
   Source:    Red Hat support policy page (HTML-scraped version table)
   Data:      x.y minor version entries in the Windows Containers support table
   Logic:
     • Only checked for x.y.0 (GA) releases; patch releases are skipped.
     • Verifies the x.y minor version is present in the support table.
     • The GA date is reported for informational purposes but not validated
       against the image published date.
   Decision:  FAIL if the x.y minor version is not listed.
              SKIP for x.y.z where z != 0.

5. EPIC STATUS  [SKIP if JIRA_* env vars not set]
   Source:    Jira — POST /rest/api/3/search/jql  (cursor-based pagination)
              Jira — parent = {epic_key} child issue search
   Data:      WINC Epic with fixVersion = "WMCO x.y.z"; all child issues
   Logic:
     • Finds the release Epic via JQL: project = WINC AND issuetype = Epic
       AND fixVersion = "WMCO x.y.z"
     • Epic status must be exactly "Closed" — no other status is acceptable,
       including "Done" or status-category-done states.
     • All child issues must be in a "done" status category.
     • Git tag v{version} must exist in the GitHub repo.
     • For x.y.0 releases: x.y must be listed on the support policy page.
   Decision:  FAIL if no epic is found, epic is not "Closed", any child issue
              is not done, git tag is missing, or (for x.y.0) support page
              is not updated.
              SKIP if JIRA_URL / JIRA_EMAIL / JIRA_TOKEN are not configured.

6. ADVISORY YAML
   Source:    GitLab CEE — releng/advisories repository
              Path: data/advisories/windows-machine-conf-tenant/{year}/{number}/advisory.yaml
   Data:      Parsed YAML spec fields and image entries
   Logic (all failures accumulated and reported together):
     1. synopsis must contain the full x.y.z version string
     2. topic must contain the full x.y.z version string
     3. product_version must equal x.y  (compared as floats — unquoted YAML
        parses "10.20" as float 10.2, so string comparison would give false
        failures; float comparison is used instead)
     4. product_stream must equal "wmco-x.y"
     5. Each image entry's tags list must include a "v{version}-{build}" entry
        (versioned tag format, not a bare build number)
     6. Bundle image containerImage digest must match the catalog bundle digest
        (sha256: prefix normalized with str.removeprefix before comparison)
   Decision:  FAIL if any of the above sub-checks fail; all failures are
              reported in a single message.


GLOBAL CHECK (runs once, not per-version)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  CATALOG COMPLETENESS
  Source:    GitHub API — GET /tags (all vX.Y.Z tags in the repo)
             Red Hat Container Catalog (all versions with catalog entries)
  Logic:
    • Fetches every vX.Y.Z git tag from GitHub and compares against the set of
      versions present in the catalog.  A git tag without a catalog entry means
      the image was either never published or was removed.
  Decision:  FAIL for each git tag that has no matching catalog entry.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
OUTPUT CODES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  [PASS]  Check passed
  [FAIL]  Check failed — action required
  [SKIP]  Check does not apply to this version or is not configured

  Exit code 0 — all applicable checks passed
  Exit code 1 — one or more checks failed
  Exit code 2 — fatal error (connectivity failure, no images found, etc.)
"""

import argparse
import os
import re
import sys
import time
from datetime import datetime
from urllib.parse import quote as urlquote
import requests
from requests.auth import HTTPBasicAuth
import yaml

CATALOG_API = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/"
    "openshift4-wincw/windows-machine-config-rhel9-operator/images"
)
# Oldest version to include in checks.  Versions below this are ignored in both
# the catalog image list and the git-tag completeness check.
MIN_VERSION = (10, 17, 0)

BUNDLE_CATALOG_API = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/"
    "openshift4-wincw/windows-machine-config-operator-bundle/images"
)
ERRATA_BASE = "https://access.redhat.com/errata"
GITHUB_API = "https://api.github.com/repos/openshift/windows-machine-config-operator"
SUPPORT_PAGE = "https://access.redhat.com/support/policy/updates/openshift_operators"
GITLAB_API = (
    "https://gitlab.cee.redhat.com/api/v4/projects/releng%2Fadvisories/repository"
)
PAGE_SIZE = 100


def _print_check(tag: str, label: str, detail: str = "", indent: int = 2) -> None:
    """Print a single status line: '  [TAG] label: detail'."""
    suffix = f": {detail}" if detail else ""
    print(f"{' ' * indent}[{tag}] {label}{suffix}")


def _github_headers() -> dict:
    """Return Authorization headers for GitHub API requests, if GITHUB_TOKEN is set."""
    token = os.environ.get("GITHUB_TOKEN")
    return {"Authorization": f"Bearer {token}"} if token else {}


def _gitlab_headers() -> dict:
    """Return Authorization headers for GitLab API requests, if GITLAB_TOKEN is set."""
    token = os.environ.get("GITLAB_TOKEN")
    return {"PRIVATE-TOKEN": token} if token else {}


def _get(url, *, retries=3, delay=2, **kwargs) -> requests.Response:
    """
    Wrapper around requests.get with retry logic for transient network errors
    (DNS resolution failures, connection resets, timeouts).  Raises
    requests.RequestException with a concise message on final failure.
    A default timeout of 30 seconds is applied when none is provided by the caller.
    """
    kwargs.setdefault("timeout", 30)
    last_exc = None
    for attempt in range(retries):
        try:
            return requests.get(url, **kwargs)  # pylint: disable=missing-timeout
        except (requests.exceptions.ConnectionError, requests.exceptions.Timeout) as exc:
            last_exc = exc
            if attempt < retries - 1:
                time.sleep(delay)
    # Simplify the urllib3 error chain to the root cause message.
    if last_exc is None:
        # retries=0 was passed; no connection was ever attempted.
        raise requests.exceptions.ConnectionError(
            f"Could not connect to {url.split('/')[2]}: no retry attempts configured"
        )
    root = last_exc
    while root.__cause__ is not None:
        root = root.__cause__
    raise requests.exceptions.ConnectionError(
        f"Could not connect to {url.split('/')[2]}: {root}"
    ) from last_exc


def _fetch_images_from(api_url: str) -> list:
    """Fetch all image records from a catalog API endpoint, handling pagination."""
    images = []
    page = 0
    while True:
        params = {
            "page_size": PAGE_SIZE,
            "page": page,
            "sort_by": "creation_date[desc]",
        }
        resp = _get(api_url, params=params, timeout=30)
        resp.raise_for_status()
        data = resp.json()
        batch = data.get("data", [])
        if not batch:
            break
        images.extend(batch)
        if len(images) >= data.get("total", 0):
            break
        page += 1
    return images


def fetch_all_images() -> list:
    """Fetch all operator image records from the catalog API."""
    return _fetch_images_from(CATALOG_API)


def fetch_bundle_commits() -> dict:
    """
    Fetch all bundle image records and return a mapping of version string
    (e.g. "10.21.1") to a dict with:
      - "commit":  build commit SHA from org.opencontainers.image.revision label
      - "digest":  sha256 image digest (e.g. "sha256:abc...")
    """
    raw = _fetch_images_from(BUNDLE_CATALOG_API)
    result = {}
    for img in raw:
        repos = img.get("repositories", [])
        version, _ = _version_from_tags(repos)
        if not version or version in result:
            continue
        labels = {
            lbl["name"]: lbl["value"]
            for lbl in img.get("parsed_data", {}).get("labels", [])
        }
        commit = labels.get("org.opencontainers.image.revision", "")
        digest = img.get("image_id", "")  # "sha256:..."
        result[version] = {"commit": commit, "digest": digest}
    return result


def _version_key(v):
    try:
        return tuple(int(x) for x in v.split("."))
    except ValueError:
        return 0, 0, 0


_VERSION_TAG_RE = re.compile(r"^v(\d+\.\d+\.\d+)$")
_VERSION_TAG_RE_BARE = re.compile(r"^(\d+\.\d+\.\d+)$")
_SHA_RE = re.compile(r"^[0-9a-f]{40}$", re.IGNORECASE)


def _version_from_tags(repos: list) -> tuple[str, bool]:
    """
    Find the x.y.z version string from a repository's tag list.
    Tags are named like 'v10.20.1', 'v10.20.1-1772659355', '06eb5cc', etc.
    Returns (version, has_v_prefix): version without 'v', and whether the
    matching tag used the correct 'v' prefix.  Returns ("", False) if not found.
    """
    for repo in repos:
        for tag in repo.get("tags", []):
            name = tag.get("name", "")
            m = _VERSION_TAG_RE.match(name)
            if m:
                return m.group(1), True
            m = _VERSION_TAG_RE_BARE.match(name)
            if m:
                return m.group(1), False
    return "", False


def extract_image_info(raw_images: list, bundle_commits: dict | None = None) -> list:
    """
    Extract version and advisory info from raw catalog API response.
    Deduplicates by version (multiple architectures may share a version).
    Returns list of dicts sorted newest-first:
        {"version": "10.18.2", "advisory_id": "RHBA-2025:1234"}
    """
    seen = {}
    for img in raw_images:
        repos = img.get("repositories", [])
        version, catalog_tag_ok = _version_from_tags(repos)
        if not version:
            continue
        if _version_key(version) < MIN_VERSION:
            continue
        if version in seen:
            continue
        advisory_id = None
        published_date = None
        for repo in repos:
            aid = repo.get("image_advisory_id")
            if aid:
                advisory_id = aid.strip()
            pd = repo.get("push_date")
            if pd:
                # Keep only the date portion (YYYY-MM-DD)
                published_date = pd[:10]
            if advisory_id and published_date:
                break
        labels = {
            lbl["name"]: lbl["value"]
            for lbl in img.get("parsed_data", {}).get("labels", [])
        }
        build_commit = labels.get("org.opencontainers.image.revision", "")
        bundle_info = (bundle_commits or {}).get(version, {})
        seen[version] = {
            "version": version,
            "advisory_id": advisory_id,
            "published_date": published_date,
            "build_commit": build_commit,
            "bundle_commit": bundle_info.get("commit", ""),
            "bundle_digest": bundle_info.get("digest", ""),
            "catalog_tag_ok": catalog_tag_ok,
        }

    return sorted(seen.values(), key=lambda x: _version_key(x["version"]), reverse=True)


# ---------------------------------------------------------------------------
# Check functions
#
# Each check has the signature:
#   check_fn(image: dict, all_versions: list[str]) -> tuple[bool | str | None, str]
#
# Return values for the first element:
#   True    — check passed  → [PASS]
#   False   — check failed  → [FAIL]
#   None    — not applicable to this version  → [SKIP]
#
# `image`        — {"version": "x.y.z", "advisory_id": "RHBA-..."}
# `all_versions` — every WMCO version known from the catalog (for cross-checks)
# ---------------------------------------------------------------------------

_ISSUED_RE = re.compile(r'<dt>Issued:</dt>\s*<dd[^>]*>(.*?)</dd>', re.DOTALL)


def _errata_issued_date(html: str) -> str:
    """Extract the Issued date from errata page HTML, or '' if not found."""
    m = _ISSUED_RE.search(html)
    return m.group(1).strip() if m else ""


def check_catalog_tag_format(image, _all_versions):
    """
    Verify that the catalog version tag uses the required 'v' prefix.
    A tag of '10.17.0' is wrong; it should be 'v10.17.0'.
    """
    version = image["version"]
    if image.get("catalog_tag_ok", True):
        return True, f"catalog tag 'v{version}' has correct 'v' prefix"
    return False, f"catalog tag is '{version}' — must be 'v{version}' (missing 'v' prefix)"


def check_advisory_version_match(image, all_versions):
    """
    Verify that the advisory for this image version:
      1. Mentions the correct WMCO version string.
      2. Does NOT mention any other known WMCO version string.
    Reports the errata issued date on success.
    """
    version = image["version"]
    advisory_id = image["advisory_id"]

    if not advisory_id:
        return False, f"No advisory found for version {version}"

    url = f"{ERRATA_BASE}/{advisory_id}"
    try:
        resp = _get(url, timeout=15, allow_redirects=True)
    except requests.RequestException as exc:
        return False, f"Failed to fetch advisory {advisory_id}: {exc}"

    if resp.status_code != 200:
        return False, f"Could not fetch advisory {advisory_id}: HTTP {resp.status_code}"

    text = resp.text
    issued = _errata_issued_date(text)
    issued_str = f", errata issued {issued}" if issued else ""

    # Check 1: correct version must be present
    if version not in text:
        return False, f"{advisory_id} does NOT mention version {version}"

    # Check 2: no OTHER known WMCO version should appear in the advisory
    wrong = sorted(v for v in all_versions if v != version and v in text)
    if wrong:
        return False, (
            f"{advisory_id} incorrectly references other WMCO versions: {', '.join(wrong)}"
        )

    return True, f"{advisory_id} mentions {version} and no other WMCO versions{issued_str}"


def check_git_tag_exists(image, _all_versions):
    """
    Verify that a git tag matching the image version exists in the GitHub repo.
    Reports the commit hash of the tag on success.
    """
    version = image["version"]
    tag = f"v{version}"
    url = f"{GITHUB_API}/git/refs/tags/{tag}"
    headers = _github_headers()

    try:
        resp = _get(url, headers=headers, timeout=15)
    except requests.RequestException as exc:
        return False, f"Failed to query GitHub for tag {tag}: {exc}"

    if resp.status_code == 404:
        return False, f"Tag {tag} not found in openshift/windows-machine-config-operator"
    if resp.status_code != 200:
        return False, f"GitHub API error for tag {tag}: HTTP {resp.status_code}"

    ref = resp.json()
    obj = ref.get("object", {})
    sha = obj.get("sha", "")
    obj_type = obj.get("type", "")

    # Annotated tags point to a tag object; resolve to the underlying commit.
    if obj_type == "tag":
        try:
            tag_resp = _get(obj.get("url", ""), headers=headers, timeout=15)
            tag_resp.raise_for_status()
            sha = tag_resp.json().get("object", {}).get("sha", "")
        except requests.RequestException as exc:
            return False, f"Could not resolve annotated tag {tag} to a commit SHA: {exc}"

    if not sha:
        return False, f"Could not resolve commit hash for tag {tag}"

    # Git tags are pushed by a developer after images are published. The bundle
    # image's build commit is the authoritative reference, not the operator image's.
    build_commit = image.get("bundle_commit") or image.get("build_commit") or ""
    if not build_commit:
        return True, f"Tag {tag} found at commit {sha[:12]} (build commit unknown)"

    if not _SHA_RE.match(sha) or not _SHA_RE.match(build_commit):
        return False, (
            f"Tag {tag}: unexpected SHA format — tag_sha={sha!r}, build_commit={build_commit!r}"
        )

    if sha != build_commit:
        # Verify the build commit actually exists in this repo before reporting
        # a mismatch — rules out the image having been built from a different repo.
        commit_url = f"{GITHUB_API}/commits/{build_commit}"
        try:
            commit_resp = _get(commit_url, headers=headers, timeout=15)
        except requests.RequestException as exc:
            return False, (
                f"Tag {tag} points to {sha[:12]} but image was built from "
                f"{build_commit[:12]} (could not verify build commit in repo: {exc})"
            )
        if commit_resp.status_code == 404:
            return False, (
                f"Tag {tag} points to {sha[:12]} but image build commit "
                f"{build_commit[:12]} does not exist in openshift/windows-machine-config-operator "
                f"(may have been built from a different repository)"
            )
        if commit_resp.status_code != 200:
            return False, (
                f"Tag {tag} points to {sha[:12]} but image was built from "
                f"{build_commit[:12]} (GitHub API error verifying build commit: "
                f"HTTP {commit_resp.status_code})"
            )
        return False, (
            f"Tag {tag} points to {sha[:12]} but image was built from "
            f"{build_commit[:12]} (both commits confirmed in repo)"
        )

    return True, f"Tag {tag} commit {sha[:12]} matches image build commit"


_SUPPORT_TABLE_ROW_RE = re.compile(
    r'data-label="Version"[^>]*>\s*([\d.]+)\s*</td>'
    r'.*?data-label="General availability"[^>]*title="([^"]+)"',
    re.DOTALL,
)

_support_ga_cache = None  # {minor_version: "YYYY-MM-DD"}, e.g. {"10.20": "2025-10-22"}


def _fetch_support_ga_dates() -> dict:
    """
    Fetch the Windows Containers support policy page and return a mapping of
    minor version string (e.g. "10.20") to GA date (YYYY-MM-DD).
    Result is cached after the first fetch.
    """
    global _support_ga_cache  # pylint: disable=global-statement
    if _support_ga_cache is not None:
        return _support_ga_cache

    try:
        resp = _get(SUPPORT_PAGE, timeout=15)
        resp.raise_for_status()
    except requests.RequestException as exc:
        raise RuntimeError(f"Failed to fetch support policy page: {exc}") from exc

    ga_map = {}
    for ver, raw_date in _SUPPORT_TABLE_ROW_RE.findall(resp.text):
        ver = ver.strip()
        try:
            # Parse "22 Oct 2025" → "2025-10-22"
            ga_map[ver] = datetime.strptime(raw_date.strip(), "%d %b %Y").strftime("%Y-%m-%d")
        except ValueError:
            ga_map[ver] = raw_date.strip()

    _support_ga_cache = ga_map
    return ga_map


def check_support_page_ga(image, _all_versions):
    """
    Only applies to x.y.0 releases. Verifies that the x.y minor version is
    listed on the Windows Containers support policy page.
    Returns None (skip) for patch releases.
    """
    version = image["version"]
    major, minor, patch = version.split(".", 2)
    if patch != "0":
        return None, "only checked for x.y.0 releases"

    minor_ver = f"{major}.{minor}"

    try:
        ga_map = _fetch_support_ga_dates()
    except RuntimeError as exc:
        return False, str(exc)

    if minor_ver not in ga_map:
        return False, f"{minor_ver} not listed on the Windows Containers support policy page"

    return True, f"{minor_ver} listed on support page with GA date {ga_map[minor_ver]}"


# ---------------------------------------------------------------------------
# Advisory YAML helpers
# ---------------------------------------------------------------------------

_advisory_yaml_cache = {}  # advisory_id → parsed dict


def _fetch_advisory_yaml(advisory_id: str) -> dict:
    """
    Fetch and parse the advisory.yaml from the GitLab releng/advisories repo.
    Result is cached by advisory_id.
    Raises RuntimeError on fetch or parse failure.
    """
    if advisory_id in _advisory_yaml_cache:
        return _advisory_yaml_cache[advisory_id]

    # advisory_id format: "RHBA-2026:4787" → year=2026, number=4787
    try:
        year, number = advisory_id.split("-")[1].split(":")
    except (IndexError, ValueError) as exc:
        raise RuntimeError(f"Cannot parse advisory ID '{advisory_id}': {exc}") from exc

    path = f"data/advisories/windows-machine-conf-tenant/{year}/{number}/advisory.yaml"
    url = f"{GITLAB_API}/files/{urlquote(path, safe='')}/raw"
    try:
        resp = _get(url, headers=_gitlab_headers(), params={"ref": "main"}, timeout=15)
        resp.raise_for_status()
    except requests.RequestException as exc:
        raise RuntimeError(f"Failed to fetch advisory YAML for {advisory_id}: {exc}") from exc

    try:
        data = yaml.safe_load(resp.text)
    except yaml.YAMLError as exc:
        raise RuntimeError(f"Failed to parse advisory YAML for {advisory_id}: {exc}") from exc

    if not isinstance(data, dict):
        raise RuntimeError(
            f"Failed to parse advisory YAML for {advisory_id}: "
            f"top-level document must be a mapping, got {type(data).__name__}"
        )

    _advisory_yaml_cache[advisory_id] = data
    return data


def check_advisory_yaml(image, _all_versions):
    """
    Fetch the advisory YAML from the releng/advisories GitLab repo and verify:
      1. synopsis and topic both contain the full x.y.z version string
      2. product_version = "{major}.{minor}"
      3. product_stream = "wmco-{major}.{minor}"
      4. Each image's tags list includes a versioned "v{version}-{build}" entry
      5. Bundle image containerImage digest matches the catalog bundle digest
    All failures are accumulated and reported together.
    """
    advisory_id = image.get("advisory_id")
    if not advisory_id:
        return False, "no advisory ID found in catalog"

    version = image["version"]
    major, minor, _ = version.split(".", 2)
    minor_ver = f"{major}.{minor}"

    try:
        adv = _fetch_advisory_yaml(advisory_id)
    except RuntimeError as exc:
        return False, str(exc)

    spec = adv.get("spec", {})
    if not isinstance(spec, dict):
        return False, f"advisory YAML ({advisory_id}): spec must be a mapping"
    failures = []

    # Check 1: synopsis and topic both contain the full x.y.z version
    synopsis = spec.get("synopsis", "")
    if version not in synopsis:
        failures.append(f"synopsis does not contain version {version!r}: {synopsis!r}")

    topic = spec.get("topic", "")
    if version not in topic:
        failures.append(f"topic does not contain version {version!r}: {topic!r}")

    # Check 2: product_version = "x.y"
    # Compare as floats: unquoted YAML parses "10.20" as float 10.2, so string
    # comparison would incorrectly flag it as "10.2" != "10.20".
    actual_pver = spec.get("product_version", "")
    try:
        pver_match = float(actual_pver) == float(minor_ver)
    except (TypeError, ValueError):
        pver_match = False
    if not pver_match:
        failures.append(f"product_version is '{actual_pver}', expected '{minor_ver}'")

    # Check 3: product_stream = "wmco-x.y"
    expected_stream = f"wmco-{minor_ver}"
    actual_stream = spec.get("product_stream", "")
    if actual_stream != expected_stream:
        failures.append(f"product_stream is '{actual_stream}', expected '{expected_stream}'")

    # Checks 4 & 5: iterate images once — check versioned tags for all, bundle digest for bundle.
    versioned_tag_prefix = f"v{version}-"
    bundle_digest = image.get("bundle_digest", "")
    bundle_digest_checked = False
    content = spec.get("content", {})
    if not isinstance(content, dict):
        return False, f"advisory YAML ({advisory_id}): spec.content must be a mapping"
    images = content.get("images", [])
    if not isinstance(images, list):
        return False, f"advisory YAML ({advisory_id}): spec.content.images must be a list"
    for img_entry in images:
        component = img_entry.get("component", "unknown")
        tags = img_entry.get("tags", [])

        # Check 4: each image's tags list must include a "v{version}-{build}" entry.
        if not any(t.startswith(versioned_tag_prefix) for t in tags):
            failures.append(
                f"{component} tags {tags} missing a versioned tag starting with '{versioned_tag_prefix}'"
            )

        # Check 5: bundle image containerImage digest must match the catalog.
        if bundle_digest and not bundle_digest_checked and "operator-bundle" in component:
            adv_digest = img_entry.get("containerImage", "").split("@")[-1]
            # Normalize: strip "sha256:" prefix for comparison (use removeprefix, not lstrip,
            # to avoid stripping hex chars that overlap with the prefix character set).
            cat_digest = bundle_digest.removeprefix("sha256:")
            adv_digest_bare = adv_digest.removeprefix("sha256:")
            if adv_digest_bare and cat_digest and adv_digest_bare != cat_digest:
                failures.append(
                    f"bundle digest in advisory ({adv_digest_bare[:16]}...) "
                    f"does not match catalog ({cat_digest[:16]}...)"
                )
            bundle_digest_checked = True

    # Check 5 (post-loop): if a bundle digest was expected but no operator-bundle
    # image entry was found, the digest was never verified — report that as a failure.
    if bundle_digest and not bundle_digest_checked:
        failures.append(
            "bundle image entry missing from advisory YAML; could not verify bundle digest"
        )

    if failures:
        return False, f"advisory YAML ({advisory_id}): " + "; ".join(failures)

    return True, f"advisory YAML valid ({advisory_id})"


# ---------------------------------------------------------------------------
# Jira helpers
# ---------------------------------------------------------------------------

def _jira_auth():
    """Return (base_url, HTTPBasicAuth) or (None, None) if not configured."""
    url = os.environ.get("JIRA_URL", "").rstrip("/")
    email = os.environ.get("JIRA_EMAIL", "")
    token = os.environ.get("JIRA_TOKEN", "")
    if not (url and email and token):
        return None, None
    return url, HTTPBasicAuth(email, token)


def _jira_post_with_retry(url, *, retries=3, delay=2, **kwargs):
    """
    Wrapper around requests.post with retry logic for transient network errors
    (connection resets, timeouts).  Raises requests.RequestException on final failure.
    The /search/jql endpoint is idempotent, so retries are safe.
    """
    last_exc = None
    for attempt in range(retries):
        try:
            return requests.post(url, **kwargs)  # pylint: disable=missing-timeout
        except (requests.exceptions.ConnectionError, requests.exceptions.Timeout) as exc:
            last_exc = exc
            if attempt < retries - 1:
                time.sleep(delay)
    raise last_exc


def _jira_search(base_url, auth, jql, fields) -> list:
    """Run a paginated Jira JQL search using the /search/jql API (cursor-based pagination)."""
    issues = []
    body = {"jql": jql, "fields": fields, "maxResults": 100}
    while True:
        resp = _jira_post_with_retry(
            f"{base_url}/rest/api/3/search/jql",
            auth=auth,
            timeout=15,
            json=body,
        )
        resp.raise_for_status()
        data = resp.json()
        batch = data.get("issues", [])
        issues.extend(batch)
        next_token = data.get("nextPageToken")
        if data.get("isLast", True) or not batch or not next_token:
            break
        body["nextPageToken"] = next_token
    return issues


def _is_done(issue) -> bool:
    """Return True if the issue's status category is 'done'."""
    cat = issue["fields"]["status"].get("statusCategory", {})
    return cat.get("key") == "done"


_epic_cache = {}  # version -> epic issue dict (or None if not found)


def _get_epic(version, base_url, auth):
    """Return the Jira epic for this WMCO version, or None if not found. Cached."""
    if version in _epic_cache:
        return _epic_cache[version]
    fix_ver = f"WMCO {version}"
    epics = _jira_search(
        base_url, auth,
        jql=f'project = WINC AND issuetype = Epic AND fixVersion = "{fix_ver}"',
        fields=["summary", "status"],
    )
    result = epics[0] if epics else None
    _epic_cache[version] = result
    return result


def _git_tag_sha(version) -> str:
    """
    Return the commit SHA for vX.Y.Z in the GitHub repo, or '' if not found
    or if any network error occurs.  Annotated tags are resolved to the
    underlying commit SHA; if that resolution fails, '' is returned rather
    than silently returning the tag-object SHA.
    """
    tag = f"v{version}"
    headers = _github_headers()
    try:
        resp = _get(f"{GITHUB_API}/git/refs/tags/{tag}", headers=headers, timeout=15)
        if resp.status_code != 200:
            return ""
        obj = resp.json().get("object", {})
        if obj.get("type") == "tag":
            r2 = _get(obj.get("url", ""), headers=headers, timeout=15)
            if not r2.ok:
                return ""
            return r2.json().get("object", {}).get("sha", "")
        return obj.get("sha", "")
    except requests.RequestException:
        return ""


def check_epic_status(image, _all_versions):
    """
    Verify the Jira release epic for this version:
      - Epic must exist in the WINC project (matched by fixVersion "WMCO x.y.z").
      - If the epic is closed, all child issues must also be closed.
      - If the epic is closed, the git tag must have been pushed.
      - If the epic is closed and this is an x.y.0 release, the support page must be updated.
    Skipped if JIRA_URL / JIRA_EMAIL / JIRA_TOKEN are not set.
    """
    base_url, auth = _jira_auth()
    if not base_url:
        return None, "JIRA_URL / JIRA_EMAIL / JIRA_TOKEN not configured"

    version = image["version"]

    try:
        epic = _get_epic(version, base_url, auth)
    except requests.RequestException as exc:
        return False, f"Jira search failed: {exc}"

    if not epic:
        return False, f"No epic found in WINC project with fixVersion 'WMCO {version}'"

    epic_key = epic["key"]
    epic_status = epic["fields"]["status"]["name"]
    epic_url = f"{base_url}/browse/{epic_key}"

    if epic_status != "Closed":
        return False, f"{epic_key} status is '{epic_status}' — must be 'Closed' ({epic_url})"

    # Epic is Closed — verify all conditions were met before closing.
    failures = []

    # Condition 1: all child issues must be closed.
    try:
        children = _jira_search(
            base_url, auth,
            jql=f"parent = {epic_key}",
            fields=["summary", "status"],
        )
    except requests.RequestException as exc:
        return False, f"Failed to fetch child issues for {epic_key}: {exc}"

    open_children = [c for c in children if not _is_done(c)]
    if open_children:
        failures.append(f"child issue(s) not closed: {', '.join(c['key'] for c in open_children)}")

    # Condition 2: git tag must be pushed.
    if not _git_tag_sha(version):
        failures.append(f"git tag v{version} not pushed")

    # Condition 3: for x.y.0, support page must list the minor version.
    major, minor, patch = version.split(".", 2)
    if patch == "0":
        try:
            ga_map = _fetch_support_ga_dates()
        except RuntimeError:
            ga_map = {}
        if f"{major}.{minor}" not in ga_map:
            failures.append(f"support page not updated for {major}.{minor}")

    if failures:
        return False, f"{epic_key} closed prematurely — {'; '.join(failures)} ({epic_url})"

    child_summary = (
        f"all {len(children)} child issue(s) closed" if children else "no child issues"
    )
    return True, f"{epic_key} correctly closed ({child_summary}) ({epic_url})"


# Registry of checks: list of (check_fn, short_name) tuples.
# Add new checks here to extend the tool.
# A check may return (None, reason) to indicate it does not apply to a version;
# the runner will display [SKIP] and exclude it from pass/fail accounting.
CHECKS = [
    (check_catalog_tag_format, "catalog_tag_format"),
    (check_advisory_version_match, "advisory_version_match"),
    (check_git_tag_exists, "git_tag_exists"),
    (check_support_page_ga, "support_page_ga"),
    (check_epic_status, "epic_status"),
    (check_advisory_yaml, "advisory_yaml"),
]


# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------

def run_checks(images_to_check: list, all_versions: list, show_failure_summary: bool = False) -> bool:
    """
    Run all registered checks against images_to_check.
    all_versions is the full set of known WMCO versions (used for cross-checks).
    When show_failure_summary is True, prints a grouped failure summary at the end.
    Returns True if every check passes, False otherwise.
    """
    results = []  # list of (version, check_name, passed, message)

    print("WMCO Release Verification")
    print("=" * 50)

    for img in images_to_check:
        version = img["version"]
        advisory_id = img["advisory_id"] or "no advisory"
        image_published = img.get("published_date") or "unknown"
        print(f"\nVersion {version}  [image published: {image_published}]  Advisory: {advisory_id}")

        for check_fn, check_name in CHECKS:
            passed, message = check_fn(img, all_versions)
            if passed is None:
                _print_check("SKIP", check_name, message)
            else:
                _print_check("PASS" if passed else "FAIL", check_name, message)
                results.append((version, check_name, passed, message))

    total_versions = len(images_to_check)
    failures_by_version = {}
    for version, check_name, passed, message in results:
        if not passed:
            failures_by_version.setdefault(version, []).append((check_name, message))

    print("\n" + "=" * 50)
    passed_count = total_versions - len(failures_by_version)
    print(f"Summary: {passed_count}/{total_versions} versions passed all checks")

    if show_failure_summary and failures_by_version:
        print("\nFailure Summary")
        print("-" * 50)
        for img in images_to_check:
            version = img["version"]
            if version not in failures_by_version:
                continue
            print(f"\n  Version {version}:")
            for check_name, message in failures_by_version[version]:
                _print_check("FAIL", check_name, message, indent=4)

    return len(failures_by_version) == 0


def fetch_git_release_tags() -> list[str]:
    """
    Fetch all vX.Y.Z release tags from the GitHub repo.
    Returns a list of version strings without the leading 'v'
    (e.g. ["10.21.1", "10.21.0", ...]).
    """
    versions = []
    page = 1
    while True:
        resp = _get(
            f"{GITHUB_API}/tags",
            params={"per_page": 100, "page": page},
            headers=_github_headers(),
            timeout=15,
        )
        if resp.status_code != 200:
            break
        batch = resp.json()
        if not batch:
            break
        for tag in batch:
            m = _VERSION_TAG_RE.match(tag.get("name", ""))
            if m:
                versions.append(m.group(1))
        page += 1
    return versions


def check_catalog_completeness(catalog_versions: list[str], git_tag_versions: list[str]) -> bool:
    """
    Verify that every vX.Y.Z git release tag has a corresponding catalog entry.
    Prints [PASS] or [FAIL] lines.  Returns True if nothing is missing.
    """
    catalog_set = set(catalog_versions)
    missing = sorted(
        [v for v in git_tag_versions if v not in catalog_set],
        key=_version_key,
    )
    if not missing:
        _print_check("PASS", "catalog_completeness", "all git release tags present in catalog")
        return True
    for v in missing:
        _print_check("FAIL", "catalog_completeness", f"v{v} has a git tag but is absent from the catalog")
    return False


def check_connectivity() -> bool:
    """
    Probe each external service used by this script.
    Prints [OK] or [FAIL] for each endpoint.
    Returns False if any required service is unreachable.
    """
    failures = []

    def probe(label, url, required=True, **kwargs):
        try:
            resp = _get(url, timeout=8, allow_redirects=True, **kwargs)
            reachable = resp.status_code < 500
        except requests.RequestException as exc:
            _print_check("FAIL", label, str(exc))
            if required:
                failures.append(label)
            return
        if reachable:
            _print_check("OK", label)
        else:
            _print_check("FAIL", label, f"HTTP {resp.status_code}")
            if required:
                failures.append(label)

    print("Connectivity check")
    print("-" * 30)

    probe("Red Hat Container Catalog", "https://catalog.redhat.com/api/containers/v1/")
    probe("Red Hat Access (errata / support)", "https://access.redhat.com/")
    probe("GitHub API", GITHUB_API, headers=_github_headers())
    # Strip "/repository" suffix to probe the project API endpoint directly.
    probe("GitLab CEE", GITLAB_API.rsplit("/repository", 1)[0], headers=_gitlab_headers())

    jira_url = os.environ.get("JIRA_URL", "").rstrip("/")
    if jira_url:
        _, auth = _jira_auth()
        probe("Jira", f"{jira_url}/rest/api/3/serverInfo", auth=auth)
    else:
        _print_check("SKIP", "Jira", "JIRA_URL not configured")

    print()
    if failures:
        print(f"ERROR: Cannot reach required service(s): {', '.join(failures)}", file=sys.stderr)
        return False
    return True


def main():
    parser = argparse.ArgumentParser(
        description="Verify WMCO release details against the Red Hat Container Catalog.",
    )
    parser.add_argument(
        "--version", "-v",
        metavar="X.Y.Z",
        help="Check only this specific version (e.g. 10.18.2)",
    )
    parser.add_argument(
        "--all", "-a",
        action="store_true",
        help="Check all shipped versions (default: latest only)",
    )
    args = parser.parse_args()

    if not check_connectivity():
        sys.exit(2)

    print("Fetching WMCO image list from Red Hat Container Catalog...")
    try:
        raw_images = fetch_all_images()
        bundle_commits = fetch_bundle_commits()
        git_tag_versions = fetch_git_release_tags()
    except requests.RequestException as exc:
        print(f"ERROR: Failed to fetch image list: {exc}", file=sys.stderr)
        sys.exit(2)

    all_images = extract_image_info(raw_images, bundle_commits)
    if not all_images:
        print("ERROR: No WMCO images found in catalog.", file=sys.stderr)
        sys.exit(2)

    all_versions = [img["version"] for img in all_images]

    git_tag_versions = [v for v in git_tag_versions if _version_key(v) >= MIN_VERSION]

    print("\nCatalog Completeness")
    print("-" * 30)
    catalog_complete = check_catalog_completeness(all_versions, git_tag_versions)
    print()

    if args.version:
        images_to_check = [img for img in all_images if img["version"] == args.version]
        if not images_to_check:
            sample = ", ".join(all_versions[:10])
            print(
                f"ERROR: Version {args.version!r} not found in catalog.\n"
                f"Known versions (newest 10): {sample}",
                file=sys.stderr,
            )
            sys.exit(2)
    elif args.all:
        images_to_check = all_images
    else:
        # Default: check only the latest version
        images_to_check = [all_images[0]]

    passed = run_checks(images_to_check, all_versions, show_failure_summary=args.all)
    sys.exit(0 if (passed and catalog_complete) else 1)


if __name__ == "__main__":
    main()
