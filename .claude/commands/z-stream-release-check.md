Run the WMCO z-stream release check to determine which release branches need a z-stream
release before sprint planning.

## Steps

1. Run the check script:

```bash
python3 hack/z-stream-release-check.py
```

2. Interpret and present the output to the user with a clear summary covering:

   - **Release branches**: Which OCP versions are active vs. EOL, and which is pre-release.
     If Jira is configured, open release tickets are shown under each branch.
   - **Image health**: Flag any branch whose catalog freshness grade is below B (action needed).
     The "Threshold Date" column shows the deadline (for healthy images) or when the image first
     crossed the threshold (for degraded images; may show a D/F date if the image skipped grade C
     entirely). CVE counts are informational only — the freshness grade already encodes CVE
     timeliness.
   - **Unreleased PRs**: For each active branch, how many team PRs have merged since the last
     release tag. Bot bump PRs (Konflux, Renovate, mintmaker, dependabot) are filtered out.
     Version-bump PRs from pre-release.sh are shown as [INFO] — they indicate release prep
     has started but do not themselves require a release.
   - **Sprint recommendation**: Grouped by branch — each branch that needs attention shows all
     reasons together (unreleased PRs with Jira keys, image grade, CVE counts, open Jira tickets).

3. If the user requests more detail on a specific branch, re-run with:

```bash
python3 hack/z-stream-release-check.py --branch <branch-name>
```

## Useful options

| Flag | Purpose |
|------|---------|
| `--all` | Include EOL branches (default: in-support only) |
| `--branch release-4.X` | Check a single branch |
| `--json` | Machine-readable output for scripting |
| `--pre-release-prs` | Fetch unreleased PRs for pre-release branches (skipped by default) |
| `--connectivity` | Test API connectivity only |
| `--cutoff-months N` | Ignored (reserved for compatibility; EOM dates come from OCP lifecycle API) |

Set environment variables to unlock optional features:

| Variable | Purpose |
|----------|---------|
| `GITHUB_TOKEN` | Avoid GitHub API rate limits |
| `JIRA_API_TOKEN` | Enable Jira release ticket tracking |
| `JIRA_USERNAME` | Atlassian account email (required with `JIRA_API_TOKEN`) |

## What triggers a release recommendation

A branch is flagged in the Sprint Recommendation when **any** of the following are true:
- Team PRs have merged since the last release tag (excluding bot bumps, non-shipped PRs,
  and version-bump PRs)
- Catalog freshness grade is below B (C, D, E, or F)
- Grade is currently A or B but will drop below B within 21 days (sprint lookahead)

CVE counts are **informational only** and do not trigger a recommendation — the freshness
grade already encodes CVE timeliness (a Critical fix older than 7 days drops the grade to
C or below, which is itself the trigger).

## Context: where this fits in the release workflow

| Tool | When to use |
|------|------------|
| `/z-stream-release-check` (this) | Before sprint planning — decide IF a release is needed |
| `hack/pre-release.sh` | During the sprint — version bump and branch prep |
| `hack/create-release-tag.py` | After the image ships — tag the release commit |
| `hack/verify-release.py` | After the tag — verify release quality against the catalog |
