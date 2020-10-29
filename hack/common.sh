#!/bin/bash

# Location of the manifests file
MANIFEST_LOC=deploy/olm-catalog/windows-machine-config-operator

get_operator_sdk() {
  # Download the operator-sdk binary only if it is not already available
  # We do not validate the version of operator-sdk if it is available already
  if type operator-sdk >/dev/null 2>&1; then
    which operator-sdk
    return
  fi

  DOWNLOAD_DIR=/tmp/operator-sdk
  # TODO: Make this download the same version we have in go dependencies in gomod
  wget --no-verbose -O $DOWNLOAD_DIR https://github.com/operator-framework/operator-sdk/releases/download/v0.19.4/operator-sdk-v0.19.4-x86_64-linux-gnu && chmod +x /tmp/operator-sdk || return
  echo $DOWNLOAD_DIR
}

# This function runs operator-sdk run --olm/cleanup depending on the given parameters
# Parameters:
# 1: command to run [run/cleanup]
# 2: path to the operator-sdk binary to use
# 3: OPTIONAL path to the directory holding the temporary CSV with image field replaced with operator image
OSDK_WMCO_management() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for OSDK_WMCO_management $#
    return 1
  fi
  if [[ "$1" != "run" && "$1" != "cleanup" ]]; then
    echo $1 does not match either run or cleanup
    return 1
  fi

  local COMMAND=$1
  local OSDK_PATH=$2

  # Currently this fails even on successes, adding this check to ignore the failure
  # https://github.com/operator-framework/operator-sdk/issues/2938
  if ! $OSDK_PATH $COMMAND packagemanifests --olm-namespace openshift-operator-lifecycle-manager --operator-namespace openshift-windows-machine-config-operator \
  --operator-version 0.0.0 ; then
    echo operator-sdk $1 failed
  fi
}

build_WMCO() {
  local OSDK=$1
  
  if [ -z "$OPERATOR_IMAGE" ]; then
      error-exit "OPERATOR_IMAGE not set"
  fi

  $OSDK build "$OPERATOR_IMAGE" --image-builder $CONTAINER_TOOL $noCache \
    --go-build-args "-ldflags -X=github.com/openshift/windows-machine-config-operator/version.Version=${VERSION}"
  if [ $? -ne 0 ] ; then
      error-exit "failed to build operator image"
  fi

  $CONTAINER_TOOL push "$OPERATOR_IMAGE"
  if [ $? -ne 0 ] ; then
      error-exit "failed to push operator image to remote repository"
  fi
}

# Updates the manifest file with the operator image, prepares the cluster to run the operator and
# runs the operator on the cluster using OLM
# Parameters:
# 1: path to the operator-sdk binary to use
run_WMCO() {
  local OSDK=$1

  transform_csv REPLACE_IMAGE $OPERATOR_IMAGE

  # Validate the operator bundle manifests
  $OSDK bundle validate $MANIFEST_LOC
  if [ $? -ne 0 ] ; then
      error-exit "operator bundle validation failed"
  fi

  oc apply -f deploy/namespace.yaml
  # Run the operator in the openshift-windows-machine-config-operator namespace
  OSDK_WMCO_management run $OSDK

  # Additional guard that ensures that operator was deployed given the SDK flakes in error reporting
  if ! oc rollout status deployment windows-machine-config-operator -n openshift-windows-machine-config-operator --timeout=5s; then
    return 1
  fi
}

# Reverts the changes made in manifests file and cleans up the installation of operator from the cluster and deletes the namespace
# Parameters:
# 1: path to the operator-sdk binary to use
cleanup_WMCO() {
  local OSDK=$1

  # Cleanup the operator and revert changes made to the csv
  if ! OSDK_WMCO_management cleanup $OSDK; then
      transform_csv $OPERATOR_IMAGE REPLACE_IMAGE
      error-exit "operator cleanup failed"
  fi
  transform_csv $OPERATOR_IMAGE REPLACE_IMAGE

  # Remove the operator from openshift-windows-machine-config-operator namespace
  oc delete -f deploy/namespace.yaml
}

# returns the operator version in `Version+GitHash` format
get_version() {
  OPERATOR_VERSION=0.0.1
  GIT_COMMIT=$(git rev-parse --short HEAD)
  VERSION="${OPERATOR_VERSION}+${GIT_COMMIT}"

  if [ -n "$(git status --porcelain)" ]; then
    VERSION="${VERSION}-dirty"
  fi

  echo $VERSION
}

# Given two parameters, replaces the value in first parameter with the second in the csv.
# Parameters:
# 1: parameter to determine value to be replaced in the csv
# 2: parameter with new value to be replaced with in the csv
transform_csv() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for replace_csv_value $#
    return 1
  fi
  sed -i "s|"$1"|"$2"|g" $MANIFEST_LOC/manifests/windows-machine-config-operator.clusterserviceversion.yaml
}
