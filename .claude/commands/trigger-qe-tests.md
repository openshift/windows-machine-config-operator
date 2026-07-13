---
description: Create a DEBUG PR to trigger WMCO QE z-stream tests via /pj-rehearse
args: "[version]"
allowed-tools: Read, Edit, Bash(grep *), Bash(ls *), Bash(git fetch *), Bash(git worktree *), Bash(git add *), Bash(git commit *), Bash(git push *), Bash(git diff *), Bash(git status *), Bash(gh pr *), Bash(make update), Bash(sed *), Grep, Glob, AskUserQuestion
---

# Trigger WMCO QE Z-Stream Tests

Trigger Windows Container (winc) QE tests for a specific OpenShift release by creating a temporary PR in `openshift/release` that renames periodic job names, making Prow treat them as "new" jobs eligible for `/pj-rehearse`.

**Arguments**: version={{version}}

## How It Works

Periodic jobs cannot be triggered on demand. By renaming the `as:` field (e.g., `winc-f14` to `winc-zstream-f14`), Prow sees a "new" job and allows `/pj-rehearse` to run it. The PR is never merged -- it exists solely to trigger the tests.

## Version Handling

The version argument is required and must be an explicit release number (e.g., `5.0`, `4.21`, `4.20`, `10.21`, `11.0`).
If no version is provided, ask the user for one. Do not guess or resolve from branch names.

### Input Validation

Before proceeding, validate the version matches one of these patterns:
- `4.NN` where NN is a number (e.g., `4.18`, `4.21`, `4.22`)
- `5.N` where N is a number (e.g., `5.0`, `5.1`)
- `10.NN` where NN is a number (e.g., `10.20`, `10.21`)
- `11.N` where N is a number (e.g., `11.0`, `11.1`)

Reject any input that does not match these patterns and ask the user for a valid version.

### Version Mapping

The user may pass either the OCP version or the WMCO version. The mapping is:
- WMCO `10.xx` = OCP `4.xx` (e.g., WMCO 10.20 = OCP 4.20)
- WMCO `11.x` = OCP `5.x` (e.g., WMCO 11.0 = OCP 5.0)

Normalize input to derive both versions:
- If user passes `10.xx` -> RELEASE=`4.xx`, WMCO_VERSION=`10.xx`
- If user passes `11.x` -> RELEASE=`5.x`, WMCO_VERSION=`11.x`
- If user passes `4.xx` -> RELEASE=`4.xx`, WMCO_VERSION=`10.xx`
- If user passes `5.x` -> RELEASE=`5.x`, WMCO_VERSION=`11.x`

Use `RELEASE` (OCP version) for config file paths and branch names.
Use `WMCO_VERSION` in the PR title and body.

## Locate the openshift/release repo

This skill assumes it is run from a local clone of `openshift/release`. Verify:
```bash
git remote get-url upstream 2>/dev/null | grep -q "openshift/release"
```
If the current directory is not the release repo, ask the user for the path.

Store the resolved path as `RELEASE_REPO`. All paths below are relative to that root:
- Config dir: `ci-operator/config/openshift/openshift-tests-private/`
- Jobs dir: `ci-operator/jobs/openshift/openshift-tests-private/`

## Workflow

### Step 1: Validate

1. Locate the config file in the release repo:
   ```bash
   {RELEASE_REPO}/ci-operator/config/openshift/openshift-tests-private/openshift-openshift-tests-private-release-{RELEASE}__amd64-nightly.yaml
   ```
2. Confirm it exists and contains `winc` jobs:
   ```bash
   grep 'as:.*winc.*f[0-9]' <config-file>
   ```
3. List the winc jobs found and show them to the user.

### Step 2: Ask which jobs to trigger

Present the list of winc jobs and ask the user which ones to rename. Options:
- **All winc jobs** (default -- what PR #81646 did for 4.21)
- **Specific jobs** (user picks from the list)

The standard set typically includes: aws, gcp, azure, vsphere, nutanix variants.

### Step 3: Create the branch and make changes

1. Verify the release repo state before proceeding:
   ```bash
   cd {RELEASE_REPO}
   git remote get-url upstream  # must be openshift/release
   ```
   If the branch `winc-zstream-{RELEASE}` or worktree already exists, abort and ask the user.
2. Fetch upstream and create a worktree:
   ```bash
   git fetch upstream
   git worktree add ../worktrees/winc-zstream-{RELEASE} -b winc-zstream-{RELEASE} upstream/main
   ```
2. In the worktree config file, for each selected job, rename the `as:` field:
   - Pattern: `winc-fNN` -> `winc-zstream-fNN`
   - Example: `aws-ipi-ovn-winc-f14` -> `aws-ipi-ovn-winc-zstream-f14`
   - Only rename jobs that do NOT already contain `zstream`
3. Run `make update` to regenerate the Prow job files (ask user to run this).
4. Verify `make update` succeeded and the generated jobs file contains the expected `winc-zstream` renames. Abort if regeneration failed or renames are missing.
5. Show the diff for review.

### Step 4: Commit and create PR

Show the diff and ask the user for explicit confirmation before committing, pushing, or creating the PR.
```bash
cd {RELEASE_REPO}/../worktrees/winc-zstream-{RELEASE}
git add ci-operator/config/openshift/openshift-tests-private/ ci-operator/jobs/openshift/openshift-tests-private/
git commit -m "DEBUG Do not merge: {WMCO_VERSION} Z stream triggered jobs"
git push -u origin winc-zstream-{RELEASE}
gh pr create --repo openshift/release \
  --title "DEBUG Do not merge: {WMCO_VERSION} Z stream triggered jobs" \
  --body "This PR modifies periodic job names so they will trigger by /pj-rehearse new jobs with latest {WMCO_VERSION} image"
```

### Step 5: Trigger rehearsals

1. Poll the PR for the REHEARSALNOTIFIER bot comment every 60 seconds, up to 10 minutes. If the comment does not appear within 10 minutes, report the timeout and stop.
2. Verify the comment author is the rehearsal bot and the job list contains the expected `winc-zstream` job names.
3. Ask the user for confirmation before posting the rehearse command.
4. Once confirmed, post:
   ```bash
   /pj-rehearse
   ```
   This triggers up to 5 rehearsals. If more than 5 jobs, use `/pj-rehearse max`.
5. Report the PR URL and triggered job names.

## Important Notes

- This PR must NEVER be merged -- it is only for triggering tests.
- The PR must NOT be a draft -- `/pj-rehearse` does not run on draft PRs.
- The `as:` rename is the only change needed in the config file. Everything else (cron, steps, env) stays the same.
- The generated jobs file (`ci-operator/jobs/`) changes automatically from `make update`.
- After tests complete, close the PR without merging.
- If the branch already exists, ask the user before overwriting.

## Files Modified (in openshift/release repo)

- `ci-operator/config/openshift/openshift-tests-private/openshift-openshift-tests-private-release-{RELEASE}__amd64-nightly.yaml` (source of truth)
- `ci-operator/jobs/openshift/openshift-tests-private/openshift-openshift-tests-private-release-{RELEASE}-periodics.yaml` (auto-generated by `make update`)

## Reference

- PR #81646 is the template for this process (4.21 z-stream test trigger).
