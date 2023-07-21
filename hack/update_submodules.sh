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

function generate_version_commit() {
  local submodule=$1
  cd "$submodule"
  if ! git remote show upstream; then
    if [ "$submodule" = "containerd" ]; then
      git remote add upstream https://github.com/containerd/containerd
    else
      git remote add upstream https://github.com/kubernetes/kubernetes
    fi
  fi
  git fetch upstream 'refs/tags/v1*:refs/tags/v1*'
  if [ "$submodule" = "containerd" ]; then
    # Ref: Containerd Version https://github.com/containerd/containerd/blob/main/Makefile#L33
    version=$(git describe --match 'v[0-9]*' --dirty='.m' --always)
  else
    # git describe --abbrev=7 returns vx.y.z-number_of_commits-hash. Remove the -number_of_commits-g and replace it
    # with + to match the Linux kubelet version pattern
    version=$(git describe --abbrev=7  | sed --expression="s|-.*-g|+|g")
  fi
  cd ..
  # Construct the Makefile variable $submodule_GIT_VERSION
  local makefile_var="$(echo "$submodule" | tr '[:lower:]' '[:upper:]')_GIT_VERSION"
  # Makefile has "$submodule_GIT_VERSION=vx.y.z+hash. Replace the $submodule_GIT_VERSION= with the new version
  sed -i "s|$makefile_var=v.*|$makefile_var=$version|g" Makefile
  git add Makefile
  if git commit -m "[build] Update $submodule version to $version" -m "This commit was generated using hack/update_submodules.sh"; then
    echo "New Makefile commit for $submodule version $version"
  fi
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
    if [ "$submodule" = "kubelet" ] || [ "$submodule" = "kube-proxy" ] || [ "$submodule" = "containerd" ]; then
      generate_version_commit "$submodule"
    fi
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


WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

# Check if branch is dirty, and exit if it is
check_git_branch
initial_branch=$(git branch --show-current)

# creates a list of branches to be created
new_branch_list=$(create_git_branch_list submodule-update $@)

# counter to sync the generated branch list with the arguments
i=1
for new_branch in $new_branch_list; do
  update_submodules_for_branch "${!i}" "$new_branch" "$modulelist" "$remote_branch"
  i=$((i+1))
done

# Return to initial branch
git checkout $initial_branch

display_push_message "${new_branch_list[@]}"
