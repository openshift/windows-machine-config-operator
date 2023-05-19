#!/bin/bash
set -euo pipefail
# this script updates the WMCO and OCP versions in the Makefile, CSV, bundle and Dockerfiles.

# Include the common.sh 
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

CVP=false
BASEIMAGE=false

usage() {
  echo "usage:"
  echo "input the name of the branch you want to increment, and the x y or z value to bump"
  echo "example: ./pre-release.sh master x"
  echo "this will bump the x stream of master by 1"
  echo "optional flags: -c"
  echo "  bumps the versions held back by CVP"
  echo "optional flags: -b"
  echo "  bumps the baseimage versions"
}

# This function updates the WMCO version in all relevant files 
# takes 2 arguments 
# 1st arg: stream to update. Can be x, y or z to represent the x y or z stream. 
# 2nd arg: WMCO version to update.
update_WMCO_version() { 
  if [ "$#" -ne 2 ]; then
    echo incorrect parameter count for update_WMCO_version $#
    return 1
  fi

  xyz_stream=$1
  version=$2 
  cluster_service_version_yaml=config/manifests/bases/windows-machine-config-operator.clusterserviceversion.yaml

  # Setting the stream var to 1 2 or 3 which is used for incrementing the correct stream
  # this tells awk which column to act on. Column 1, column 2, or column 3.
  # which correspond to the x, y and z stream respectively. 
  if [[ $xyz_stream == "x" ]]; then
    stream_var=1
  fi
  if [[ $xyz_stream == "y" ]]; then
    stream_var=2
  fi
  if [[ $xyz_stream == "z" ]]; then
    stream_var=3
  fi
 
  # Increment the version
  updated_version=$(echo $version |
    awk -F. -v OFS=. -v stream_var="$stream_var" '{$stream_var += 1 ; print}')

  last_version=$(grep "olm.skipRange" $cluster_service_version_yaml |
    grep -oP ">=\K([0-9]+\.[0-9]+\.[0-9]+)\s*<" | sed "s/\s*<//")

  echo "WMCO version $version detected from the Makefile"

  echo "Updating the Makefile to version $updated_version"
  # Example: matches *WMCO_VERSION ?= 9.0.0*
  sed -i "s/WMCO_VERSION ?= $version/WMCO_VERSION ?= $updated_version/g" Makefile

  echo "Updating the skipRange version in $cluster_service_version_yaml to $updated_version"
  # Example: matches olm.skipRange: '>=8.0.0 *<9.0.0*'
  sed -i "s/<$version/<$updated_version/g" $cluster_service_version_yaml

  echo "Updating previous skipRange version in $cluster_service_version_yaml to $version"
  # Example: matches olm.skipRange: '*>=8.0.0* <9.0.0'
  sed -i "s/>=$last_version/>=$version/g" $cluster_service_version_yaml
 
  echo "Updating bundle/windows-machine-config-operator.package.yaml to $updated_version"
  # Example: matches   - currentCSV: windows-machine-*config-operator.v9.0.0*
  sed -i "s/config-operator.v$version/config-operator.v$updated_version/g" bundle/windows-machine-config-operator.package.yaml
   
}

# This function updates the OCP version in all relevant files 
# takes 2 arguments 
# 1st arg: stream to update. Can be x, y or z to represent the x y or z stream. 
# 2nd arg: WMCO version to update.
update_OCP_version() {
  if [ "$#" -ne 2 ]; then
    echo incorrect parameter count for update_OCP_version $#
    return 1
  fi

  stream=$1
  wmco_version=$2 
  cluster_service_version_yaml=config/manifests/bases/windows-machine-config-operator.clusterserviceversion.yaml
  
  # stream_var tells awk which column to act on. Column 2 is the OCP y stream 
  if [[ $stream == "x" ]]; then
    stream_var=2
  else 
    echo ""
    echo "no need to update OCP version"
    echo ""
    return 0
  fi
  
  version=$(get_OCP_version $wmco_version)
  echo "using OCP version $version"

  # Increment the version by using the stream_var variable to select which column to increment 
  updated_version=$(echo $version |
    awk -F. -v OFS=. -v stream_var="$stream_var" '{$stream_var += 1 ; print}')

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

# This sets the CVP and baseimage flags
while getopts "bc" opt; do 
  case "$opt" in 
    c) CVP=true;;
    b) BASEIMAGE=true;;
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

# check if the git branch is dirty, and exit if it is
check_git_branch

initial_branch=$(git branch --show-current)
new_branch=$(create_git_branch_list $stream-stream-bump $base_branch)
switch_git_branch $base_branch $new_branch

# Locate and store the WMCO version from the makefile 
# finds "WMCO_VERSION ?=" and selects text following it
WMCO_VERSION=$(sed -n -e 's/^.*WMCO_VERSION ?= //p' Makefile)
update_WMCO_version $stream $WMCO_VERSION
update_OCP_version $stream $WMCO_VERSION 
make bundle

commit_message="[release] Update $base_branch $stream stream to version
This commit was generated by hack/pre-release.sh"
generate_commit "$commit_message" build bundle Makefile config

# Return to the original branch 
git checkout $initial_branch

# Display the newly created branches with push instructions 
display_push_message "$new_branch"
