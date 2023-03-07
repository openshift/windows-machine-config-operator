#!/usr/bin/env bash
set -euo pipefail

# RED HAT INTERNAL USE ONLY
#
# Purpose
# -------
# Which PRs are in the build currently attached to the errata for the next
# release?
#
# The script will query Errata Tool for the active errata based on the
# branch specified.  It will then look up the build attached and
# determine the WMCO commit the build was built from.  Finally, it will list the
# merge commits since the last WMCO release for that version.  The commit
# associated with the build will be tagged with a shortened build NVR,
# 'wmco-container-X.Y.Z-n'.
#
# Install Required Tools
# ----------------------
# dnf install curl jq gh
#
# Usage
# -----
# * VPN connectivity and valid kerberos ticket required
# * Update your local clone and branches
# * Run this script ./hack/errata-check.sh <branch_name>

# Set variables
ERRATA_API_URL="https://errata.devel.redhat.com/api/v1/erratum"
RED=$(tput setaf 1)
GREEN=$(tput setaf 2)
YELLOW=$(tput setaf 3)
BLUE=$(tput setaf 4)
BRIGHT=$(tput bold)
NORMAL=$(tput sgr0)

# Define functions
function help() {
  printf "${YELLOW}"
  echo "Usage: errata-check.sh branch_name"
  echo "List merge commits in the build attached to an active release errata"
  echo ""
  echo "Example:"
  echo "# Check the build for the next WMCO v7 errata"
  echo "errata-check.sh release-4.12"
  printf "${NORMAL}"
}

function verify-args {
  if [ $# -eq 0 ]
  then
    print-error "No branch provided"
    help
    exit 1
  fi
}

function print-label {
  local label=$1
  local item=$2
  printf "${BLUE}%-20s= ${NORMAL}%s\n" "$label" "$item"
}

function print-info {
  local text=$1
  printf "\n${BRIGHT}%s${NORMAL}\n" "$text"
}

function print-green {
  local text=$1
  printf "${GREEN}%s${NORMAL}\n" "$text"
}

function print-yellow {
  local text=$1
  printf "${YELLOW}%s${NORMAL}\n" "$text"
}

function print-error {
  local text=$1
  printf "\n${RED}ERROR: %s${NORMAL}\n" "$text" 1>&2
}

function validate-target-branch {
  local branch=$1
  if [[ "$branch" =~ ^release-(.*[^0-9])([0-9]+)$ ]]; then
    # Branches older than release-4.9 are unsupported
    if [[ ${BASH_REMATCH[2]} -lt 9 ]]; then
      print-error "Branch $branch is unsupported, only release-4.9 and newer are supported"
      exit 1
    fi
  else
    print-error "Branch $branch is unsupported, only 'release-x.y' branches are supported"
    exit 1
  fi
}

function check-upstream-status {
  # Checks if the local repo is up to date with upstream
  local branch=$1
  if ! git branch | grep "$branch" > /dev/null 2>&1; then
    print-error "Branch $branch does not exist locally"
    exit 1
  fi
  local local_head
  local upstream_head
  local_head=$(git rev-parse "$branch")
  upstream_head=$(git ls-remote upstream -h "refs/heads/$branch" | awk '{ print $1 }')
  if [ "$local_head" != "$upstream_head" ]; then
    print-error "Local branch $branch is not up to date with upstream"
    exit 1
  fi
}

function get-wmco-version {
  local branch=$1
  WMCO_VERSION=$(git show "$branch":Makefile | grep -Po '(?<=WMCO_VERSION \?= )\d+')
  print-label "WMCO Version" "v$WMCO_VERSION"
}

function verify-errata-connectivity {
  local url="https://errata.devel.redhat.com"
  local result
  hostname=$(echo -E "$url" | awk -F/ '{print $3}')
  # Test connection by only retrieving headers
  set +o errexit
  result=$(curl --head --silent --negotiate --user : "$url")
  exitcode=$?
  set -o errexit
  if [ $exitcode -ne 0 ]; then
    print-error "Unable to curl server. (hostname: $hostname, error: $exitcode)  Connected to VPN?"
    exit 1
  fi
  if echo -E "$result" | grep "401 Unauthorized"; then
    print-error "401 Unauthorized.  (hostname: $hostname)  Valid kerberos ticket?"
    exit 1
  fi
}

function curl-json {
  local url=$1
  local result
  # Retrieve the requested content
  result=$(curl --silent --negotiate --user : "$url")
  echo -E "$result"
}

function add-build-tag {
  # Add a tag for the build nvr (wmco-container-X.Y.Z-n)
  print-info "Adding build tag:"
  local build_nvr=$1
  local wmco_tag
  wmco_tag="wmco-container-${build_nvr:42}"
  if git show-ref --tags "refs/tags/$wmco_tag"; then
    print-green "Tag for $wmco_tag already exists"
  else
    print-yellow "Tagging WMCO hash $GITHUB_HASH as $wmco_tag"
    git tag "$wmco_tag" "$GITHUB_HASH"
  fi
}

function get-commits {
  local start_hash=$1
  local end_hash=$2
  mapfile -t MERGE_COMMITS < <(git rev-list --format=oneline --merges "$start_hash...$end_hash")
}

function get-jira-fix-version {
  local jira_id=$1
  local result
  local exitcode
  local fix_version_count
  local fix_version
  result=$(curl -s "https://issues.redhat.com/rest/api/2/issue/$jira_id")
  exitcode=$?
  if [ $exitcode -ne 0 ]; then
    print-error "Unable to curl server. (error: $exitcode)  Connected to VPN?"
    exit 1
  fi
  if [[ "$result" == *"Login Required"* ]]; then
    echo "Restricted"
    return
  fi

  fix_version_count=$(echo -E "$result" | jq '.fields.fixVersions | length')
  if [ "$fix_version_count" -gt 1 ]; then
    echo "Multiple"
    return
  elif [ "$fix_version_count" -eq 0 ]; then
    echo "Undefined"
    return
  else
    fix_version=$(echo -E "$result" | jq -r '.fields.fixVersions[0].name')
    echo "$fix_version"
  fi
}

function list-commits {
  local color=$1
  local -r format="%-40s %-4s %-65.65s %-13s %-11s %-13s\n"
  printf "${BLUE}"
  printf "$format" "Commit Hash" "PR#" "PR Title" "Jira ID" "Fix Version" "Errata Status"
  printf "%s %s %s %s %s %s\n" "$(printf "=%.0s" {1..40})" "$(printf "=%.0s" {1..4})" "$(printf "=%.0s" {1..65})" "$(printf "=%.0s" {1..13})" "$(printf "=%.0s" {1..11})" "$(printf "=%.0s" {1..13})"
  printf "$color"
  for commit in "${MERGE_COMMITS[@]}"
  do
    local commit_hash=""
    local pr_num=""
    local pr_title=""
    local jira_id=""
    local jira_fix_version=""
    local attached=""
    commit_hash=$(echo "$commit" | awk '{ print $1 }')
    pr_num=$(echo "$commit" | awk '{ print substr($5,2) }')
    pr_title=$(gh pr view "$pr_num" --json title | jq -r .title)
    jira_id=$(echo "$pr_title" | grep -Eo '(WINC|OCPBUGS)-[0-9]*') || true
    if [ -n "$jira_id" ]; then
      jira_fix_version=$(get-jira-fix-version "$jira_id")
      if [[ "$ERRATA_JIRA_IDS" == *"$jira_id"* ]]; then
        attached="${GREEN}Attached${color}"
      else
        attached="${YELLOW}Not Attached${color}"
      fi
    fi
    printf "$format" "$commit_hash" "$pr_num" "$pr_title" "$jira_id" "$jira_fix_version" "$attached"
  done
  printf "${NORMAL}"
}

function get-last-wmco-version {
  local branch=$1
  local last_release=""
  local result
  result=$(git tag --merged "$branch" | { grep -E "^v" || true; })
  if [ -z "$result" ]; then
    # Release tag not found, find the branching point from the last release
    # Decrement the y-value of the branch
    [[ "$branch" =~ (.*[^0-9])([0-9]+)$ ]] && last_release="${BASH_REMATCH[1]}$((BASH_REMATCH[2] - 1))";
    # Check if last release branch is cloned locally
    print-yellow "Release tag not found, finding branching point from $last_release"
    check-upstream-status "$last_release"
    LAST_WMCO_VERSION="Branching Day ${WMCO_VERSION}.0.0"
    LAST_WMCO_HASH=$({ diff -u <(git rev-list --first-parent "$branch") <(git rev-list --first-parent "$last_release") || true; } | sed -ne 's/^ //p' | head -1)
    return
  else
    # Determine last release from version tags
    LAST_WMCO_VERSION=$(echo -E "$result" | sort --version-sort | tail -1 )
    LAST_WMCO_HASH=$(git rev-list -n 1 "$LAST_WMCO_VERSION")
  fi
}

###############
# Main script #
###############
verify-args "$@"

TARGET_BRANCH=$1
validate-target-branch "$TARGET_BRANCH"
verify-errata-connectivity
check-upstream-status "$TARGET_BRANCH"
get-wmco-version "$TARGET_BRANCH"
get-last-wmco-version "$TARGET_BRANCH"

# Query Errata Tool for the active errata for the desired release version
ERRATA_SEARCH=$(curl-json "$ERRATA_API_URL/search?product%5B%5D=79&show_state_IN_PUSH=1&show_state_NEW_FILES=1&show_state_PUSH_READY=1&show_state_QE=1&show_state_REL_PREP=1&synopsis_text=Windows+Containers+$WMCO_VERSION")
# Parse errata search result for errata details
ERRATA_ID=$(echo -E "$ERRATA_SEARCH" | jq '.data[0].id')
print-label "Errata Link" "https://errata.devel.redhat.com/advisory/$ERRATA_ID"
ERRATA_STATUS=$(echo -E "$ERRATA_SEARCH" | jq -r '.data[0].status')
print-label "Errata Status" "$ERRATA_STATUS"
ERRATA_SYNOPSIS=$(echo -E "$ERRATA_SEARCH" | jq -r '.data[0].synopsis')
print-label "Errata Synopsis" "$ERRATA_SYNOPSIS"

# Query Errata Tool for errata details
ERRATA_JSON=$(curl-json "$ERRATA_API_URL/$ERRATA_ID.json")
ERRATA_JIRA_IDS=$(echo -E "$ERRATA_JSON" | jq -r '.jira_issues.idsfixed | join (" ")')

# Query Errata Tool for the builds attached to the errata
ERRATA_BUILDS_JSON=$(curl-json "$ERRATA_API_URL/$ERRATA_ID/builds.json")

# Select "windows-machine-config-operator-bundle-container" build
BUILD_WMCO_BUNDLE=$(echo -E "$ERRATA_BUILDS_JSON" \
  | jq 'to_entries[] | select(.key|startswith("OSE")) | .value.builds | .[] | with_entries(select(.key | match("windows-machine-config-operator-bundle-container")))[]')
BUILD_BUNDLE_NVR=$(echo -E "$BUILD_WMCO_BUNDLE" | jq -r '.nvr')
print-label "Build NVR (bundle)" "$BUILD_BUNDLE_NVR"
BUNDLE_CVP_URL=$(curl -s "http://external-ci-coldstorage.datahub.redhat.com/cvp/cvp-redhat-operator-bundle-image-validation-test/${BUILD_BUNDLE_NVR}/" | grep "$BUILD_BUNDLE_NVR" | tail -1 | awk -F\" '{ print $2 }')

# Select "windows-machine-config-operator-container" build
BUILD_WMCO_CONTAINER=$(echo -E "$ERRATA_BUILDS_JSON" \
  | jq 'to_entries[] | select(.key|startswith("OSE")) | .value.builds | .[] | with_entries(select(.key | match("windows-machine-config-operator-container")))[]')
BUILD_NVR=$(echo -E "$BUILD_WMCO_CONTAINER" | jq -r '.nvr')
print-label "Build NVR" "$BUILD_NVR"
CONTAINER_CVP_URL=$(curl -s "http://external-ci-coldstorage.datahub.redhat.com/cvp/cvp-product-test/${BUILD_NVR}/" | grep "$BUILD_NVR" | tail -1 | awk -F\" '{ print $2 }')

BUILD_ID=$(echo -E "$BUILD_WMCO_CONTAINER" | jq '.id')
BREW_LINK="https://brewweb.engineering.redhat.com/brew/buildinfo?buildID=$BUILD_ID"
print-label "Brew build link" "$BREW_LINK"

print-info "CVP Test Reports:"
print-label "Bundle CVP Report" "${BUNDLE_CVP_URL}cvp-test-report.html"
print-label "Container CVP Report" "${CONTAINER_CVP_URL}cvp-test-report.html"

print-info "Built using the following commits:"
# Query Brew for the distgit hash used for the build
DISTGIT_HASH=$(curl -s "$BREW_LINK" \
  | grep Source | awk -F# '{ print substr($2, 1, length($2)-5) }')
DISTGIT_URL="https://pkgs.devel.redhat.com/cgit/containers/windows-machine-config-operator/commit/?id=$DISTGIT_HASH"
print-label "DistGit link" "$DISTGIT_URL"

# Query DisGit for the Midstream hash used for the build
MIDSTREAM_HASH=$(curl -s "$DISTGIT_URL" \
  | grep "Midstream ref:" | awk -F# '{ print $2 }')
print-label "Midstream link" "https://gitlab.cee.redhat.com/openshift-winc-midstream/openshift-winc-midstream/-/commit/$MIDSTREAM_HASH"

# Query distgit for the GitHub hash that matches the distgit hash for the build
GITHUB_HASH=$(curl -s \
  "https://pkgs.devel.redhat.com/cgit/containers/windows-machine-config-operator/tree/container.yaml/?id=$DISTGIT_HASH" \
  | grep "ref:" | awk '{ print $2 }')
print-label "GitHub link" "https://github.com/openshift/windows-machine-config-operator/commit/$GITHUB_HASH"

add-build-tag "$BUILD_NVR"

print-info "Merge commits in $TARGET_BRANCH after build $BUILD_NVR"
get-commits "$GITHUB_HASH" "refs/heads/$TARGET_BRANCH"
list-commits "${YELLOW}"

print-info "Merge commits in $TARGET_BRANCH since WMCO $LAST_WMCO_VERSION included in build $BUILD_NVR"
get-commits "$LAST_WMCO_HASH" "$GITHUB_HASH"
list-commits "${GREEN}"
