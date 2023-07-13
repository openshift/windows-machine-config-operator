#!/bin/bash
set -euo pipefail

# this script updates the WMCO and OCP versions in the Makefile, CSV, bundle and Dockerfiles.

# Include the common.sh 
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

CVP=false
BASEIMAGE=false
DISTGIT=false
OPERATOR_SDK_VERSION=v1.15.0

# codes for printing and resetting pink text
PINK='\033[95m'
START_COLOR=$(tput sgr0)

usage() {
  echo "usage:"
  echo "input the name of the branch you want to increment, and the x y or z value to bump"
  echo "example: ./pre-release.sh master x"
  echo "this will bump the x stream of master by 1"
  echo "optional flags: -c"
  echo "  bumps the versions held back by CVP"
  echo "optional flags: -b"
  echo "  bumps the baseimage versions"
  echo "optional flags: -d"
  echo "  generates a patch to bump the dist-git version"
  echo -e "  ${PINK}Requires connection to the RedHat VPN"
  printf "${START_COLOR}"
}

# This function increments any xyz version given an x y or z
# returns the incremented version 
# takes 2 arguments 
# 1st arg: version to update in x.y.z format
# 2nd arg: stream to update. Can be x, y or z to represent the x y or z stream.
increment_version() {
  if [ "$#" -ne 2 ]; then
    echo incorrect parameter count for increment_version $#
    return 1
  fi
  version=$1
  # Setting the stream var to 1 2 or 3 which is used for incrementing the correct stream
  # this tells awk which column to act on. Column 1, column 2, or column 3.
  # which correspond to the x, y and z stream respectively.
  declare -A stream_var
  stream_var["x"]=1
  stream_var["y"]=2
  stream_var["z"]=3
  stream=${stream_var[$2]}

  # zero out the positions less than the one you're updating 
  if [ "$stream" -le 2 ]; then
    version=$(echo $version | awk -F. -v OFS=. -v stream="3" '{$stream = 0 ; print}')
    if [ "$stream" -eq 1 ]; then
      version=$(echo $version | awk -F. -v OFS=. -v stream="2" '{$stream = 0 ; print}')
    fi 
  fi

  # Increment x y or z based on the column split on "."
  # Example: version is set to 6.0.0, stream is set to 1
  # output would be 7.0.0
  echo $version | awk -F. -v OFS=. -v stream="$stream" '{$stream += 1 ; print}'
}

# This function updates the WMCO version in all relevant files
# takes 3 arguments
# 1st arg: stream to update. Can be x, y or z to represent the x y or z stream.
# 2nd arg: WMCO version to update.
# 3rd arg: Updated WMCO version
update_WMCO_version() { 
  if [ "$#" -ne 3 ]; then
    echo incorrect parameter count for update_WMCO_version $#
    return 1
  fi

  stream=$1
  version=$2
  updated_version=$3
  cluster_service_version_yaml=config/manifests/bases/windows-machine-config-operator.clusterserviceversion.yaml
 
  last_version=$(grep "olm.skipRange" $cluster_service_version_yaml |
    grep -oP ">=\K([0-9]+\.[0-9]+\.[0-9]+)\s*<" | sed "s/\s*<//")

  echo "WMCO version $version detected from the Makefile"

  echo "Updating the Makefile to version $updated_version"
  # Example: matches *WMCO_VERSION ?= 9.0.0*
  sed -i "s/WMCO_VERSION ?= $version/WMCO_VERSION ?= $updated_version/g" Makefile

  echo "Updating skipRange upper bound in $cluster_service_version_yaml to $updated_version"
  # Example: matches olm.skipRange: '>=8.0.0 *<9.0.0*'
  sed -i "s/<$version/<$updated_version/g" $cluster_service_version_yaml
  
  if [[ $stream == "x" ]]; then
    echo "Updating skipRange lower bound in $cluster_service_version_yaml to $version"
    # Example: matches olm.skipRange: '*>=8.0.0* <9.0.0'
    sed -i "s/>=$last_version/>=$version/g" $cluster_service_version_yaml
  fi 
  
  echo "Updating bundle/windows-machine-config-operator.package.yaml to $updated_version"
  # Example: matches   - currentCSV: windows-machine-*config-operator.v9.0.0*
  sed -i "s/config-operator.v$version/config-operator.v$updated_version/g" bundle/windows-machine-config-operator.package.yaml
   
}

# This function updates the OCP version in all relevant files 
# takes 3 arguments
# 1st arg: stream to update. Can be x, y or z to represent the x y or z stream. 
# 2nd arg: WMCO version to update.
# 3rd arg: Updated WMCO version
update_OCP_version() {
  if [ "$#" -ne 3 ]; then
    echo incorrect parameter count for update_OCP_version $#
    return 1
  fi

  stream=$1
  wmco_version=$2
  updated_version=$3
  cluster_service_version_yaml=config/manifests/bases/windows-machine-config-operator.clusterserviceversion.yaml
  
  if [[ "$stream" != "x" ]]; then
    echo ""
    echo "no need to update OCP version"
    echo ""
    return 0
  fi
  
  version=$(get_OCP_version $wmco_version)
  echo "using OCP version $version"

  # Find the last version in the dockerfiles
  last_version=$(sed -n 's/.*\([0-9]\.[0-9][0-9]\).*/\1/p' build/Dockerfile)

  echo "Updating $cluster_service_version_yaml version to $updated_version"
  # Example: matches on any instance of the OCP version, such as 4.14 
  sed -i "s/$version/$updated_version/g" $cluster_service_version_yaml

  if [[ $CVP == "true" ]]; then  
    echo "Updating bundle.Dockerfile version to $updated_version"
    # Example: matches LABEL com.redhat.openshift.versions="*=v4.13*"
    sed -i "s/=v$version/=v$updated_version/g" bundle.Dockerfile

    echo "Updating bundle.Dockerfile last version to $version"
    # Example: matches LABEL com.redhat.openshift.versions="=v4.13"
    sed -i "s/=v$last_version/=v$version/g" bundle.Dockerfile

    echo "Updating bundle/metadata/annotations.yaml version to $updated_version"
    # Example: matches com.redhat.openshift.versions: "*=v4.13*"
    sed -i "s/=v$version/=v$updated_version/g" bundle/metadata/annotations.yaml 
  else
    echo ""
    echo "skipping bundle.Dockerfile and bundle/metadata/annotations.yaml due to CVP being a version behind"
    echo "if these do need to be bumped, rerun with the -c flag from your original branch"
    echo ""
  fi 

  if [[ $BASEIMAGE == "true" ]]; then   
    baseimages=("Dockerfile" "Dockerfile.base" "Dockerfile.ci" "Dockerfile.wmco")
    for item in "${baseimages[@]}"; do 
      echo "Updating build/$item to $updated_version"
      sed -i "s/openshift-$version/openshift-$updated_version/g" build/$item
    done
  else
    echo ""
    echo "skipping updating base images"
    echo "if these do need to be bumped, rerun with the -b flag from your original branch"
    echo ""
  fi 
}

# This function handles updating the versions in GitHub
# takes 4 arguments
# 1st arg: base branch to operate on
# 2nd arg: stream to update. Can be x, y or z to represent the x y or z stream.
# 3rd arg: current WMCO version
# 4th arg: updated WMCO version
github_update() {
  if [ "$#" -ne 4 ]; then
    echo incorrect parameter count for github_update $#
    return 1
  fi

  base_branch=$1
  stream=$2
  wmco_version=$3
  updated_version=$4

  # check if the git branch is dirty, and exit if it is
  check_git_branch

  new_branch=$(create_git_branch_list $stream-stream-bump $base_branch)
  switch_git_branch $base_branch $new_branch

  update_WMCO_version $stream $wmco_version $updated_version
  update_OCP_version $stream $wmco_version $updated_version

  git submodule update --init --recursive
  make bundle

  # remove make bundle artifacts
  sed -i 's/REPLACE_IMAGE:latest/REPLACE_IMAGE/' bundle/manifests/windows-machine-config-operator.clusterserviceversion.yaml
  sed -i 's/operator-sdk-v1.14.0+git/operator-sdk-v1.15.0+git/' bundle/manifests/windows-machine-config-operator.clusterserviceversion.yaml

  commit_message="[$base_branch] Update version to $updated_version

  This commit was generated by hack/pre-release.sh"
  generate_commit "$commit_message" build bundle Makefile config

  # Display the newly created branches with push instructions
  push_message=$(display_push_message "$new_branch")
  echo
  echo -e "${PINK}${push_message}"
  printf "${START_COLOR}"
  echo
}

# This function handles updating the versions in dist_git and creates a patch. Expects you to be connected to the VPN 
# takes 2 arguments
# 1st arg: stream to update. Can be x, y or z to represent the x y or z stream.#
# 2nd arg: current WMCO version
# 3rd arg: updated WMCO version
dist_git_update() {
  if [ "$#" -ne 3 ]; then
    echo incorrect parameter count for dist_git_update $#
    return 1
  fi
  stream=$1
  wmco_version=$2
  updated_version=$3

  current_dir=$PWD
  # clones the dist-git repo into a temporary directory
  # this will be cleaned up at the end
  temp=$(mktemp -d)
  cd $temp

  set +o errexit
  git clone https://pkgs.devel.redhat.com/git/containers/windows-machine-config-operator-bundle tmp/windows-machine-config-operator-bundle
  if [ $? -ne 0 ]; then
    echo -e "${PINK}git clone failed. Are you connected to the VPN?"
    printf "${START_COLOR}"
    rm -rf $temp
    exit 1
  fi
  set -o errexit

  DIST_GIT_LOCATION=$temp/tmp/windows-machine-config-operator-bundle

  OCP_version=$(get_OCP_version $wmco_version)

  echo "dist-git location set to $DIST_GIT_LOCATION"
  cd $DIST_GIT_LOCATION
  check_git_branch

  # switch to the dist-git branch that corresponds with the current ocp version
  rhel_version=$(get_rhel_version $OCP_version)
  base_branch=rhaos-$OCP_version-rhel-$rhel_version
  git checkout $base_branch

  previous_version=$(grep -Po 'previous_version="\K.*(?=")' render_templates)

  # Example: matches version="v8.0.1"
  sed -i "s/version=\"v[0-9]\+\(\.[0-9]\+\)\+\"/version=\"v$updated_version\"/g" Dockerfile

  # Example: matches version: 8.0.1
  sed -i "s/version: [0-9]\+\(\.[0-9]\+\)\+/version: $updated_version/g" render_templates

  # Example: matches previous_version="8.0.1"
  sed -i "s/previous_version=\"[0-9]\+\(\.[0-9]\+\)\+\"/previous_version=\"$wmco_version\"/g" render_templates

  # Comment out lines during x stream releases, and uncomment during other releases. 
  if [ $stream == "x" ]; then 
    sed -i "s@^....sed -i '/version@    # sed -i '/version@" render_templates
    sed -i 's/^previous_version="/# previous_version="/' render_templates
  else
    sed -i "s@# sed -i '/version@sed -i '/version@" render_templates
    sed -i 's/# previous_version="/previous_version="/' render_templates
  fi 

  echo "updated $wmco_version to $updated_version"

  commit_message="[release-$OCP_version] Prepare for $updated_version release

  This commit was generated by hack/pre-release.sh"

  generate_commit "$commit_message" Dockerfile render_templates

  # Generates a patch that can be applied and pushedby the users
  patch_name=dist-git-$wmco_version-to-$updated_version.patch
  git format-patch -1 --output=$current_dir/$patch_name

  cd $current_dir
  rm -rf $temp
  cat $current_dir/$patch_name
  echo
  echo -e "${PINK}Patch $patch_name created. Apply in your dist-git repo using \n
    git apply $patch_name"
    printf "${START_COLOR}"
  echo
}

# check to see if the operator-sdk version is correct
operator_sdk_current_version=$(operator-sdk version |  grep -oP 'operator-sdk version: "\K[^"]+' )
if [[ "$operator_sdk_current_version" != "$OPERATOR_SDK_VERSION" ]]; then
  echo -e "${PINK}operator-sdk $OPERATOR_SDK_VERSION must be installed."
  echo -e "${PINK}exiting."
  printf "${START_COLOR}"
  exit
fi

# This sets the CVP and baseimage flags
while getopts "bcd" opt; do
  case "$opt" in 
    c) CVP=true;;
    b) BASEIMAGE=true;;
    d) DISTGIT=true;;
  esac 
done 
shift $((OPTIND-1))

# If there are less than two arguments, exit
if [ ${#@} -lt 2 ]; then
  echo "Invalid number of args"
  echo ""
  usage
  exit
fi

# the base branch comes first, and the stream to update comes second 
base_branch=$1
stream=$2

if [[ $stream != "x" && $stream != "y" && $stream != "z" ]]; then
  echo ""
  echo "Stream argument can only be x, y or z"
  echo "input stream argument is $stream"
  echo ""
  usage
  exit
fi

initial_branch=$(git branch --show-current)
git checkout $base_branch
git submodule update --init --recursive  > /dev/null 2>&1

# Locate and store the WMCO version from the makefile
# finds "WMCO_VERSION ?=" and selects text following it
WMCO_VERSION=$(sed -n -e 's/^.*WMCO_VERSION ?= //p' Makefile)
updated_version=$(increment_version $WMCO_VERSION $stream)

if [[ $DISTGIT == "false" ]]; then
  github_update $base_branch $stream $WMCO_VERSION $updated_version
else
  dist_git_update $stream $WMCO_VERSION $updated_version
fi

git checkout $initial_branch
git submodule update --init --recursive  > /dev/null 2>&1
