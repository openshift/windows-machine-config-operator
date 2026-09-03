#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS] <WMCO_VERSION> [RELEASE_REPO_PATH]

Trigger WMCO QE tests by creating a temporary PR in openshift/release
that renames winc periodic job names, making them eligible for /pj-rehearse.

Arguments:
  WMCO_VERSION       WMCO version (e.g., 10.20, 10.21, 11.0)
  RELEASE_REPO_PATH  Path to local openshift/release clone (optional).
                     Falls back to RELEASE_REPO env var.

Options:
  -s, --stream TYPE  Stream type: 'y' (all jobs) or 'z' (subset). Default: z
  -j, --jobs JOBS    Comma-separated list of job names to include.
                     Overrides the default job selection for the stream type.
  -x, --exclude JOBS Comma-separated list of job names to exclude.
  -h, --help         Show this help message.

An interactive menu is shown before proceeding, allowing you to toggle
individual jobs on/off regardless of the -j/-x flags or stream defaults.

Stream types:
  z-stream (default): Triggers a subset of winc jobs (aws, gcp, vsphere).
                      Used for patch release validation (e.g., 4.21.1).
  y-stream:           Triggers ALL winc jobs (aws, gcp, azure, vsphere,
                      vsphere-disconnected, nutanix, etc.).
                      Used for minor release validation (e.g., 4.21, 4.22).

Examples:
  $(basename "$0") 10.20 ~/git/release                     # z-stream, default jobs
  $(basename "$0") -s y 10.21 ~/git/release                 # y-stream, all jobs
  $(basename "$0") -j aws-ipi-ovn-winc-f14 10.20            # only specific job
  $(basename "$0") -s y -x nutanix-ipi-ovn-winc-f14 10.21   # y-stream minus nutanix
  RELEASE_REPO=~/src/release $(basename "$0") 11.0
EOF
  exit 1
}

# Default z-stream job patterns (platforms triggered for patch releases)
ZSTREAM_PATTERNS="aws-ipi-ovn-winc|gcp-ipi-ovn-winc|vsphere-ipi-ovn-winc"

validate_version() {
  local ver="$1"
  if ! echo "$ver" | grep -qE '^(10\.[0-9]+|11\.[0-9]+)$'; then
    error-exit "Invalid WMCO version: $ver (expected format: 10.NN or 11.N)"
  fi
}

RELEASE_REMOTE=""
validate_release_repo() {
  local repo_path="$1"
  if [[ ! -d "$repo_path/.git" ]]; then
    error-exit "$repo_path is not a git repository"
  fi
  local remote_url
  for remote in upstream origin; do
    remote_url=$(git -C "$repo_path" remote get-url "$remote" 2>/dev/null || true)
    if echo "$remote_url" | grep -qE 'openshift/release(\.git)?$'; then
      RELEASE_REMOTE="$remote"
      return
    fi
  done
  error-exit "$repo_path does not point to openshift/release (got: $remote_url)"
}

cleanup() {
  local exit_code=$?
  if [[ $exit_code -ne 0 ]]; then
    if [[ -n "${WORKTREE_DIR:-}" && -d "$WORKTREE_DIR" ]]; then
      echo "Cleaning up worktree: $WORKTREE_DIR"
      git -C "${RELEASE_REPO:-}" worktree remove "$WORKTREE_DIR" --force 2>/dev/null || rm -rf "$WORKTREE_DIR"
    fi
    if [[ -n "${BRANCH:-}" && -n "${RELEASE_REPO:-}" ]]; then
      git -C "$RELEASE_REPO" branch -D "$BRANCH" 2>/dev/null || true
    fi
  fi
}

# --- Parse options ---
STREAM_TYPE="z"
INCLUDE_JOBS=""
EXCLUDE_JOBS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -s|--stream)
      STREAM_TYPE="$2"
      if [[ "$STREAM_TYPE" != "y" && "$STREAM_TYPE" != "z" ]]; then
        error-exit "Invalid stream type: $STREAM_TYPE (expected 'y' or 'z')"
      fi
      shift 2
      ;;
    -j|--jobs)
      INCLUDE_JOBS="$2"
      shift 2
      ;;
    -x|--exclude)
      EXCLUDE_JOBS="$2"
      shift 2
      ;;
    -h|--help)
      usage
      ;;
    -*)
      error-exit "Unknown option: $1"
      ;;
    *)
      break
      ;;
  esac
done

if [[ $# -lt 1 ]]; then
  usage
fi

WMCO_VER="$1"
validate_version "$WMCO_VER"

OCP_VER=$(get_OCP_version "$WMCO_VER")
echo "WMCO version: $WMCO_VER -> OCP version: $OCP_VER"
echo "Stream type: ${STREAM_TYPE}-stream"

# --- Locate the release repo ---
RELEASE_REPO="${2:-${RELEASE_REPO:-}}"
WORKTREE_DIR=""
BRANCH=""
trap cleanup EXIT

if [[ -z "$RELEASE_REPO" ]]; then
  echo "Error: No openshift/release repo path provided."
  echo ""
  echo "Either:"
  echo "  1. Pass it as the second argument:  $(basename "$0") $WMCO_VER /path/to/release"
  echo "  2. Set the RELEASE_REPO env var:    export RELEASE_REPO=/path/to/release"
  echo ""
  echo "To clone it:  git clone https://github.com/openshift/release.git"
  exit 1
fi

validate_release_repo "$RELEASE_REPO"

# --- Locate config file ---
CONFIG_DIR="ci-operator/config/openshift/openshift-tests-private"
CONFIG_FILE="${RELEASE_REPO}/${CONFIG_DIR}/openshift-openshift-tests-private-release-${OCP_VER}__amd64-nightly.yaml"

if [[ ! -f "$CONFIG_FILE" ]]; then
  error-exit "Config file not found: $CONFIG_FILE"
fi

# --- Find winc jobs ---
ALL_WINC_JOBS=$(grep -E '^- as: .+winc.*-f[0-9]+$' "$CONFIG_FILE" | sed 's/^- as: //' || true)
if [[ -z "$ALL_WINC_JOBS" ]]; then
  error-exit "No winc periodic jobs found in $CONFIG_FILE"
fi

echo ""
echo "All winc jobs in config:"
echo "$ALL_WINC_JOBS" | while read -r job; do echo "  $job"; done

# --- Select jobs based on stream type and user overrides ---
SELECTED_JOBS=""

if [[ -n "$INCLUDE_JOBS" ]]; then
  # User explicitly specified jobs
  IFS=',' read -ra INCLUDE_LIST <<< "$INCLUDE_JOBS"
  for job in "${INCLUDE_LIST[@]}"; do
    job=$(echo "$job" | xargs) # trim whitespace
    if echo "$ALL_WINC_JOBS" | grep -qx "$job"; then
      SELECTED_JOBS="${SELECTED_JOBS}${job}\n"
    else
      echo "Warning: job '$job' not found in config, skipping"
    fi
  done
elif [[ "$STREAM_TYPE" == "y" ]]; then
  # Y-stream: all winc jobs
  SELECTED_JOBS=$(echo "$ALL_WINC_JOBS" | while read -r job; do echo "${job}\n"; done)
  SELECTED_JOBS=$(echo -e "$SELECTED_JOBS")
else
  # Z-stream: subset matching known platforms
  SELECTED_JOBS=$(echo "$ALL_WINC_JOBS" | grep -E "$ZSTREAM_PATTERNS" || true)
fi

# Apply exclusions
if [[ -n "$EXCLUDE_JOBS" ]]; then
  IFS=',' read -ra EXCLUDE_LIST <<< "$EXCLUDE_JOBS"
  for excl in "${EXCLUDE_LIST[@]}"; do
    excl=$(echo "$excl" | xargs)
    SELECTED_JOBS=$(echo -e "$SELECTED_JOBS" | grep -v "^${excl}$" || true)
  done
fi

# --- Build job arrays for interactive selection ---
declare -a ALL_JOBS_ARR=()
declare -a SELECTED_ARR=()
declare -a SKIPPED_ARR=()

while IFS= read -r job; do
  [[ -z "$job" ]] && continue
  if echo "$job" | grep -q "zstream"; then
    SKIPPED_ARR+=("$job")
  else
    ALL_JOBS_ARR+=("$job")
    if echo -e "$SELECTED_JOBS" | grep -qx "$job"; then
      SELECTED_ARR+=(1)
    else
      SELECTED_ARR+=(0)
    fi
  fi
done <<< "$ALL_WINC_JOBS"

if [[ ${#ALL_JOBS_ARR[@]} -eq 0 ]]; then
  error-exit "No jobs to rename. All winc jobs already contain 'zstream' or none matched."
fi

if [[ ${#SKIPPED_ARR[@]} -gt 0 ]]; then
  echo ""
  echo "Skipping (already renamed):"
  for s in "${SKIPPED_ARR[@]}"; do echo "  $s"; done
fi

RENAME_LABEL="${STREAM_TYPE}stream"

# --- Interactive job selection menu ---
show_menu() {
  echo ""
  echo "Job selection (${STREAM_TYPE}-stream):"
  for i in "${!ALL_JOBS_ARR[@]}"; do
    local num=$((i + 1))
    if [[ "${SELECTED_ARR[$i]}" -eq 1 ]]; then
      echo "  [x] $num) ${ALL_JOBS_ARR[$i]}"
    else
      echo "  [ ] $num) ${ALL_JOBS_ARR[$i]}"
    fi
  done
  echo ""
  echo "Toggle a job by entering its number. Press Enter when done."
}

while true; do
  show_menu
  read -rp "> " choice
  if [[ -z "$choice" ]]; then
    break
  fi
  if ! [[ "$choice" =~ ^[0-9]+$ ]] || [[ "$choice" -lt 1 ]] || [[ "$choice" -gt ${#ALL_JOBS_ARR[@]} ]]; then
    echo "Invalid selection: $choice"
    continue
  fi
  idx=$((choice - 1))
  if [[ "${SELECTED_ARR[$idx]}" -eq 1 ]]; then
    SELECTED_ARR[$idx]=0
  else
    SELECTED_ARR[$idx]=1
  fi
done

# --- Build final rename list ---
JOBS_TO_RENAME=""
for i in "${!ALL_JOBS_ARR[@]}"; do
  if [[ "${SELECTED_ARR[$i]}" -eq 1 ]]; then
    JOBS_TO_RENAME="${JOBS_TO_RENAME}${ALL_JOBS_ARR[$i]}\n"
  fi
done

if [[ -z "$(echo -e "$JOBS_TO_RENAME" | grep -v '^$')" ]]; then
  error-exit "No jobs selected. Nothing to rename."
fi

echo ""
echo "Will rename (${STREAM_TYPE}-stream):"
echo -e "$JOBS_TO_RENAME" | while read -r job; do
  [[ -z "$job" ]] && continue
  echo "  $job -> $(echo "$job" | sed 's/winc-/winc-zstream-/')"
done

echo ""
read -rp "Proceed? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted."
  exit 0
fi

# --- Create branch and worktree ---
BRANCH="winc-${RENAME_LABEL}-${OCP_VER}"
cd "$RELEASE_REPO"

echo "Fetching ${RELEASE_REMOTE}..."
git fetch "$RELEASE_REMOTE"

WORKTREE_DIR="${RELEASE_REPO}/../worktrees/${BRANCH}"

if [[ -d "$WORKTREE_DIR" ]]; then
  echo "Removing existing worktree: $WORKTREE_DIR"
  git worktree remove "$WORKTREE_DIR" --force 2>/dev/null || rm -rf "$WORKTREE_DIR"
fi
git worktree prune 2>/dev/null || true

if git show-ref --verify --quiet "refs/heads/$BRANCH" 2>/dev/null; then
  echo "Removing existing branch: $BRANCH"
  git branch -D "$BRANCH"
fi

git worktree add "$WORKTREE_DIR" -b "$BRANCH" "${RELEASE_REMOTE}/main"
cd "$WORKTREE_DIR"

# --- Rename jobs ---
WORKTREE_CONFIG="${WORKTREE_DIR}/${CONFIG_DIR}/openshift-openshift-tests-private-release-${OCP_VER}__amd64-nightly.yaml"

echo -e "$JOBS_TO_RENAME" | while read -r job; do
  [[ -z "$job" ]] && continue
  new_name=$(echo "$job" | sed 's/winc-/winc-zstream-/')
  sed -i.bak "s/^- as: ${job}$/- as: ${new_name}/" "$WORKTREE_CONFIG"
done
rm -f "${WORKTREE_CONFIG}.bak"

# --- Verify renames ---
echo ""
echo "Verifying renames in config file..."
grep 'winc-zstream' "$WORKTREE_CONFIG" | sed 's/^/  /'

# --- Run make update ---
echo ""
echo "Running make update..."
make update

# --- Verify generated jobs ---
JOBS_FILE="${WORKTREE_DIR}/ci-operator/jobs/openshift/openshift-tests-private/openshift-openshift-tests-private-release-${OCP_VER}-periodics.yaml"
if ! grep -q 'winc-zstream' "$JOBS_FILE"; then
  error-exit "Generated jobs file does not contain winc-zstream renames. make update may have failed."
fi

STREAM_LABEL="$(echo "$STREAM_TYPE" | tr '[:lower:]' '[:upper:]') stream"

echo ""
read -rp "Commit, push, and create PR? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Aborted. Changes remain in worktree: $WORKTREE_DIR"
  exit 0
fi

# --- Commit and push ---
git add "${CONFIG_DIR}/" "ci-operator/jobs/openshift/openshift-tests-private/"
git commit -m "DEBUG Do not merge: ${WMCO_VER} ${STREAM_LABEL} triggered jobs"
git push -u -f origin "$BRANCH"

# --- Create PR ---
PR_URL=$(gh pr create --repo openshift/release \
  --title "DEBUG Do not merge: ${WMCO_VER} ${STREAM_LABEL} triggered jobs" \
  --body "This PR modifies periodic job names so they will trigger by /pj-rehearse new jobs with latest ${WMCO_VER} image (${STREAM_LABEL})")

echo ""
echo "PR created: $PR_URL"

PR_NUMBER=$(echo "$PR_URL" | grep -o '[0-9]*$')

# --- Wait for REHEARSALNOTIFIER ---
echo ""
echo "Waiting for REHEARSALNOTIFIER comment (up to 10 minutes)..."
DEADLINE=$((SECONDS + 600))
NOTIFIER_FOUND=false

while [[ $SECONDS -lt $DEADLINE ]]; do
  COMMENT=$(gh pr view "$PR_NUMBER" --repo openshift/release --json comments \
    --jq '.comments[] | select(.body | test("REHEARSALNOTIFIER")) | .body' 2>/dev/null | head -1 || true)
  if [[ -n "$COMMENT" ]]; then
    NOTIFIER_FOUND=true
    break
  fi
  echo "  Waiting... ($((DEADLINE - SECONDS))s remaining)"
  sleep 60
done

if [[ "$NOTIFIER_FOUND" != "true" ]]; then
  echo "REHEARSALNOTIFIER comment not found within 10 minutes."
  echo "You can manually trigger with: /pj-rehearse"
  echo "PR: $PR_URL"
  exit 0
fi

echo ""
echo "REHEARSALNOTIFIER found. Rehearsable jobs:"
echo "$COMMENT" | grep -E 'winc-zstream' | sed 's/^/  /' || true

echo ""
read -rp "Post /pj-rehearse to trigger tests? [y/N] " confirm
if [[ "$confirm" != [yY] ]]; then
  echo "Skipped. Post '/pj-rehearse' manually on: $PR_URL"
  exit 0
fi

JOB_COUNT=$(echo -e "$JOBS_TO_RENAME" | grep -c . || true)
if [[ "$JOB_COUNT" -gt 5 ]]; then
  REHEARSE_CMD="/pj-rehearse max"
else
  REHEARSE_CMD="/pj-rehearse"
fi

gh pr comment "$PR_NUMBER" --repo openshift/release --body "$REHEARSE_CMD"
echo ""
echo "Posted: $REHEARSE_CMD"
echo "PR: $PR_URL"
echo ""
echo "After tests complete, close the PR without merging."
