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
if [[ ! "$ACTION" =~ ^build|run|cleanup$ ]]; then
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

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

cd $WMCO_ROOT
OSDK=$(get_operator_sdk)
VERSION=$(get_version)

# Builds the container image and pushes it to remote repository. Uses this built image to run the operator on the cluster.
# It is user's responsibility to clean old/unused containers in container repository as well as local system.
case "$ACTION" in
    build)
  build_WMCO $OSDK 

    ;;
    run)
  build_WMCO $OSDK

  # Setup and run the operator
  run_WMCO $OSDK

	;;
    cleanup)
  # Cleanup the operator resources
  cleanup_WMCO $OSDK

	;;
esac
