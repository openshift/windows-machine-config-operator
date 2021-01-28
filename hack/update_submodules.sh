#!/bin/bash

# Update submodules HEAD to remote branches

set -euo pipefail

function help() {
  echo "Usage: update_submodules.sh [OPTIONS] branch_name"
  echo "Update submodules HEAD to remote branches"
  echo "Must be executed in repo root directory"
  echo ""
  echo "Options:"
  echo "-m   Only update the specified modules, separated by spaces. Defaults to all submodules"
  echo "-r   Set a new remote branch for the module. Only usable when updating a single module"
  echo "-h   Shows usage text"
  echo ""
  echo "Examples:"
  echo "# Update all submodules for the branches master and release-4.6"
  echo "update_submodules.sh master release-4.6"
  echo ""
  echo "# Update the submodules kubernetes and ovn-kubernetes for the branch master"
  echo "update_submodules.sh -m \"kubernetes ovn-kubernetes\" master"
  echo ""
  echo "# Point the kubernetes submodule at the remote branch release-4.9 for the branch master"
  echo "update_submodules.sh -m kubernetes -r release-4.9 master"
}

# Stages changes to the given submodule and makes a commit including those changes
function generate_submodule_commit() {
  local submodule=$1
  cd $submodule
  submodule_url=$(git remote get-url origin)
  short_head=$(git rev-parse --short HEAD)
  long_head=$(git rev-parse HEAD)
  cd ..
  git add $submodule
  # Commit changes if there are any
  if git commit -m "[submodule][$submodule] Update to $short_head" -m "Update to $submodule_url/commit/$long_head" -m "This commit was generated using hack/update_submodules.sh"; then
    echo "New commit for $submodule"
  fi
}

# Creates a new branch based off the given branch with commits updating all submodules provided in a space separated list
function update_submodules_for_branch() {
  local base_branch=$1
  local new_branch=$2
  local modulelist=$3
  local remote_branch=${4:-""}

  if git branch -D $new_branch; then
    echo "Deleted branch $new_branch"
  fi
  git checkout $base_branch -b $new_branch

  # Generate a commit updating each submodule
  for submodule in $modulelist; do
    # Update the branch of the submodule if it has changed
    if [ ! -z $remote_branch ]; then
      git config -f .gitmodules submodule.$submodule.branch $remote_branch
      git add .gitmodules
    fi
    # Update the submodule to the latest remote commit
    git submodule update --remote $submodule
    generate_submodule_commit $submodule
  done
}

modulelist=""
remote_branch=""
while getopts "m:r:h?" opt; do
  case "$opt" in
  m) modulelist=$OPTARG;;
  r) remote_branch=$OPTARG;;
  h|\?) help; exit 0;;
  esac
done
shift $((OPTIND -1))

# If the -m option wasnt given, update all modules
if [ -z "$modulelist" ]; then
  modulelist=$(git config --file .gitmodules --get-regexp path | awk '{ print $2 }')
fi

# If the -r option was given, ensure only one module is being updated
if [ ! -z "$remote_branch" ]; then
  # a space in the modulelist indicates there are multiple modules specified
  if [[ "$modulelist" == *" "* ]]; then
    echo "The '-r' option can only be used when a single module is specified with '-m'"
    exit 1
  fi
fi

# Return if the working branch is dirty
if ! git diff --quiet; then
  echo "branch dirty, exiting to not overwrite work"
  exit 1
fi

# For each branch that the user gives (master, release-4.7), create a new branch based off it,
# with commits updating the submodules
initial_branch=$(git branch --show-current)
new_branch_list=()
for branch in "$@"; do
  new_branch="$branch-submodule-update-$(date +%m-%d)"
  update_submodules_for_branch "$branch" "$new_branch" "$modulelist" "$remote_branch"
  new_branch_list+=($new_branch)
done

# Return to initial branch
git checkout $initial_branch
echo "****"
echo "New branches created:"
echo "    [${new_branch_list[@]}]"
echo ""
echo "For each branch, you may push to your fork and create a PR against openshift/windows-machine-config-operator"
echo "example:"
echo "# assumes you have your fork set as the remote 'origin'"
echo "git push origin ${new_branch_list[0]}"

