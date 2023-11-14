#!/bin/bash
set -euo pipefail

# Purpose
# -------
# Which PRs need to be backported to a branch that has been frozen for release purposes?
#
# This script will query Github to find all PRs that have merged into master and more recent release branches
# since a release blocker issue was opened for the specified branch. The script will list the correct merge order of all
# PRs that went in to make it easier to see what needs to be backported once the release blocker issue is closed.
#
# Install Required Tools
# ----------------------
# jq gh
#
# Usage
# -----
# * Ensure your local master branch is up-to-date
# * Run this script from master:
# * ./hack/merge-order.sh <branch_name>

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/util.sh

function help() {
  printf "${BRIGHT}"
  echo "Usage: merge-order.sh branch_name"
  echo "List merged PRs in order for all relevant branches since release blocker issue was opened for the given branch."
  echo ""
  echo "Example:"
  echo "# Get merged PRs since code freeze for WMCO v8 for all branches above release-4.13 (master, release-4.14, etc.)"
  echo "errata-check.sh release-4.13"
  printf "${NORMAL}"
}


# Main script #
if [ $# -eq 0 ]; then
    print-error "Invalid arguments, no branch provided"
    help
    exit 1
fi

TARGET_BRANCH=$1
BRANCH_VERSION=$(validate-target-branch "$TARGET_BRANCH")

# Check for a blocking issue for the given branch
rel_issue=$(gh issue list --state open --search "Merge blocker for active release | branch:$TARGET_BRANCH" \
    --json author,createdAt,number,title,url)

# Extract the total count of issues from the response
total_count=$(echo "${rel_issue}" | jq length)
if [[ ${total_count} -ne 1 ]]; then
  print-error "Expected one issue for branch $TARGET_BRANCH, found $total_count instead."
  exit 1
fi

print-label "Found blocking Github issue for branch" "$TARGET_BRANCH"
echo "$rel_issue" | jq -r '[.[] | {Issue_Number: .number, Title: .title, Author: .author.login, Created_Date: .createdAt, URL: .url}] | (first | keys_unsorted) as $keys | $keys, map([.[ $keys[] ]])[] | @tsv' | column -t -s $'\t' | \
    awk -F'\t' 'NR==1 { printf ("\033[1;33m%s\t%s\t%s\t%s\t%s\033[0m\n", $1, $2, $3, $4, $5) } NR>1 { print }'

date_created=$(date -d "$(echo "${rel_issue}" | jq -r ".[].createdAt")" +"%Y-%m-%dT%H:%M:%SZ")

print_separator

# Get the WMCO version (x.y.z) from the Makefile
WMCO_VERSION=$(sed -n -e 's/^.*WMCO_VERSION ?= //p' Makefile)
# The minor WMCO version (y) maps to a release branch number (release-4.y)
MINOR_WMCO_VERSION="$(echo "${WMCO_VERSION#*.}" | cut -d '.' -f 1)"

curr_branch="master"
for ((i=$MINOR_WMCO_VERSION+1; i>$BRANCH_VERSION;)); do
  # Get PRs merged after the merge blocker issue was opened, sorted by merge date
  mergedPRs=$(gh pr list --state merged --base $curr_branch --json author,mergedAt,number,title,url --jq ". | sort_by(.mergedAt) | [.[] | select(.mergedAt > \"$date_created\")]")
  count=$(echo "$mergedPRs" | jq length)

  printf "${YELLOW}%d${NORMAL} PRs merged into ${GREEN}%s${NORMAL} since blocker for %s was opened on %s\n" $count "$curr_branch" "$TARGET_BRANCH" "$date_created"

  if [ "$count" -ne 0 ]; then
    print-info "Merge Order:"
    echo "$mergedPRs" | jq -r '[.[] | {PR_Number: .number, Title: .title, Author: .author.login, Merged_Date: .mergedAt, URL: .url}] | (first | keys_unsorted) as $keys | $keys, map([.[ $keys[] ]])[] | @tsv' | column -t -s $'\t' | \
        awk -F'\t' 'NR==1 { printf ("\033[1;33m%s\t%s\t%s\t%s\t%s\033[0m\n", $1, $2, $3, $4, $5) } NR>1 { print }'
  fi
  print_separator

  # Repeat for each release version above the target version
  i=$((i-1))
  curr_branch="release-4.$i"
done





