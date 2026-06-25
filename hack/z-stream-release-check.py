#!/usr/bin/env python3
"""
z-stream-release-check.py — Determine which WMCO release branches need a z-stream release.

Run this before sprint planning to identify which release branches require action.
Exit code 0 means the script completed successfully (review the output to decide).
Exit code 2 means a fatal error occurred (connectivity failure, API error, etc.).

Usage:
    python3 hack/z-stream-release-check.py
        Check all in-support release branches. Prints image health, unreleased
        PRs, and a sprint recommendation for each active branch.
    python3 hack/z-stream-release-check.py --all              # include past-EOM branches
    python3 hack/z-stream-release-check.py --branch release-4.18  # single branch
    python3 hack/z-stream-release-check.py --json             # machine-readable output
    python3 hack/z-stream-release-check.py --pre-release-prs  # include pre-release PR list
    python3 hack/z-stream-release-check.py --connectivity     # test connectivity only

Optional environment variables:
    GITHUB_TOKEN    — GitHub personal access token (avoids API rate limiting)
    JIRA_API_TOKEN  — Jira API token for release ticket tracking
    JIRA_USERNAME   — Jira username / Atlassian account email (required with JIRA_API_TOKEN)

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
PREREQUISITES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Runtime
    Python 3.9 or later
    No additional binaries required — all repository data is fetched via the
    GitHub API; git is not required in PATH.

  Python packages
    requests  (pip install requests)

  Network access
    catalog.redhat.com    — Red Hat Container Catalog (no auth required)
    access.redhat.com     — OCP lifecycle API (no auth required)
    api.github.com        — GitHub REST API (no auth required; unauthenticated
                            requests are rate-limited to 60/hour per IP —
                            set GITHUB_TOKEN to avoid throttling)
    redhat.atlassian.net  — Jira (optional; requires JIRA_API_TOKEN)

  Permissions
    All required APIs are publicly accessible and do not require authentication
    for read operations.  GITHUB_TOKEN is strongly recommended when running in
    CI or repeatedly.  JIRA_API_TOKEN enables release ticket tracking but is
    entirely optional — all release decision logic works without Jira access.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
DATA SOURCES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

  Red Hat Container Catalog
    WMCO operator images:  catalog.redhat.com →
                           openshift4-wincw/windows-machine-config-rhel9-operator
    Base image:            catalog.redhat.com → ubi9/ubi-minimal

  Red Hat OCP Lifecycle API
    Maintenance end dates:  access.redhat.com/product-life-cycles/api/v1/products/
                            ?name=OpenShift+Container+Platform
    Returns JSON with a phases[] array per version; each phase has a name,
    end_date, and date_format field. WMCO is a platform-aligned operator and
    follows OCP support dates. EUS Term 2 support begins with WMCO 10.18 / OCP 4.18.

  GitHub API  (github.com/openshift/windows-machine-config-operator)
    Release branches, tags, PR metadata, branch-to-tag comparison

  Jira  (redhat.atlassian.net)
    Open release Epics and Tasks in the WINC project

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CHECKS, DATA POINTS, AND THRESHOLDS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

1. SUPPORT WINDOW
   Source:    Red Hat OCP Lifecycle JSON API
   Data:      EOM date per OCP minor version, from the phases[] array in the API response.
              WMCO is a platform-aligned operator and follows OCP's
              lifecycle dates exactly. Starting with OCP 4.18 / WMCO 10.18, WMCO ships
              EUS Term 2 support — so for OCP 4.18+ releases where the OCP lifecycle API
              provides an EUS Term 2 date, that date is used as the effective EOM.
              In practice the API only provides EUS Term 2 dates for even-numbered (EUS)
              releases; odd-numbered releases have no such phase and fall back to the
              Maintenance support end date.
   Logic:     Primary: use EUS Term 2 end date if the API provides one and the version
              is EUS-eligible: OCP 4.x with minor ≥ 18, or any OCP 5.x+ version.
              Otherwise: use the Maintenance support end date from the API.
              Fallback: if the API has no entry for a version, check whether any newer
              version of the same OCP major IS returned. If yes, the version has rolled
              off the listing → classified as past EOM. If no newer version is found,
              the version is assumed to be too recent for the API → Active.
   Decision:  Branches where today > EOM are past maintenance and excluded from
              recommendations (use --all to include them in output).
   ⚙ Threshold:  EUS Term 2 end date (OCP 4.18+ or any OCP 5.x+, when API provides one)
                 Maintenance support end date (all other releases)

2. BRANCH CLASSIFICATION
   Source:    GitHub branches + GitHub tags + Red Hat Container Catalog
   Data:      Existence of vMAJOR.MINOR.* tags; presence in RHEL9 catalog
   Logic:     Each release-X.Y branch falls into one of three categories:

     a. IN CATALOG (active or past EOM)
        Branch has matching WMCO tags AND images in the RHEL9 catalog.
        Support status is determined by the EOM date from the OCP lifecycle API.
        Active branches are always shown; past-EOM catalog branches are shown only
        with --all (the past-EOM catalog case is unusual — most versions stay in
        the catalog through their support window).

     b. OLD EOM (not in RHEL9 catalog, has tags)
        Branch has matching WMCO tags but images are absent from the RHEL9
        catalog (shipped via an older RHEL8 catalog). Examples: release-4.17
        and earlier WMCO 10.x releases. EOM status is confirmed by the OCP
        lifecycle API. These branches are hidden by default; use --all to include
        them. Label: "EOM YYYY-MM-DD" or for pre-RHEL9 branches: "EOM (pre-RHEL9)".
        OCP 4.X branches below 4.15 predate WMCO 10.x entirely (WMCO v1.x–v9.x
        tags); they are always classified as old EOM without a tag check.

     c. PRE-RELEASE (in GitHub, no tags yet)
        Branch exists in GitHub but no matching WMCO release tags exist —
        the newest branch, which still tracks master until its first GA release.
        OCP major → WMCO major mapping: ocp_major + 6  (OCP 4 → WMCO 10,
        OCP 5 → WMCO 11, etc.), so tags searched are v{ocp+6}.{minor}.*.
        These branches are shown in RELEASE BRANCHES with "[PRE-RELEASE]" and
        are skipped in IMAGE HEALTH and SPRINT RECOMMENDATION — no action.

   ⚙ Threshold:  OCP 4.x: minor >= 15 required for WMCO 10.x era (_WMCO10_MIN_OCP_MINOR)
                 OCP 5.x+: no minimum — all branches are checked for tags
                 Mapping: _ocp_to_wmco_major(ocp_major) = ocp_major + 6

3. IMAGE FRESHNESS GRADE
   Source:    Red Hat Container Catalog — freshness_grades[] on each image record
              Red Hat KB article: https://access.redhat.com/articles/2803031
   Data:      Time-series of letter grades (A → B → C → D → E → F), each with a
              start_date and end_date. Current grade = entry spanning today.
              The grade is time-based ("containers age like milk, not wine"): it
              measures the age of the *oldest unpatched* Critical or Important
              erratum, not the count of CVEs. Moderate and Low CVEs have zero
              impact on the grade. Grade F is also assigned to EOL image streams.
   Columns:
     Grade       — Current letter grade of the published WMCO image
     Threshold Date — First date the image reaches grade C or worse (C, D, E, or F):
                      • For A/B images: the deadline — ship before this to stay healthy
                      • For C/D/E/F images: when the image first crossed the threshold
                        (may show a D/E/F date if the image skipped C entirely)
   Why:       Grade B balances security hygiene with operational practicality.
              Grade A (zero pending errata) would trigger on any single erratum
              regardless of age, creating unnecessary urgency for minor fixes.
              Grade C tolerates Critical CVE fixes up to 30 days unaddressed —
              too permissive for an actively-maintained security-sensitive
              operator.  Grade B (Critical ≤ 7 days, Important ≤ 30 days)
              matches Red Hat's recommended container image security SLA.
   Decision:  Grade below B (i.e. C, D, E, or F) triggers a release recommendation.
              Grade A or B is considered acceptable unless the grade will drop
              below B within SPRINT_LOOKAHEAD_DAYS (default: 21 days). Sprint
              planning happens at the START of a sprint; the release may not
              ship until the END — so an upcoming degradation must be acted on
              now, before the grade has actually fallen.
   ⚙ Threshold:  Grade must be A or B to be clear (change GRADE_ORDER / grade_is_below_b
                 to accept C if the team decides C is tolerable)
                 Sprint lookahead: SPRINT_LOOKAHEAD_DAYS = 21 days (one sprint)

4. CVE VULNERABILITIES (published WMCO image)
   Source:    Red Hat Container Catalog — GET /images/id/{_id}/vulnerabilities
   Data:      All vulnerability records for the published WMCO image, tallied by
              the severity field: critical, important, moderate, low
   Columns:
     CVEs — Compact counts, e.g. "2C 1I 3M 5L" (zero-severity values omitted)
   Why:       The freshness grade (check 3) already encodes CVE timeliness: a
              Critical fix older than 7 days drops the grade to C or below,
              which is itself the release trigger.  Triggering separately on
              CVE counts would double-count the same signal and introduce false
              positives — a single Moderate CVE does not warrant a z-stream
              release on its own.  CVE counts answer "what gets fixed if we
              release now?" rather than "should we release?"
   Decision:  CVEs are displayed for informational purposes only and do NOT
              trigger a release recommendation. Use CVE counts to assess whether
              a release that is already needed will also resolve security issues.

5. BASE IMAGE REBUILD VALUE (ubi9/ubi-minimal:latest)
   Source:    Red Hat Container Catalog — freshness_grades on the latest ubi9/ubi-minimal image
   Data:      Threshold date of the current base image (first date its grade drops below B)
   Logic:     Compare the base image threshold date to the published WMCO image threshold date:
                ext ✓ — base threshold is later, or base has no near-term threshold:
                        rebuilding from the current base extends the grade window
                same  — base threshold equals the WMCO threshold: rebuild does not extend window
                ↓     — base threshold is earlier: rebuild may shorten the grade window
                —     — WMCO image has no threshold date (grade is stable)
                ?     — data unavailable
   Column:    Base (in IMAGE HEALTH table)
   Why:       CVE counts are unreliable for this comparison — a base image with more CVEs
              than the WMCO image would incorrectly show as 'same'. The threshold date is
              the correct measure because the CHI grade is time-based (age of oldest
              unpatched erratum), not count-based. Comparing threshold dates directly
              answers: "will a rebuild buy us more time before the grade drops?"
   Decision:  Advisory only — does NOT itself trigger a release recommendation.
              Used to answer: "if we build now, will the resulting image have a later
              threshold date than the currently published image?"
   Note:      Branches 4.18–4.20 use FROM ubi9/ubi-minimal:latest (no digest pin)
              and always pick up the current base at build time. Branch 4.21+ has
              Mintmaker-managed digest pins updated via bot PRs.

6. UNRELEASED PULL REQUESTS
   Source:    GitHub Compare API (GET /compare/{tag}...{branch})
              GitHub PR files API  (GET /pulls/{pr}/files) — for non-shipped check
   Data:      Merge commits on the branch HEAD since the last release tag
   Filtering:
     Bot PRs excluded — identified by head-branch prefix:
       konflux/, mintmaker/, renovate/, dependabot/
     Bot PRs also excluded by GitHub login:
       openshift-bot, openshift-merge-robot, openshift-ci-robot
     cherry-pick robot PRs are KEPT — they carry real bug/CVE fixes
     Non-shipped PRs excluded — PRs where ALL changed files match the CI
       skip-if-only-changed pattern. A file is non-shipped if it falls under
       test/, docs/, or hack/ (_NON_SHIPPED_PATH_PREFIXES) OR matches the CI
       job regex (_NON_SHIPPED_FILE_RE): ote/, .github/, .tekton/ directories;
       any *.md file; or root-level config files (.gitignore, .coderabbit.yaml,
       renovate.json, OWNERS, PROJECT, LICENSE, Containerfile, Containerfile.bundle).
       Checked via the PR files API; if the file list cannot be fetched, the
       PR is conservatively treated as shipped.
     Version-bump PRs excluded from action count — PRs whose title matches
       "Update version to X.Y.Z" (created by pre-release.sh). These are shown
       as [INFO] to indicate release prep has started, but do not themselves
       indicate a release is needed.
   Why:       Git history via the GitHub API is a credential-free, objective
              source of truth for whether code has been released.  Bot bump
              PRs (Konflux, Renovate, mintmaker, dependabot) are excluded
              because they carry no customer-visible logic change — they update
              dependencies or image references, not operator behavior.  The
              cherry-pick robot is kept because its PRs carry real bug and CVE
              fixes cherry-picked from master and therefore represent unshipped
              customer-facing changes.  Non-shipped paths are excluded because
              they do not affect the operator image and do not require a
              release to deliver value to customers. The set mirrors the CI
              job's skip-if-only-changed pattern (_NON_SHIPPED_FILE_RE) plus
              test/ and hack/ (_NON_SHIPPED_PATH_PREFIXES).
   Decision:  Any non-bot, non-non-shipped, non-version-bump team PR triggers
              a release recommendation.
   Note:      GitHub limits /compare to 250 commits. If ahead_by > 250, older PRs
              may be missing and the output will show a truncation warning.
   ⚙ Threshold:  team_prs > 0 (excluding bots, non-shipped, and version bumps) → action needed
                 (extend _NON_SHIPPED_PATH_PREFIXES or _NON_SHIPPED_FILE_RE to skip more paths)

7. JIRA RELEASE TRACKING  (optional — requires JIRA_API_TOKEN + JIRA_USERNAME)
   Source:    Jira POST /rest/api/3/search/jql
   Query:     project = WINC AND issuetype in (Epic, Task)
              AND summary ~ "release" AND statusCategory != Done
   Data:      Open release Epics and Tasks, matched to branches via fixVersion
              format "WMCO {wmco_major}.{minor}.{patch}" → OCP minor "{ocp_major}.{minor}"
              e.g. "WMCO 10.19.2" → OCP "4.19",  "WMCO 11.0.1" → OCP "5.0"
              Supports all WMCO major versions (10.x / OCP 4.x, 11.x / OCP 5.x, etc.)
   Display:   Shown as ↳ sub-lines under each branch in RELEASE BRANCHES and
              SPRINT RECOMMENDATION. Tasks are sorted before Epics (Tasks are
              the actionable, numbered work items).
   Why:       A branch can need a release with no open ticket (e.g. an urgent
              fix cherry-picked without filing a ticket first), and a branch
              can have an open ticket without needing a release (the ticket may
              track planning or prep work not yet landed).  Git history is the
              ground truth for whether PRs are unreleased; Jira tickets are a
              supplementary view for sprint planning context, not a decision
              input.  Requiring Jira credentials would also exclude team members
              who do not have access, whereas git history via the GitHub API is
              always available.
   Decision:  Purely informational — Jira ticket state does NOT affect whether
              a release is recommended. A branch can need a release with no ticket
              open, or have a ticket open and still be clear on other checks.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
RELEASE RECOMMENDATION LOGIC
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Three conditions are used because each has a direct, objective measure in the
available data and maps to concrete customer impact:
  - Unreleased PRs: merged code is in the branch but not yet available to
    customers running the published image.
  - Image grade (current): the existing published image has aged past the
    acceptable security threshold and needs a rebuild immediately.
  - Image grade (upcoming): sprint planning happens at the START of a 3-week
    sprint, but a new release may not ship until the END of the sprint.
    If the image grade will drop below B within SPRINT_LOOKAHEAD_DAYS (21
    days), the release must be started now — waiting until the grade
    actually drops means the shipped release will already be late.

All other signals are either derivable from these three or represent planning
state rather than shipping state:
  - CVE counts are captured by the grade (a Critical fix > 7 days old drops
    the grade to C, which is itself the trigger).
  - Jira ticket state reflects planning intent, not what code has shipped.
  - Base image CVE status is advisory — the image grade already reflects
    whether the base is contributing unpatched errata.

A branch appears in SPRINT RECOMMENDATION (action required) when ANY of the
following is true for an in-support, non-pre-release branch:

  ✗ UNRELEASED PRs   — one or more non-bot, non-shipped, non-version-bump
                        team PRs have merged since the last release tag
  ✗ IMAGE GRADE      — the published catalog image grade is below B (C, D, E, or F)
  ⚠ UPCOMING GRADE   — current grade is A or B but will drop below B within
                        SPRINT_LOOKAHEAD_DAYS days (default: 21 days)

All three conditions are independent — any one is sufficient.

Conditions that do NOT trigger a recommendation:
  • CVEs (any severity) — informational only; use CVE counts to assess whether
    a release that is already needed will also resolve security issues
  • A version-bump PR ("Update version to X.Y.Z") without other team PRs
  • PRs that only touch non-shipped paths (test/, docs/, hack/)
  • Base image (ubi9/ubi-minimal) still having CVEs — this is advisory only
  • Jira tickets being open, in-progress, or absent
  • Grade A or B — the image grade itself does not trigger action
"""

import argparse
import json
import os
import re
import sys
import time
from datetime import date, datetime

import requests

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

CATALOG_API = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/"
    "openshift4-wincw/windows-machine-config-rhel9-operator/images"
)
_CATALOG_UBI_MINIMAL_URL = (
    "https://catalog.redhat.com/api/containers/v1/repositories/"
    "registry/registry.access.redhat.com/repository/ubi9/ubi-minimal/images"
)
GITHUB_API = "https://api.github.com/repos/openshift/windows-machine-config-operator"
OCP_LIFECYCLE_API = (
    "https://access.redhat.com/product-life-cycles/api/v1/products/"
    "?name=OpenShift+Container+Platform"
)
JIRA_SEARCH_API = "https://redhat.atlassian.net/rest/api/3/search/jql"
# serverInfo is a GET endpoint that works without auth — used for connectivity probes.
_JIRA_SERVER_INFO_URL = "https://redhat.atlassian.net/rest/api/3/serverInfo"
JIRA_BROWSE = "https://redhat.atlassian.net/browse"

# OCP major → WMCO major offset. OCP 4 uses WMCO 10, OCP 5 uses WMCO 11, etc.
# All version derivations (tags, Jira fixVersions, support page lookups) use this.
_OCP_TO_WMCO_MAJOR_OFFSET = 6


def _ocp_to_wmco_major(ocp_major: int) -> int:
    """OCP 4 → WMCO 10, OCP 5 → WMCO 11, etc."""
    return ocp_major + _OCP_TO_WMCO_MAJOR_OFFSET


def _wmco_to_ocp_major(wmco_major: int) -> int:
    """WMCO 10 → OCP 4, WMCO 11 → OCP 5, etc."""
    return wmco_major - _OCP_TO_WMCO_MAJOR_OFFSET


# OCP 4.15 = WMCO 10.15 is the first v10.x release. OCP 4.X branches below this
# used WMCO v1.x–v9.x tags and will never match v10.x patterns; legacy EOM.
# There is no equivalent floor for OCP 5.x — all release-5.X branches are v11.x era.
_WMCO10_MIN_OCP_MINOR = 15

# WMCO began following OCP EUS Term 2 lifecycle starting with OCP 4.18 / WMCO 10.18.
# For OCP 4.x: EUS Term 2 applies to releases with minor >= 18 when the API provides a date.
# For OCP 5.x+: EUS Term 2 applies to all releases when the API provides a date,
# since WMCO 11.x (OCP 5.x) exists entirely within the EUS Term 2 era.
_WMCO_EUS_T2_MIN_OCP4_MINOR = 18

_VERSION_TAG_RE = re.compile(r"^v(\d+\.\d+\.\d+)$")

# Merge commit subject: "Merge pull request #NNN from owner/branch-name"
_MERGE_COMMIT_RE = re.compile(r"^Merge pull request #(\d+) from \S+?/(.+)")

# Branch name prefixes that identify bot-generated bump PRs.
# Note: openshift-cherrypick-robot PRs use "cherry-pick-NNN-to-branch" branches
# and are intentionally NOT listed here — they carry real bug fixes.
_BOT_BRANCH_PREFIXES = (
    "konflux/",
    "mintmaker/",
    "renovate/",
    "dependabot/",
)

# GitHub user logins that are infrastructure bots (secondary check via PR API).
_BOT_LOGINS = frozenset({
    "openshift-bot",
    "openshift-merge-robot",
    "openshift-ci-robot",
})

# Jira item pattern extracted from PR titles (e.g. "WINC-1234", "OCPBUGS-5678")
_JIRA_RE = re.compile(r"\b(WINC|OCPBUGS|RFE)-\d+\b")

# Version bump PRs created by pre-release.sh (e.g. "Update version to 10.18.3").
# These are informational — they do not themselves indicate a release is needed.
_VERSION_BUMP_RE = re.compile(r"\bUpdate version to \d+\.\d+\.\d+", re.IGNORECASE)

# Path prefixes that are NOT included in the shipped image (checked via str.startswith).
# Combined with _NON_SHIPPED_FILE_RE below; a file is non-shipped if it matches either.
_NON_SHIPPED_PATH_PREFIXES = (
    "test/",
    "docs/",
    "hack/",
)

# CI skip-if-only-changed regex — mirrors the pattern used in the CI job configuration
# to skip test runs when only non-code files change. A PR is excluded from the
# unreleased count when every changed file matches this regex OR a prefix above.
# Source: skip_if_only_changed in the CI job config.
_NON_SHIPPED_FILE_RE = re.compile(
    r"^(?:ote|docs|\.github|\.tekton)/"
    r"|\.md$"
    r"|^(?:\.gitignore|\.coderabbit\.yaml|renovate\.json|OWNERS|PROJECT|LICENSE"
    r"|Containerfile|Containerfile\.bundle)$"
)

# Sprint planning horizon (days). Sprint planning happens at the start of a
# 3-week sprint, but a new release may not ship until the end. The script flags
# branches whose image grade will drop below B within this window so the release
# can be started now rather than waiting until the grade has already degraded.
SPRINT_LOOKAHEAD_DAYS = 21

# ---------------------------------------------------------------------------
# Shared helpers (adapted from hack/verify-release.py)
# ---------------------------------------------------------------------------


def _github_headers() -> dict:
    token = os.environ.get("GITHUB_TOKEN")
    return {"Authorization": f"token {token}"} if token else {}


def _jira_auth() -> "tuple | None":
    """Return (username, api_token) for Jira Basic auth, or None if not configured."""
    token = os.environ.get("JIRA_API_TOKEN")
    user = os.environ.get("JIRA_USERNAME")
    return (user, token) if token and user else None


def _get(url, *, retries=3, delay=2, **kwargs) -> requests.Response:
    """Retry-enabled GET wrapper for transient network errors."""
    retries = max(1, retries)  # always attempt at least once
    last_exc = None
    for attempt in range(retries):
        try:
            return requests.get(url, **kwargs)
        except requests.exceptions.ConnectionError as exc:
            last_exc = exc
            if attempt < retries - 1:
                time.sleep(delay)
        except requests.RequestException:
            raise
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
    page_size = 100
    while True:
        params = {"page_size": page_size, "page": page, "sort_by": "creation_date[desc]"}
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


def _version_from_tags(repos: list) -> str:
    """Extract x.y.z version string from a catalog image's repository tag list."""
    for repo in repos:
        for tag in repo.get("tags", []):
            m = _VERSION_TAG_RE.match(tag.get("name", ""))
            if m:
                return m.group(1)
    return ""


def _version_key(v: str) -> tuple:
    try:
        return tuple(int(x) for x in v.split("."))
    except ValueError:
        return (0, 0, 0)


# ---------------------------------------------------------------------------
# OCP Lifecycle API dates
# ---------------------------------------------------------------------------

# {ocp_minor: {"eom": "YYYY-MM-DD"}}  keyed by OCP minor string (e.g. "4.18")
_support_dates_cache = None


def _parse_api_date(date_val: str, date_fmt: str) -> "str | None":
    """
    Parse an OCP lifecycle API phase date to "YYYY-MM-DD", or None.
    The API uses date_format="date" for real ISO timestamps and
    date_format="string" for placeholder text ("N/A", formula strings, etc.).
    ISO timestamps have the form "2026-08-25T00:00:00.000Z".
    """
    if date_fmt != "date" or not date_val or date_val.upper() == "N/A":
        return None
    # Take only the date portion before the "T" separator.
    return date_val[:10]


def _fetch_support_dates() -> dict:
    """
    Fetch OCP lifecycle data from the Red Hat Product Life Cycles API and return
    a mapping of OCP minor version string to EOM date:
      {"4.18": {"eom": "2028-02-25"}, "4.19": {"eom": "2026-12-17"}, ...}

    The API returns each version with a "phases" array. Each phase entry has:
      "name"        — phase name (e.g. "Maintenance support")
      "end_date"    — ISO timestamp or "N/A"
      "date_format" — "date" for real dates, "string" for N/A / formula text

    EOM date selection per version:
      - OCP 4.18+ with a non-null EUS Term 2 end date: use "Extended update
        support Term 2" end_date. WMCO started supporting EUS Term 2 with
        OCP 4.18 / WMCO 10.18.
      - All other releases: use "Maintenance support" end_date.

    Result is cached after the first fetch.
    """
    global _support_dates_cache
    if _support_dates_cache is not None:
        return _support_dates_cache

    try:
        resp = _get(OCP_LIFECYCLE_API, timeout=15)
        resp.raise_for_status()
        data = resp.json()
    except requests.RequestException as exc:
        raise RuntimeError(f"Failed to fetch OCP lifecycle API: {exc}") from exc
    except ValueError as exc:
        raise RuntimeError(f"Invalid JSON from OCP lifecycle API: {exc}") from exc

    products = data.get("data", [])
    if not products:
        raise RuntimeError("OCP lifecycle API returned no product data")

    dates_map = {}
    for ver in products[0].get("versions", []):
        name = ver.get("name", "").strip()
        if not name:
            continue
        parts = name.split(".")
        if len(parts) != 2:
            continue
        try:
            ocp_major_int = int(parts[0])
            ocp_minor_int = int(parts[1])
        except ValueError:
            continue

        # Walk the phases array to extract maintenance support and EUS Term 2 dates.
        maint_end = None
        eus_t2_end = None
        for phase in ver.get("phases", []):
            phase_name = phase.get("name", "")
            parsed = _parse_api_date(
                phase.get("end_date", ""), phase.get("date_format", "")
            )
            if phase_name == "Maintenance support":
                maint_end = parsed
            elif phase_name == "Extended update support Term 2":
                eus_t2_end = parsed

        # Use EUS Term 2 date when the API provides one and WMCO supports it:
        #   OCP 4.x: EUS Term 2 support began with 4.18 / WMCO 10.18
        #   OCP 5.x+: EUS Term 2 applies to all versions (WMCO 11.x era)
        eus_eligible = (
            ocp_major_int == 4 and ocp_minor_int >= _WMCO_EUS_T2_MIN_OCP4_MINOR
        ) or ocp_major_int > 4
        use_eus_t2 = eus_eligible and eus_t2_end
        eom = eus_t2_end if use_eus_t2 else maint_end

        dates_map[name] = {"eom": eom}

    _support_dates_cache = dates_map
    return dates_map


def _lookup_support_dates(ocp_minor: str, support_dates: dict) -> dict:
    """
    Look up support dates for an OCP minor version (e.g. "4.18").
    The OCP lifecycle API uses OCP minor format keys ("4.18", "4.21").
    Returns the matching dict, or {} if the version is not listed.
    """
    return support_dates.get(ocp_minor) or {}


def _compute_eom_note(ocp_minor: str) -> str:
    """
    Compute a human-readable EOM status note for a branch that is not in the
    current (RHEL9) catalog. Tries the OCP lifecycle API for an authoritative
    EOM date; falls back to a generic label if the API has no data.
    """
    ocp_parts = ocp_minor.split(".")
    ocp_major_int = int(ocp_parts[0])
    ocp_minor_int = int(ocp_parts[1])

    # Branches that predate the WMCO 10.x / RHEL9 era never had RHEL9 images.
    if ocp_major_int == 4 and ocp_minor_int < _WMCO10_MIN_OCP_MINOR:
        return "EOM (pre-RHEL9)"

    # For WMCO 10+ branches that had releases but are absent from the current
    # catalog, look up the OCP lifecycle API for the actual EOM date.
    try:
        support_dates = _fetch_support_dates()
    except RuntimeError:
        return "EOM (unknown)"

    dates = _lookup_support_dates(ocp_minor, support_dates)
    eom_str = dates.get("eom")

    if eom_str:
        return f"EOM {eom_str}"
    return "EOM (unknown)"


# ---------------------------------------------------------------------------
# Catalog data
# ---------------------------------------------------------------------------


def fetch_catalog_versions() -> list:
    """
    Fetch all published WMCO operator image records from the Red Hat Container Catalog.
    Returns list of dicts with version, ocp_minor, published_date, freshness_grades,
    container_grades_msg, and build_commit. Deduplicated by version, sorted newest-first.
    """
    raw = _fetch_images_from(CATALOG_API)
    seen = {}
    for img in raw:
        repos = img.get("repositories", [])
        version = _version_from_tags(repos)
        if not version or version in seen:
            continue

        published_date = None
        for repo in repos:
            pd = repo.get("push_date")
            if pd:
                published_date = pd[:10]
                break

        freshness_grades = img.get("freshness_grades", [])

        cg = img.get("container_grades", {})
        container_grades_msg = cg.get("status_message", "") if isinstance(cg, dict) else ""

        labels = {
            lbl.get("name"): lbl.get("value")
            for lbl in img.get("parsed_data", {}).get("labels", [])
            if lbl.get("name")
        }
        build_commit = labels.get("org.opencontainers.image.revision", "")

        # WMCO 10.18.2 → OCP minor "4.18", WMCO 11.0.1 → OCP minor "5.0"
        parts = version.split(".")
        if len(parts) >= 2:
            wmco_major = int(parts[0])
            ocp_minor = f"{_wmco_to_ocp_major(wmco_major)}.{parts[1]}"
        else:
            ocp_minor = ""

        seen[version] = {
            "version": version,
            "image_internal_id": img.get("_id", ""),
            "ocp_minor": ocp_minor,
            "published_date": published_date,
            "freshness_grades": freshness_grades,
            "container_grades_msg": container_grades_msg,
            "build_commit": build_commit,
        }

    return sorted(seen.values(), key=lambda x: _version_key(x["version"]), reverse=True)


def get_latest_version_per_branch(all_versions: list) -> dict:
    """
    Group catalog versions by OCP minor and return only the latest per branch.
    Returns {"4.18": <version dict>, "4.19": <version dict>, ...}
    """
    by_branch = {}
    for v in all_versions:  # already sorted newest-first
        branch = v["ocp_minor"]
        if branch not in by_branch:
            by_branch[branch] = v
    return by_branch


# ---------------------------------------------------------------------------
# Support window
# ---------------------------------------------------------------------------


def annotate_support_status(latest_by_branch: dict) -> dict:
    """
    Annotate each branch entry with support status fields:
      - in_support: bool
      - eom_date: "YYYY-MM-DD" or None
      - support_note: human-readable status string

    EOM dates come from the OCP lifecycle API. For OCP 4.18+ EUS releases,
    the EUS Term 2 date is used when available (WMCO supports EUS Term 2 from 10.18+).
    For all other releases, the Maintenance support end date is used.
    """
    try:
        support_dates = _fetch_support_dates()
    except RuntimeError as exc:
        print(f"ERROR: Could not fetch OCP lifecycle API: {exc}", file=sys.stderr)
        sys.exit(2)

    today = date.today()

    result = {}
    for ocp_minor, data in latest_by_branch.items():
        dates = _lookup_support_dates(ocp_minor, support_dates)

        entry = dict(data)
        eom_date_str = dates.get("eom")

        if eom_date_str:
            eom_date = datetime.strptime(eom_date_str, "%Y-%m-%d").date()
            in_support = today <= eom_date
            support_note = f"Active (EOM {eom_date_str})" if in_support else f"EOM {eom_date_str}"
        else:
            # Not in API response — the API only lists versions currently in some
            # support phase, so absence means this version has passed its EOM.
            # Distinguish: if a *newer* version of the same OCP major IS in the
            # API, this version has definitely rolled off. If no newer version is
            # found (e.g. the API hasn't listed a very recently released version),
            # treat it as active until data appears.
            ocp_parts = ocp_minor.split(".")
            ocp_major_int = int(ocp_parts[0])
            ocp_minor_int = int(ocp_parts[1])
            newer_in_api = any(
                k.split(".")[0] == str(ocp_major_int)
                and len(k.split(".")) == 2
                and k.split(".")[1].isdigit()
                and int(k.split(".")[1]) > ocp_minor_int
                for k in support_dates
            )
            eom_date_str = None
            if newer_in_api:
                in_support = False
                support_note = "EOM (not in lifecycle API)"
            else:
                in_support = True
                support_note = "Active (not yet in lifecycle API)"

        entry.update(
            {
                "eom_date": eom_date_str,
                "in_support": in_support,
                "support_note": support_note,
            }
        )
        result[ocp_minor] = entry

    return result


# ---------------------------------------------------------------------------
# Image health
# ---------------------------------------------------------------------------

GRADE_ORDER = {"A": 0, "B": 1, "C": 2, "D": 3, "E": 4, "F": 5, "?": 6}


def get_current_freshness_grade(freshness_grades: list) -> tuple:
    """
    Return (current_grade, grade_expires_date) where grade_expires_date is "YYYY-MM-DD" or None.
    Finds the freshness_grades entry spanning today.
    """
    today_str = date.today().isoformat()
    for entry in freshness_grades:
        start = entry.get("start_date", "")[:10]
        end_raw = entry.get("end_date")
        end = end_raw[:10] if end_raw else None
        grade = entry.get("grade", "?")
        if start <= today_str and (end is None or today_str < end):
            return grade, end
    return "?", None


def grade_is_below_b(grade: str) -> bool:
    """Return True if grade is below B (i.e. C, D, E, or F), indicating action needed."""
    return GRADE_ORDER.get(grade, 5) > GRADE_ORDER["B"]


def _grade_expires_within_sprint(threshold_date: "str | None") -> bool:
    """
    Return True if the image will drop below grade B within SPRINT_LOOKAHEAD_DAYS days.
    Used to flag images that are currently A or B but will degrade before a release
    started today could realistically ship.
    """
    if not threshold_date:
        return False
    try:
        td = datetime.strptime(threshold_date, "%Y-%m-%d").date()
    except ValueError:
        return False
    days_until = (td - date.today()).days
    return 0 < days_until <= SPRINT_LOOKAHEAD_DAYS


def get_threshold_date(freshness_grades: list) -> "str | None":
    """
    Return the start_date of the first freshness_grades entry at C or below (C, D, E, or F).
    For images currently at A or B this is the deadline for shipping a new release
    before the grade falls below the acceptable threshold.
    For images already below B the catalog API generates freshness_grades dynamically
    from the current date, so start_date of the current entry reflects today rather
    than the actual historical crossing date. Callers should not display this value
    as a meaningful past date for already-below-B images (use grade_warn to detect
    this case and suppress the date in the UI).
    """
    for entry in freshness_grades:
        g = entry.get("grade", "?")
        if GRADE_ORDER.get(g, 5) >= GRADE_ORDER["C"]:
            return entry.get("start_date", "")[:10]
    return None


# ---------------------------------------------------------------------------
# CVE / vulnerability data
# ---------------------------------------------------------------------------

_CVE_SEVERITIES = ("critical", "important", "moderate", "low")

_CATALOG_CVE_URL = (
    "https://catalog.redhat.com/api/containers/v1/images/id/{image_id}/vulnerabilities"
)


def fetch_image_cves(image_internal_id: str) -> dict:
    """
    Fetch CVE vulnerability counts for a catalog image by its internal _id.
    Paginates the /vulnerabilities endpoint and tallies counts by severity.

    Returns:
        {"critical": N, "important": N, "moderate": N, "low": N, "total": N, "error": None}
    On fetch failure, returns zeroed counts with "error" set to the error string.
    """
    url = _CATALOG_CVE_URL.format(image_id=image_internal_id)
    counts = {s: 0 for s in _CVE_SEVERITIES}
    page = 0
    fetched = 0

    while True:
        try:
            resp = _get(url, params={"page_size": 100, "page": page}, timeout=30)
            resp.raise_for_status()
        except requests.RequestException as exc:
            return {**counts, "total": sum(counts.values()), "error": str(exc)}

        data = resp.json()
        batch = data.get("data", [])
        total = data.get("total", 0)

        for vuln in batch:
            severity = vuln.get("severity", "").lower()
            if severity in counts:
                counts[severity] += 1

        fetched += len(batch)
        if fetched >= total or not batch:
            break
        page += 1

    return {**counts, "total": sum(counts.values()), "error": None}


def _format_cve_counts(cve_counts: "dict | None") -> str:
    """
    Format CVE counts as a compact string showing only non-zero severities.
    E.g. {"critical":0,"important":1,"moderate":1,"low":1} → "1I 1M 1L"
    Returns "—" for no CVEs, "?" if the fetch errored.
    """
    if cve_counts is None:
        return "—"
    if cve_counts.get("error"):
        return "?"
    labels = {"critical": "C", "important": "I", "moderate": "M", "low": "L"}
    parts = [f"{cve_counts[s]}{labels[s]}" for s in _CVE_SEVERITIES if cve_counts.get(s)]
    return " ".join(parts) if parts else "—"


def _has_actionable_cves(cve_counts: "dict | None") -> bool:
    """Return True if the image has Critical or Important CVEs (warrants a release)."""
    if not cve_counts or cve_counts.get("error"):
        return False
    return cve_counts.get("critical", 0) > 0 or cve_counts.get("important", 0) > 0


_base_image_data_cache = None


def fetch_base_image_data() -> "dict | None":
    """
    Fetch freshness grade data for the current ubi9/ubi-minimal:latest base image.
    Returns {"grade": str, "threshold_date": str or None, "error": None}, or None on failure.
    Result is cached — all WMCO branches share the same base image.

    Used to determine whether rebuilding WMCO from the current base image would extend
    the threshold date (the date when the image grade first drops below B).
    """
    global _base_image_data_cache
    if _base_image_data_cache is not None:
        return _base_image_data_cache

    try:
        resp = _get(
            _CATALOG_UBI_MINIMAL_URL,
            params={"page_size": 1, "page": 0, "sort_by": "creation_date[desc]"},
            timeout=30,
        )
        resp.raise_for_status()
        batch = resp.json().get("data", [])
        if not batch:
            return None
        img = batch[0]
    except requests.RequestException as exc:
        _base_image_data_cache = {"grade": "?", "threshold_date": None, "error": str(exc)}
        return _base_image_data_cache

    freshness_grades = img.get("freshness_grades", [])
    grade, _ = get_current_freshness_grade(freshness_grades)
    threshold_date = get_threshold_date(freshness_grades)
    _base_image_data_cache = {"grade": grade, "threshold_date": threshold_date, "error": None}
    return _base_image_data_cache


def _base_image_rebuild_label(wmco_threshold: "str | None", base_data: "dict | None") -> str:
    """
    Return a label describing whether rebuilding from the current base image would extend
    the threshold date (the date the published image's grade will first drop below B).

    '—'      : WMCO image has no threshold date — grade is stable, no comparison needed
    'ext ✓'  : base threshold is later (or absent) — rebuilding extends the grade window
    'same'   : base and WMCO have the same threshold date — rebuild does not extend window
    '↓'      : base threshold is earlier — rebuilding may shorten the grade window
    '?'      : data unavailable
    """
    if base_data is None or base_data.get("error"):
        return "?"
    if wmco_threshold is None:
        return "—"  # WMCO grade is stable, no threshold to compare
    base_threshold = base_data.get("threshold_date")
    if base_threshold is None or base_threshold > wmco_threshold:
        return "ext ✓"  # base is clean or has a later threshold
    if base_threshold == wmco_threshold:
        return "same"
    return "↓"  # base threshold is earlier than WMCO's


def _is_action_needed(results: list) -> bool:
    """Return True if any supported branch needs a z-stream release."""
    return any(
        not r.get("pre_release")
        and r.get("in_support")
        and (
            any(
                not pr.get("is_version_bump")
                for pr in (r.get("unreleased") or {}).get("team_prs", [])
            )
            or r.get("grade_warn")
            or r.get("grade_deadline_warn")
            or (r.get("unreleased") or {}).get("error")
        )
        for r in results
    )


# ---------------------------------------------------------------------------
# GitHub: release branches and tags
# ---------------------------------------------------------------------------

_RELEASE_BRANCH_RE = re.compile(r"^release-(\d+)\.(\d+)$")


def fetch_github_release_branches() -> list:
    """Fetch all release-X.Y branch names from GitHub, sorted by (major, minor)."""
    branches = []
    page = 1
    while True:
        url = f"{GITHUB_API}/branches"
        params = {"per_page": 100, "page": page}
        resp = _get(url, headers=_github_headers(), params=params, timeout=30)
        resp.raise_for_status()
        batch = resp.json()
        if not batch:
            break
        for b in batch:
            if _RELEASE_BRANCH_RE.match(b.get("name", "")):
                branches.append(b["name"])
        if len(batch) < 100:
            break
        page += 1
    return sorted(branches, key=lambda b: tuple(int(x) for x in b[len("release-"):].split(".")))


def fetch_github_tags() -> dict:
    """Fetch all tags from GitHub. Returns {tag_name: commit_sha}."""
    tags = {}
    page = 1
    while True:
        url = f"{GITHUB_API}/tags"
        params = {"per_page": 100, "page": page}
        resp = _get(url, headers=_github_headers(), params=params, timeout=30)
        resp.raise_for_status()
        batch = resp.json()
        if not batch:
            break
        for t in batch:
            tags[t["name"]] = t["commit"]["sha"]
        if len(batch) < 100:
            break
        page += 1
    return tags


def find_latest_tag_for_branch(ocp_minor: str, all_tags: dict) -> "str | None":
    """
    Given an OCP minor version like "4.18" or "5.0", find the highest WMCO release tag
    (e.g. "v10.18.2" or "v11.0.1"). Returns None if no tags exist (pre-release branch).
    """
    ocp_parts = ocp_minor.split(".")
    wmco_major = _ocp_to_wmco_major(int(ocp_parts[0]))
    minor = ocp_parts[1]  # e.g. "18" or "0"
    pattern = re.compile(rf"^v{wmco_major}\.{minor}\.(\d+)$")
    candidates = []
    for tag_name in all_tags:
        m = pattern.match(tag_name)
        if m:
            candidates.append((int(m.group(1)), tag_name))
    if not candidates:
        return None
    candidates.sort(reverse=True)
    return candidates[0][1]


def _find_pre_release_base_tag(prev_ocp_minor: str, all_tags: dict) -> "str | None":
    """
    Return the compare base tag for a pre-release branch.

    Prefers the GA tag (vX.Y.0) of the previous OCP minor because the pre-release
    branch is cut around the time of the previous minor's GA — commits since that tag
    are what will appear in the first release of the new branch. Falls back to the
    highest patch tag if no GA tag exists.
    """
    ocp_parts = prev_ocp_minor.split(".")
    wmco_major = _ocp_to_wmco_major(int(ocp_parts[0]))
    minor = ocp_parts[1]
    ga_tag = f"v{wmco_major}.{minor}.0"
    if ga_tag in all_tags:
        return ga_tag
    return find_latest_tag_for_branch(prev_ocp_minor, all_tags)


# ---------------------------------------------------------------------------
# GitHub: unreleased pull requests
# ---------------------------------------------------------------------------


def _fetch_pr_details(pr_number: str) -> "dict | None":
    """
    Fetch a single PR's details from the GitHub API.
    Returns a dict with pr_number, title, author, is_bot, jira, merged_at.
    Returns None if the fetch fails.
    """
    url = f"{GITHUB_API}/pulls/{pr_number}"
    try:
        resp = _get(url, headers=_github_headers(), timeout=15)
        if resp.status_code != 200:
            return None
        pr = resp.json()
    except requests.RequestException:
        return None

    user = pr.get("user", {})
    login = user.get("login", "")
    # GitHub marks bots explicitly, or via [bot] suffix on login
    is_bot = (
        user.get("type") == "Bot"
        or login.endswith("[bot]")
        or login in _BOT_LOGINS
    )

    title = pr.get("title", "")
    jira_m = _JIRA_RE.search(title)
    return {
        "pr_number": pr_number,
        "title": title[:80],
        "author": login,
        "is_bot": is_bot,
        "jira": jira_m.group(0) if jira_m else "",
        "merged_at": (pr.get("merged_at") or "")[:10],
        "is_version_bump": bool(_VERSION_BUMP_RE.search(title)),
    }


def _is_non_shipped_file(filename: str) -> bool:
    """
    Return True if a file does not contribute to the shipped image.
    Matches _NON_SHIPPED_PATH_PREFIXES (test/, docs/, hack/) OR _NON_SHIPPED_FILE_RE
    (CI skip-if-only-changed pattern: ote/, .github/, .tekton/, *.md, root config files).
    """
    return (
        any(filename.startswith(p) for p in _NON_SHIPPED_PATH_PREFIXES)
        or bool(_NON_SHIPPED_FILE_RE.search(filename))
    )


def _is_non_shipped_pr(pr_number: str) -> "bool | None":
    """
    Return True if every file changed by this PR is non-shipped (test/, docs/, hack/,
    or matches the CI skip-if-only-changed pattern). Returns None on fetch failure;
    callers should treat None as shipped (conservative).
    """
    url = f"{GITHUB_API}/pulls/{pr_number}/files"
    all_files = []
    page = 1
    while True:
        try:
            resp = _get(
                url, headers=_github_headers(),
                params={"per_page": 100, "page": page}, timeout=15,
            )
            if resp.status_code != 200:
                return None
            batch = resp.json()
        except requests.RequestException:
            return None
        if not batch:
            break
        all_files.extend(batch)
        if len(batch) < 100:
            break
        page += 1

    if not all_files:
        return None  # no files listed — treat as shipped to be safe

    return all(_is_non_shipped_file(f.get("filename", "")) for f in all_files)


def fetch_unreleased_prs(last_tag: str, branch: str, tick=None) -> dict:
    """
    Use the GitHub Compare API to find merge commits on `branch` since `last_tag`,
    then fetch PR details for each non-bot merge.

    Only merge commits are considered (one per merged PR). Individual commits
    within a PR are intentionally ignored. Bot bump PRs (Konflux, Renovate,
    mintmaker, dependabot) are filtered out; cherry-pick robot PRs are kept.
    PRs where every changed file is in a non-shipped path (test/, docs/, hack/)
    are also filtered out — they carry no customer-facing change.

    tick: optional callable invoked once per team PR processed in pass 2
          (called regardless of whether the PR is kept or filtered). Used by
          callers to display progress dots as each PR detail + file check completes.

    Returns:
        ahead_by          — total commit count between tag and branch HEAD
        total_prs         — number of merge commits found (team + bot)
        team_prs          — list of shipped, non-bot PR dicts
        bot_filtered      — count of bot PRs excluded
        non_shipped_filtered — count of non-shipped (docs/test/hack-only) PRs excluded
        truncated         — True if ahead_by > 250 (GitHub limit; older PRs may be missing)
        error             — error string or None
    """
    url = f"{GITHUB_API}/compare/{last_tag}...{branch}"
    resp = _get(url, headers=_github_headers(), timeout=30)

    if resp.status_code == 404:
        return {
            "ahead_by": 0, "total_prs": 0, "team_prs": [],
            "bot_filtered": 0, "non_shipped_filtered": 0,
            "truncated": False,
            "error": f"Compare not found: {last_tag}...{branch}",
        }
    resp.raise_for_status()

    data = resp.json()
    ahead_by = data.get("ahead_by", 0)
    raw_commits = data.get("commits", [])

    # Pass 1: identify merge commits and classify as bot vs. team by branch name
    team_pr_numbers = []
    bot_filtered = 0

    for c in raw_commits:
        subject = c.get("commit", {}).get("message", "").split("\n")[0]
        m = _MERGE_COMMIT_RE.match(subject)
        if not m:
            continue  # individual commit inside a PR — skip

        pr_number = m.group(1)
        head_branch = m.group(2)  # e.g. "konflux/references/release-4.18"

        if any(head_branch.startswith(p) for p in _BOT_BRANCH_PREFIXES):
            bot_filtered += 1
        else:
            team_pr_numbers.append(pr_number)

    # Pass 2: fetch full PR details for team PRs to get title and Jira item,
    # then check the changed-file list to filter non-shipped (docs/test/hack) PRs.
    # tick() is called once per PR regardless of outcome so the caller can show
    # a progress dot for each remote round-trip.
    team_prs = []
    non_shipped_filtered = 0
    for pr_num in team_pr_numbers:
        details = _fetch_pr_details(pr_num)
        if not details:
            # Include with minimal info if the fetch fails
            if tick:
                tick()
            team_prs.append({
                "pr_number": pr_num, "title": "(PR details unavailable)",
                "author": "", "is_bot": False, "jira": "", "merged_at": "",
                "is_version_bump": False,
            })
            continue
        if details.get("is_bot"):
            # Caught at API level (e.g. bot login not covered by branch prefix)
            bot_filtered += 1
            if tick:
                tick()
            continue
        # Filter PRs that only touch non-shipped paths (test/, docs/, hack/).
        # If the file list is unavailable, conservatively treat the PR as shipped.
        if _is_non_shipped_pr(pr_num):
            non_shipped_filtered += 1
            if tick:
                tick()
            continue
        team_prs.append(details)
        if tick:
            tick()

    return {
        "ahead_by": ahead_by,
        "total_prs": len(team_pr_numbers) + bot_filtered,
        "team_prs": team_prs,
        "bot_filtered": bot_filtered,
        "non_shipped_filtered": non_shipped_filtered,
        "truncated": ahead_by > len(raw_commits),
        "error": None,
    }


# ---------------------------------------------------------------------------
# Jira release tracking
# ---------------------------------------------------------------------------

def fetch_jira_release_tickets() -> "dict | None":
    """
    Fetch open release Epics and Tasks from the WINC Jira project.

    fixVersions use the format "WMCO 10.{minor}.{patch}", which maps directly to
    OCP minor version 4.{minor}. Both Epics (containers) and Tasks (actionable,
    with numbered sub-tasks) are returned; Tasks are listed first per branch.

    Returns:
        {ocp_minor: [{"key", "summary", "status", "version", "issuetype", "url"}]}
        or None if JIRA_API_TOKEN / JIRA_USERNAME are not set.
        On fetch errors returns {} (empty dict, not None) so callers can distinguish
        "not configured" from "configured but failed".
    """
    auth = _jira_auth()
    if auth is None:
        return None

    jql = (
        'project = WINC AND issuetype in (Epic, Task) AND summary ~ "release" '
        "AND statusCategory != Done ORDER BY updated DESC"
    )
    payload = {
        "jql": jql,
        "fields": ["summary", "status", "fixVersions", "issuetype"],
        "maxResults": 50,
    }
    try:
        resp = requests.post(
            JIRA_SEARCH_API, auth=auth, json=payload,
            headers={"Accept": "application/json", "Content-Type": "application/json"},
            timeout=15,
        )
        resp.raise_for_status()
    except requests.RequestException as exc:
        print(f"WARNING: Jira fetch failed: {exc}", file=sys.stderr)
        return {}

    issues = resp.json().get("issues", [])
    result = {}

    for issue in issues:
        fields = issue.get("fields", {})
        for fv in fields.get("fixVersions", []):
            name = fv.get("name", "")  # e.g. "WMCO 10.19.2" or "WMCO 11.0.1"
            if not name.startswith("WMCO "):
                continue
            version_str = name[5:]  # "10.19.2" or "11.0.1"
            parts = version_str.split(".")
            if len(parts) != 3:
                continue
            try:
                wmco_major = int(parts[0])
            except ValueError:
                continue
            ocp_major = _wmco_to_ocp_major(wmco_major)
            if ocp_major < 4:  # skip any pre-OCP-4 fixVersions
                continue
            ocp_minor = f"{ocp_major}.{parts[1]}"
            result.setdefault(ocp_minor, []).append({
                "key": issue["key"],
                "summary": fields.get("summary", "").strip(),
                "status": fields.get("status", {}).get("name", ""),
                "version": version_str,
                "issuetype": fields.get("issuetype", {}).get("name", ""),
                "url": f"{JIRA_BROWSE}/{issue['key']}",
            })

    # Within each branch, sort Tasks before Epics (Tasks are the actionable items).
    for tickets in result.values():
        tickets.sort(key=lambda t: (0 if t["issuetype"] == "Task" else 1, t["key"]))

    return result


# ---------------------------------------------------------------------------
# Check runner
# ---------------------------------------------------------------------------


def run_checks(
    branch_data: dict,
    all_tags: dict,
    all_github_branches: list,
    include_eol: bool = False,
    filter_branch: "str | None" = None,
    jira_tickets: "dict | None" = None,
    base_image_data: "dict | None" = None,
    progress: bool = True,
    pre_release_prs: bool = False,
) -> list:
    """
    For each branch, collect image health and unreleased commit data.
    Pre-release branches (in GitHub but not in catalog) are included with status PRE-RELEASE.
    Returns list of result dicts sorted by OCP minor version.

    progress: when True, print per-branch status lines with dots for each remote
              API call (CVE fetch, PR detail + file checks).
    """
    results = []

    # Build the set of all branches to consider.
    # Branches in GitHub but NOT in the catalog fall into two categories:
    #   1. True pre-release: branch exists, but NO release tags exist yet
    #      (the newest branch, which still tracks master)
    #   2. Old EOL: branch exists, tags exist, but images used an older catalog
    #      (RHEL8-era branches before OCP 4.18 are not in the RHEL9 catalog)
    catalog_minors = set(branch_data)  # e.g. {"4.18", "4.19", "5.0"}
    github_minors = set()
    for b in all_github_branches:
        m = _RELEASE_BRANCH_RE.match(b)
        if m:
            github_minors.add(f"{m.group(1)}.{m.group(2)}")

    no_catalog_minors = github_minors - catalog_minors

    # Determine which no-catalog branches are truly pre-release (no tags) vs. old EOL.
    # OCP 4.X branches below 4.15 predate WMCO 10.x and are always legacy EOL.
    # OCP 5.X+ branches have no legacy floor — all are either pre-release or EOL by tags.
    true_pre_release = set()
    old_eol = set()
    for ocp_minor in no_catalog_minors:
        ocp_major = int(ocp_minor.split(".")[0])
        ocp_minor_int = int(ocp_minor.split(".")[1])
        if ocp_major == 4 and ocp_minor_int < _WMCO10_MIN_OCP_MINOR:
            # OCP 4.X before WMCO 10.x era — no v10.x tags exist by design
            old_eol.add(ocp_minor)
        else:
            tag = find_latest_tag_for_branch(ocp_minor, all_tags)
            if tag is None:
                true_pre_release.add(ocp_minor)
            else:
                old_eol.add(ocp_minor)

    # Combine: catalog branches + true pre-release branches
    # Old EOL branches (RHEL8-era) are only shown with --all
    all_minors = catalog_minors | true_pre_release
    if include_eol:
        all_minors |= old_eol

    for ocp_minor in sorted(all_minors, key=lambda v: tuple(int(x) for x in v.split("."))):
        branch_name = f"release-{ocp_minor}"

        if filter_branch and branch_name != filter_branch:
            continue

        # True pre-release branch: exists in GitHub, no release tags yet.
        # Compare against the GA tag of the previous OCP minor (e.g. v10.21.0 for
        # release-4.22) to show what will appear in the first release of this branch.
        if ocp_minor in true_pre_release:
            # Derive the previous minor from the known set rather than arithmetic
            # decrement, which breaks at major-version boundaries (e.g. 5.0 → 5.-1).
            ocp_key = tuple(int(x) for x in ocp_minor.split("."))
            known_sorted = sorted(
                github_minors | catalog_minors,
                key=lambda v: tuple(int(x) for x in v.split(".")),
            )
            earlier = [m for m in known_sorted
                       if tuple(int(x) for x in m.split(".")) < ocp_key]
            prev_ocp_minor = earlier[-1] if earlier else None
            base_tag = (
                _find_pre_release_base_tag(prev_ocp_minor, all_tags)
                if prev_ocp_minor else None
            )

            unreleased_data = None
            if not base_tag:
                if progress:
                    print(f"  {branch_name}: [pre-release] (no previous tag found)")
            elif not pre_release_prs:
                if progress:
                    print(f"  {branch_name}: [pre-release] (use --pre-release-prs to show PRs)")
            else:
                if progress:
                    print(f"  {branch_name}: [pre-release] PRs", end="", flush=True)

                def _pre_tick():
                    if progress:
                        print(".", end="", flush=True)

                try:
                    unreleased_data = fetch_unreleased_prs(
                        base_tag, branch_name, tick=_pre_tick
                    )
                except requests.RequestException as exc:
                    unreleased_data = {
                        "ahead_by": 0, "total_prs": 0, "team_prs": [],
                        "bot_filtered": 0, "non_shipped_filtered": 0,
                        "truncated": False, "error": str(exc),
                    }
                if progress:
                    print(" done")

            results.append(
                {
                    "branch": branch_name,
                    "ocp_minor": ocp_minor,
                    "pre_release": True,
                    "in_support": False,
                    "support_note": "Pre-release (no catalog entry yet)",
                    "version": None,
                    "published_date": None,
                    "freshness_grade": None,
                    "grade_expires": None,
                    "grade_warn": False,
                    "grade_deadline_warn": False,
                    "threshold_date": None,
                    "cve_counts": None,
                    "base_image_data": base_image_data,
                    "security_errata": "",
                    "security_warn": False,
                    "latest_tag": base_tag,
                    "unreleased": unreleased_data,
                    "jira_tickets": (jira_tickets or {}).get(ocp_minor, []),
                }
            )
            continue

        # Old EOL branch: has tags but no RHEL9 catalog entry.
        # Pre-WMCO10 branches (< 4.15) predate RHEL9. WMCO10+ branches like
        # release-4.17 shipped via the RHEL8 catalog and are now EOM per the
        # OCP lifecycle API. Use the actual EOM date from the OCP lifecycle
        # API when available rather than the generic "not in current catalog".
        if ocp_minor in old_eol:
            if progress:
                print(f"  {branch_name}: [EOM - skipped]")
            latest_tag = find_latest_tag_for_branch(ocp_minor, all_tags)
            results.append(
                {
                    "branch": branch_name,
                    "ocp_minor": ocp_minor,
                    "pre_release": False,
                    "in_support": False,
                    "support_note": _compute_eom_note(ocp_minor),
                    "version": None,
                    "published_date": None,
                    "freshness_grade": None,
                    "grade_expires": None,
                    "grade_warn": False,
                    "grade_deadline_warn": False,
                    "threshold_date": None,
                    "cve_counts": None,
                    "base_image_data": base_image_data,
                    "security_errata": "",
                    "security_warn": False,
                    "latest_tag": latest_tag,
                    "unreleased": None,
                    "jira_tickets": (jira_tickets or {}).get(ocp_minor, []),
                }
            )
            continue

        data = branch_data[ocp_minor]
        in_support = data.get("in_support", True)

        if not in_support and not include_eol:
            continue

        eom_date = data.get("eom_date")
        grade, grade_expires = get_current_freshness_grade(data.get("freshness_grades", []))

        # Past-EOM catalog branches: include basic catalog data in the output
        # (version, grade, support note) but skip the expensive CVE and PR API
        # calls — that data is not actionable and UNRELEASED filters them out anyway.
        if not in_support:
            if progress:
                print(f"  {branch_name}: [EOM - skipped]")
            latest_tag = find_latest_tag_for_branch(ocp_minor, all_tags)
            results.append(
                {
                    "branch": branch_name,
                    "ocp_minor": ocp_minor,
                    "pre_release": False,
                    "version": data["version"],
                    "published_date": data.get("published_date"),
                    "in_support": False,
                    "support_note": data.get("support_note", "Unknown"),
                    "freshness_grade": grade,
                    "grade_expires": grade_expires,
                    "grade_warn": grade_is_below_b(grade),
                    "grade_deadline_warn": False,
                    "threshold_date": get_threshold_date(data.get("freshness_grades", [])),
                    "cve_counts": None,
                    "base_image_data": base_image_data,
                    "security_errata": data.get("container_grades_msg", ""),
                    "security_warn": False,
                    "latest_tag": latest_tag,
                    "unreleased": None,
                    "jira_tickets": (jira_tickets or {}).get(ocp_minor, []),
                }
            )
            continue

        if progress:
            eom_note = f" [EOM {eom_date}]" if eom_date else ""
            print(f"  {branch_name}{eom_note}: ", end="", flush=True)

        result = {
            "branch": branch_name,
            "ocp_minor": ocp_minor,
            "pre_release": False,
            "version": data["version"],
            "published_date": data.get("published_date"),
            "in_support": True,
            "support_note": data.get("support_note", "Unknown"),
        }

        # Image health
        result["freshness_grade"] = grade
        result["grade_expires"] = grade_expires
        result["grade_warn"] = grade_is_below_b(grade)
        result["threshold_date"] = get_threshold_date(data.get("freshness_grades", []))
        # Flag if grade is currently acceptable but will drop within the sprint window.
        # grade_warn and grade_deadline_warn are mutually exclusive by construction.
        result["grade_deadline_warn"] = (
            not result["grade_warn"]
            and _grade_expires_within_sprint(result["threshold_date"])
        )

        # CVE vulnerabilities from the catalog Security tab
        image_internal_id = data.get("image_internal_id", "")
        if image_internal_id:
            if progress:
                print("CVEs", end="", flush=True)
            result["cve_counts"] = fetch_image_cves(image_internal_id)
            if progress:
                print(".", end="", flush=True)
        else:
            result["cve_counts"] = {
                "critical": 0, "important": 0, "moderate": 0, "low": 0,
                "total": 0, "error": "no image ID",
            }
        result["base_image_data"] = base_image_data

        # Catalog health: unapplied package/layer updates.
        # security_errata / security_warn are not shown in the text report (CVE counts
        # from the /vulnerabilities endpoint are more actionable), but they are included
        # in the result dict so JSON consumers can access raw container_grades status.
        msg = data.get("container_grades_msg", "")
        has_errata = bool(msg and ("Critical" in msg or "Important" in msg))
        result["security_errata"] = msg
        result["security_warn"] = has_errata

        # Unreleased commits — one dot per PR processed (details + file check)
        latest_tag = find_latest_tag_for_branch(ocp_minor, all_tags)
        result["latest_tag"] = latest_tag

        if latest_tag is None:
            result["unreleased"] = None
        else:
            if progress:
                print(" PRs", end="", flush=True)

            def _tick():
                if progress:
                    print(".", end="", flush=True)

            try:
                result["unreleased"] = fetch_unreleased_prs(
                    latest_tag, branch_name, tick=_tick
                )
            except requests.RequestException as exc:
                result["unreleased"] = {
                    "ahead_by": 0, "total_prs": 0, "team_prs": [],
                    "bot_filtered": 0, "non_shipped_filtered": 0,
                    "truncated": False, "error": str(exc),
                }

        if progress:
            print(" done")

        result["jira_tickets"] = (jira_tickets or {}).get(ocp_minor, [])
        results.append(result)

    return results


# ---------------------------------------------------------------------------
# ANSI helpers (auto-disabled when not writing to a terminal)
# ---------------------------------------------------------------------------

_USE_COLOR = sys.stdout.isatty()


def _colored(code: str, s: str) -> str:
    return f"\033[{code}m{s}\033[0m" if _USE_COLOR else s


def _red(s: str) -> str:
    return _colored("31", s)


def _yellow(s: str) -> str:
    return _colored("33", s)


def _green(s: str) -> str:
    return _colored("32", s)


def _bold(s: str) -> str:
    return _colored("1", s)


# ---------------------------------------------------------------------------
# Text report
# ---------------------------------------------------------------------------


def format_text_report(results: list, today_str: str) -> str:
    """Render run_checks() results as a human-readable multi-section text report."""
    lines = []
    lines.append(_bold(f"WMCO Z-Stream Release Check — {today_str}"))
    lines.append("=" * 60)

    # ── Section 1: Release Branches ──────────────────────────────
    lines.append("")
    lines.append(_bold("RELEASE BRANCHES"))
    lines.append(f"{'Branch':<22} {'Last Release':<16} {'Published':<12} {'OCP':<7} Status")
    lines.append("-" * 90)

    for r in results:
        tag = r.get("latest_tag") or "--"
        pub = r.get("published_date") or "--"
        ocp = r.get("ocp_minor", "")
        note = r.get("support_note", "")

        if r.get("pre_release"):
            tag_col = "[PRE-RELEASE]"
            note_str = _yellow(note)
        elif not r.get("in_support"):
            tag_col = tag
            note_str = _yellow(note)
        else:
            tag_col = tag
            note_str = note

        lines.append(f"{r['branch']:<22} {tag_col:<16} {pub:<12} {ocp:<7} {note_str}")
        for ticket in r.get("jira_tickets", []):
            key = ticket["key"]
            version = ticket["version"]
            status = ticket["status"]
            itype = ticket["issuetype"]
            status_str = _yellow(status) if status == "In Progress" else status
            lines.append(f"  {'':>20}↳ {_bold(key)} v{version} ({itype}) — {status_str}")

    # ── Section 2: Image Health ───────────────────────────────────
    # Threshold Date: the first date the image reaches any grade below B (C, D, E, or F).
    # For A/B images: deadline for a new release to maintain acceptable grade.
    # For C/D/E/F images: when the image first crossed the threshold (may be a D/E/F
    # date if the freshness_grades time-series skips C entirely).
    # CVEs: counts from the catalog Security tab (C=Critical I=Important M=Moderate L=Low).
    # Base: whether rebuilding from the current ubi9/ubi-minimal base image extends the
    #       threshold date (the date the grade first drops below B).
    #   ext ✓ = base threshold is later (or clean) — rebuild extends the grade window
    #   same  = base threshold equals WMCO threshold — rebuild does not extend window
    #   ↓     = base threshold is earlier — rebuild may shorten the grade window
    lines.append("")
    lines.append(_bold("IMAGE HEALTH (Red Hat Container Catalog)"))
    header = f"{'Version':<14} {'Grade':<8} {'Threshold Date':<16} {'CVEs':<14} {'Base':<10} Status"
    lines.append(header)
    lines.append("-" * 72)

    for r in results:
        if r.get("pre_release") or not r.get("in_support"):
            continue

        version = r.get("version", "")
        grade = r.get("freshness_grade") or "?"
        # For images already below B the catalog API sets start_date to today rather
        # than the actual historical crossing date, so the value is not meaningful.
        td_raw = "--" if r.get("grade_warn") else (r.get("threshold_date") or "--")
        cve_str = _format_cve_counts(r.get("cve_counts"))

        # Base image rebuild label — ANSI-safe padding (color applied before spaces).
        base_label = _base_image_rebuild_label(
            r.get("threshold_date"), r.get("base_image_data")
        )
        base_pad = " " * (10 - len(base_label))
        if base_label == "ext ✓":
            base_col = _green(base_label) + base_pad
        else:
            base_col = base_label + base_pad

        # Threshold Date column: yellow when grade will expire within the sprint window.
        # ANSI-safe: pad using the raw string length, then apply color codes.
        td_pad = " " * (16 - len(td_raw))
        if r.get("grade_deadline_warn"):
            td_col = _yellow(td_raw) + td_pad
        else:
            td_col = td_raw + td_pad

        # Pad grade outside ANSI codes so terminal column width is correct.
        if r.get("grade_warn"):
            grade_col = _red(grade) + " " * (8 - len(grade))
            status_str = _red("✗")
        elif r.get("grade_deadline_warn"):
            grade_col = _green(grade) + " " * (8 - len(grade))
            status_str = _yellow("⚠")
        else:
            grade_col = _green(grade) + " " * (8 - len(grade))
            status_str = _green("✓")

        row = f"v{version:<13} {grade_col} {td_col} {cve_str:<14} {base_col} {status_str}"
        lines.append(row)

    # ── Section 3: Unreleased Pull Requests ──────────────────────
    lines.append("")
    lines.append(_bold("UNRELEASED PULL REQUESTS"))
    lines.append("-" * 60)

    for r in results:
        branch = r.get("branch", "")
        tag = r.get("latest_tag")

        if r.get("pre_release"):
            tag = r.get("latest_tag")
            unreleased = r.get("unreleased")
            since = f" (since {tag})" if tag else ""
            if not unreleased:
                if tag:
                    lines.append(
                        f"{branch}{since}: [pre-release]"
                        " — use --pre-release-prs to show unreleased PRs"
                    )
                else:
                    lines.append(f"{branch}: [pre-release] — no base tag found")
            elif unreleased.get("error"):
                lines.append(
                    f"{branch}{since}: [pre-release] ERROR — {unreleased['error']}"
                )
            else:
                team_prs = unreleased.get("team_prs", [])
                bot_filtered = unreleased.get("bot_filtered", 0)
                non_shipped = unreleased.get("non_shipped_filtered", 0)
                action_prs = [pr for pr in team_prs if not pr.get("is_version_bump")]
                info_prs = [pr for pr in team_prs if pr.get("is_version_bump")]
                filter_parts = []
                if bot_filtered:
                    filter_parts.append(
                        f"{bot_filtered} bot bump{'s' if bot_filtered != 1 else ''}"
                    )
                if non_shipped:
                    filter_parts.append(f"{non_shipped} non-shipped")
                bot_note = (
                    f"  ({', '.join(filter_parts)} filtered)" if filter_parts else ""
                )
                count = len(action_prs)
                if count == 0:
                    lines.append(
                        f"{branch}{since}: "
                        f"{_green('no team PRs  ✓')}{bot_note} [pre-release]"
                    )
                else:
                    plural = "s" if count != 1 else ""
                    lines.append(
                        f"{branch}{since}: "
                        f"{_yellow(f'{count} team PR{plural}  ⚠')}{bot_note} [pre-release]"
                    )
                for pr in action_prs:
                    lines.append(f"  PR #{pr['pr_number']}  {pr['title']}")
                for pr in info_prs:
                    lines.append(f"  PR #{pr['pr_number']}  [INFO] {pr['title']}")
            continue

        # Skip EOL branches before checking unreleased data — old_eol branches
        # have unreleased=None (we don't fetch PR data for them), which would
        # otherwise trigger the misleading "no tag found" message below.
        if not r.get("in_support"):
            continue

        unreleased = r.get("unreleased")
        if not unreleased:
            lines.append(f"{branch}: no tag found — skipped")
            continue

        if unreleased.get("error"):
            lines.append(f"{branch} (since {tag}): ERROR — {unreleased['error']}")
            continue

        team_prs = unreleased.get("team_prs", [])
        bot_filtered = unreleased.get("bot_filtered", 0)
        non_shipped = unreleased.get("non_shipped_filtered", 0)
        truncated = unreleased.get("truncated", False)

        action_prs = [pr for pr in team_prs if not pr.get("is_version_bump")]
        info_prs = [pr for pr in team_prs if pr.get("is_version_bump")]
        action_count = len(action_prs)

        filter_parts = []
        if bot_filtered:
            filter_parts.append(f"{bot_filtered} bot bump{'s' if bot_filtered != 1 else ''}")
        if non_shipped:
            filter_parts.append(f"{non_shipped} non-shipped")
        bot_note = f"  ({', '.join(filter_parts)} filtered)" if filter_parts else ""

        if action_count == 0:
            # Zero action PRs — clean (version-bump-only PRs don't trigger a release)
            lines.append(f"{branch} (since {tag}): {_green('no team PRs  ✓')}{bot_note}")
        else:
            plural = "s" if action_count != 1 else ""
            lines.append(
                f"{branch} (since {tag}): {_yellow(f'{action_count} team PR{plural}  ⚠')}{bot_note}"
            )

        for pr in action_prs:
            lines.append(f"  PR #{pr['pr_number']}  {pr['title']}")

        for pr in info_prs:
            lines.append(f"  PR #{pr['pr_number']}  [INFO] {pr['title']}")
        if truncated:
            lines.append(
                f"  ⚠ {unreleased['ahead_by']} total commits exceeds limit"
                " — older PRs may be missing"
            )

    # ── Section 4: Sprint Recommendation ─────────────────────────
    # Grouped by branch: all reasons a release is needed are shown together.
    lines.append("")
    lines.append(_bold("SPRINT RECOMMENDATION"))
    lines.append("-" * 60)

    action_branches = []   # list of (result, action_prs, info_prs)
    clear_branch_names = []

    for r in results:
        if r.get("pre_release") or not r.get("in_support"):
            continue
        u = r.get("unreleased") or {}
        team_prs = u.get("team_prs", [])
        action_prs = [pr for pr in team_prs if not pr.get("is_version_bump")]
        info_prs = [pr for pr in team_prs if pr.get("is_version_bump")]
        if action_prs or r.get("grade_warn") or r.get("grade_deadline_warn") or u.get("error"):
            action_branches.append((r, action_prs, info_prs))
        else:
            clear_branch_names.append(r["branch"])

    if not action_branches:
        lines.append(_green("✓ No z-stream releases needed. All images healthy."))
    else:
        for r, action_prs, info_prs in action_branches:
            branch = r["branch"]
            tag = r.get("latest_tag") or "--"
            u = r.get("unreleased") or {}
            cve = r.get("cve_counts") or {}

            lines.append("")
            lines.append(_bold(branch))

            # Unreleased PRs
            if action_prs:
                bot_filtered = u.get("bot_filtered", 0)
                non_shipped = u.get("non_shipped_filtered", 0)
                filter_parts = []
                if bot_filtered:
                    filter_parts.append(f"{bot_filtered} bot")
                if non_shipped:
                    filter_parts.append(f"{non_shipped} non-shipped")
                bot_note = f"  ({', '.join(filter_parts)} filtered)" if filter_parts else ""
                jiras = [pr["jira"] for pr in action_prs if pr.get("jira")]
                jira_str = f"  [{', '.join(jiras)}]" if jiras else ""
                count = len(action_prs)
                plural = "s" if count != 1 else ""
                lines.append(
                    f"  {_red('✗')} Unreleased PRs: {count} since {tag}{bot_note}{jira_str}"
                )
                for pr in action_prs:
                    lines.append(f"      PR #{pr['pr_number']}  {pr['title']}")
                    lines.append(f"             @{pr.get('author', '')}  {pr.get('merged_at', '')}")
                if u.get("truncated"):
                    lines.append(
                        f"      ⚠ {u['ahead_by']} total commits exceeds limit"
                        " — older PRs may be missing"
                    )

            # Image health — current grade below B
            if r.get("grade_warn"):
                grade = r.get("freshness_grade", "?")
                lines.append(_red(f"  ✗ Image health: Grade {grade} — below threshold"))

            # Image health — grade acceptable now but will drop within sprint window
            if r.get("grade_deadline_warn"):
                grade = r.get("freshness_grade", "?")
                threshold = r.get("threshold_date") or "--"
                try:
                    days_left = (
                        datetime.strptime(threshold, "%Y-%m-%d").date() - date.today()
                    ).days
                    days_str = f" ({days_left} day{'s' if days_left != 1 else ''})"
                except ValueError:
                    days_str = ""
                lines.append(_yellow(
                    f"  ⚠ Image grade: Currently {grade}, will drop below B "
                    f"on {threshold}{days_str} — start release now"
                ))

            # Base image rebuild note — shown when grade is the trigger
            if r.get("grade_warn") or r.get("grade_deadline_warn"):
                base_rebuild = _base_image_rebuild_label(
                    r.get("threshold_date"), r.get("base_image_data")
                )
                if base_rebuild == "ext ✓":
                    msg = "↑ base image extends grade window — rebuild improves threshold date"
                    lines.append(f"    {_green(msg)}")
                elif base_rebuild == "same":
                    lines.append(
                        "    ↑ base image: same threshold — rebuild does not extend grade window"
                    )
                elif base_rebuild == "↓":
                    lines.append(
                        "    ↑ base image threshold is earlier — rebuild may not extend window"
                    )

            # CVEs — informational context only, not a release trigger
            if _has_actionable_cves(cve):
                cve_parts = []
                if cve.get("critical"):
                    cve_parts.append(_red(f"{cve['critical']} Critical"))
                if cve.get("important"):
                    cve_parts.append(_yellow(f"{cve['important']} Important"))
                if cve.get("moderate"):
                    cve_parts.append(f"{cve['moderate']} Moderate")
                if cve.get("low"):
                    cve_parts.append(f"{cve['low']} Low")
                lines.append(f"  ℹ CVEs (info): {', '.join(cve_parts)}")

            # Jira release tracking
            tickets = r.get("jira_tickets", [])
            if tickets:
                for ticket in tickets:
                    status_str = (
                        _yellow(ticket["status"]) if ticket["status"] == "In Progress"
                        else ticket["status"]
                    )
                    lines.append(
                        f"  → {_bold(ticket['key'])} v{ticket['version']}"
                        f" ({ticket['issuetype']}) — {status_str}"
                    )
            else:
                # Mention any version-bump PR as a signal that release prep has started
                hint = ""
                if info_prs:
                    m = re.search(r"\d+\.\d+\.\d+", info_prs[0]["title"])
                    if m:
                        hint = f"  (PR #{info_prs[0]['pr_number']} bumped version to {m.group(0)})"
                lines.append(f"  → No open release ticket{hint}")

    if clear_branch_names:
        lines.append("")
        lines.append(_green(f"✓ No action needed: {', '.join(clear_branch_names)}"))

    lines.append("")
    action_needed = bool(action_branches)
    status_label = "action required" if action_needed else "all clear"
    lines.append(f"Status: {status_label}")

    return "\n".join(lines)


# ---------------------------------------------------------------------------
# JSON report
# ---------------------------------------------------------------------------


def format_json_report(results: list, today_str: str) -> str:
    """Render run_checks() results as machine-readable JSON."""
    action_needed = _is_action_needed(results)
    return json.dumps(
        {"date": today_str, "action_required": action_needed, "branches": results},
        indent=2,
        default=str,
    )


# ---------------------------------------------------------------------------
# Connectivity check
# ---------------------------------------------------------------------------


def check_connectivity(quiet: bool = False) -> bool:
    """Probe all required external APIs and print reachability status. Returns False on failure.

    When quiet=True, all stdout output is suppressed (used with --json to keep stdout
    free of non-JSON content). Failure messages are always written to stderr.
    """
    failures = []

    def probe(label: str, url: str, required: bool = True, **kwargs):
        try:
            resp = _get(url, timeout=8, allow_redirects=True, **kwargs)
            reachable = resp.status_code < 500
        except requests.RequestException as exc:
            if not quiet:
                print(f"  [FAIL] {label}: {exc}")
            if required:
                failures.append(label)
            return
        if not quiet:
            status = "[OK]  " if reachable else "[FAIL]"
            print(f"  {status} {label}" + ("" if reachable else f": HTTP {resp.status_code}"))
        if not reachable and required:
            failures.append(label)

    jira_auth = _jira_auth()

    if not quiet:
        print("Connectivity check")
        print("-" * 30)
    probe("Red Hat Container Catalog", "https://catalog.redhat.com/api/containers/v1/")
    probe("Red Hat OCP Lifecycle API", OCP_LIFECYCLE_API)
    probe("GitHub API", GITHUB_API, headers=_github_headers())
    if jira_auth:
        # Use serverInfo (GET, no auth required) to test reachability; auth validity
        # is implicitly verified when fetch_jira_release_tickets() runs later.
        probe("Jira (WINC project)", _JIRA_SERVER_INFO_URL, required=False)
    elif not quiet:
        print("  [SKIP] Jira — set JIRA_API_TOKEN and JIRA_USERNAME to enable release tracking")
    if not quiet:
        print()

    if failures:
        print(
            f"ERROR: Cannot reach required service(s): {', '.join(failures)}",
            file=sys.stderr,
        )
        return False
    return True


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    """Parse CLI arguments, run all checks, and print the release report."""
    parser = argparse.ArgumentParser(
        description="Check which WMCO release branches need a z-stream release.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""\
Examples:
  python3 hack/z-stream-release-check.py                        # in-support branches only
  python3 hack/z-stream-release-check.py --all                  # include past-EOM branches
  python3 hack/z-stream-release-check.py --branch release-4.18  # single branch
  python3 hack/z-stream-release-check.py --json                 # machine-readable output
  python3 hack/z-stream-release-check.py --connectivity         # test connectivity only
""",
    )
    parser.add_argument(
        "--all", "-a", action="store_true",
        help="Include past-EOM branches (default: in-support only)"
    )
    parser.add_argument(
        "--json", action="store_true", dest="json_output", help="Output machine-readable JSON"
    )
    parser.add_argument(
        "--branch",
        metavar="BRANCH",
        help="Check only this branch (e.g. release-4.18)",
    )
    parser.add_argument(
        "--cutoff-months",
        type=int,
        default=18,
        metavar="N",
        help="Ignored (reserved for compatibility; EOM dates come from the OCP lifecycle API)",
    )
    parser.add_argument(
        "--pre-release-prs", action="store_true", dest="pre_release_prs",
        help="Fetch unreleased PRs for pre-release branches (skipped by default)",
    )
    parser.add_argument(
        "--connectivity", action="store_true", help="Test connectivity and exit"
    )
    args = parser.parse_args()

    if args.connectivity:
        sys.exit(0 if check_connectivity() else 2)

    if not check_connectivity(quiet=args.json_output):
        sys.exit(2)

    today_str = date.today().isoformat()
    show_progress = not args.json_output

    def _prog(msg):
        """Print a progress message (suppressed for --json to keep stdout clean)."""
        if show_progress:
            print(msg, flush=True)

    def _prog_start(msg):
        """Print start of a progress line without newline."""
        if show_progress:
            print(msg, end="", flush=True)

    def _prog_end(msg):
        """Complete the current progress line."""
        if show_progress:
            print(msg, flush=True)

    _prog_start("Fetching WMCO image list from Red Hat Container Catalog...")
    try:
        all_catalog_versions = fetch_catalog_versions()
    except requests.RequestException as exc:
        print(f"\nERROR: Failed to fetch catalog: {exc}", file=sys.stderr)
        sys.exit(2)
    _prog_end(f" done ({len(all_catalog_versions)} images)")

    if not all_catalog_versions:
        print("ERROR: No WMCO images found in catalog.", file=sys.stderr)
        sys.exit(2)

    latest_by_branch = get_latest_version_per_branch(all_catalog_versions)

    _prog_start("Fetching OCP lifecycle (EOM dates) from Red Hat API...")
    branch_data = annotate_support_status(latest_by_branch)
    _prog_end(" done")

    _prog_start("Fetching GitHub release branches...")
    try:
        all_github_branches = fetch_github_release_branches()
        _prog_end(f" done ({len(all_github_branches)} branches)")
    except requests.RequestException as exc:
        print(f"\nWARNING: Could not fetch GitHub branches: {exc}", file=sys.stderr)
        all_github_branches = []

    _prog_start("Fetching GitHub tags...")
    try:
        all_tags = fetch_github_tags()
        _prog_end(f" done ({len(all_tags)} tags)")
    except requests.RequestException as exc:
        print(f"\nERROR: Failed to fetch GitHub tags: {exc}", file=sys.stderr)
        sys.exit(2)

    _prog_start("Fetching Jira release tickets...")
    jira_release_tickets = fetch_jira_release_tickets()
    if jira_release_tickets is None:
        _prog_end(" skipped (set JIRA_API_TOKEN and JIRA_USERNAME to enable)")
    else:
        ticket_count = sum(len(v) for v in jira_release_tickets.values())
        _prog_end(f" done ({ticket_count} open ticket{'s' if ticket_count != 1 else ''})")

    _prog_start("Fetching base image (ubi9/ubi-minimal) grade data...")
    base_image_data = fetch_base_image_data()
    if base_image_data and not base_image_data.get("error"):
        base_grade = base_image_data.get("grade", "?")
        base_td = base_image_data.get("threshold_date") or "none"
        _prog_end(f" done (grade {base_grade}, threshold {base_td})")
    else:
        _prog_end(" failed (base column will show '?')")

    _prog("Checking release branches...")
    results = run_checks(
        branch_data,
        all_tags,
        all_github_branches,
        include_eol=args.all,
        filter_branch=args.branch,
        jira_tickets=jira_release_tickets,
        base_image_data=base_image_data,
        progress=show_progress,
        pre_release_prs=args.pre_release_prs,
    )

    if not results:
        print(
            "No branches matched. Use --all to include past-EOM branches.",
            file=sys.stderr,
        )
        sys.exit(0)

    if show_progress:
        print()  # blank line between progress block and report

    report = (
        format_json_report(results, today_str)
        if args.json_output
        else format_text_report(results, today_str)
    )
    print(report)

    sys.exit(0)


if __name__ == "__main__":
    main()
