#!/bin/bash

# olm.sh - run/cleanup the operator with OLM
#
# USAGE
#    olm.sh run/cleanup -c OPERATOR_IMAGE
# OPTIONS
#    $1      Action                   run/cleanup the operator installation
#    -i      Ignore image cache       builds the operator image without using local image build cache
#    -c=     Operator Image           container url and tag for the operator image


# container tool to use with operator-sdk
CONTAINER_TOOL=podman

function error-exit() {
    echo "Error: $*" >&2
    exit 1
}

# specify the action. Either run or cleanup the operator
ACTION=$1
if [[ ! "$ACTION" =~ ^run|cleanup$ ]]; then
    error-exit "Action (1st parameter) must be \"run\" or \"cleanup\""
fi
shift # shift position of the positional parameters for getopts

# Options
while getopts ":ic:" opt; do
    case "$opt" in
	i) noCache="--image-build-args=\"--no-cache\"";;
	c) OPERATOR_IMAGE="$OPTARG";;
	?) error-exit "Unknown option"
    esac
done

if [ -z "$AWS_SHARED_CREDENTIALS_FILE" ]; then
    error-exit "env AWS_SHARED_CREDENTIALS_FILE not found"
fi

if [ -z "$KUBE_SSH_KEY_PATH" ]; then
    error-exit "env KUBE_SSH_KEY_PATH not found"
fi

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

cd $WMCO_ROOT
OSDK=$(get_operator_sdk)

# Builds the container image and pushes it to remote repository. Uses this built image to run the operator on the cluster.
# It is user's responsibility to clean old/unused containers in container repository as well as local system.
case "$ACTION" in
    run)
  if [ -z "$OPERATOR_IMAGE" ]; then
      error-exit "OPERATOR_IMAGE not set"
  fi

  $OSDK build "$OPERATOR_IMAGE" --image-builder $CONTAINER_TOOL $noCache
  if [ $? -ne 0 ] ; then
      error-exit "failed to build operator image"
  fi

  $CONTAINER_TOOL push "$OPERATOR_IMAGE"
  if [ $? -ne 0 ] ; then
      error-exit "failed to push operator image to remote repository"
  fi

  # Setup and run the operator
  run_WMCO $OSDK

	;;
    cleanup)
  # Cleanup the operator resources
  cleanup_WMCO $OSDK

	;;
esac
