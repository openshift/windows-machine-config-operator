#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

NODE_COUNT=""
SKIP_NODE_DELETION=""
KEY_PAIR_NAME=""

export CGO_ENABLED=0

get_WMCO_logs() {
  oc logs -l name=windows-machine-config-operator -n windows-machine-config-operator
}

# This function runs operator-sdk test with certain go test arguments
# Parameters:
# 1: path to the operator-sdk binary to use
# 2: go test arguments
OSDK_WMCO_test() {
  if [ $# != 2 ]; then
    echo incorrect parameter count for OSDK_WMCO_test
    return 1
  fi

  local OSDK_PATH=$1
  local TEST_FLAGS=$2

  if ! $OSDK_PATH test local ./test/e2e --no-setup --debug --operator-namespace=windows-machine-config-operator --go-test-flags "$TEST_FLAGS"; then
    get_WMCO_logs
    return 1
  fi
}

while getopts ":n:k:s" opt; do
  case ${opt} in
    n ) # process option for the node count
      NODE_COUNT=$OPTARG
      ;;
    k ) # process option for the keypair to be used by AWS cloud provider
      KEY_PAIR_NAME=$OPTARG
      ;;
    s ) # process option for skipping deleting Windows VMs created by test suite
      SKIP_NODE_DELETION="-skip-node-deletion"
      ;;
    \? )
      echo "Usage: $0 [-n] [-k] [-s]"
      exit 0
      ;;
  esac
done

OSDK=$(get_operator_sdk)

# Set default values for the flags. Without this operator-sdk flags are getting
# polluted. For example, if KEY_PAIR_NAME is not passed or passed as empty value
# the value is literally taken as "" instead of empty-string so default values we
# specified in main_test.go has literally no effect. Not sure, if this is because of
# the way operator-sdk testing is done using `go test []string{}
NODE_COUNT=${NODE_COUNT:-2}
SKIP_NODE_DELETION=${SKIP_NODE_DELETION:-"-skip-node-deletion=false"}
KEY_PAIR_NAME=${KEY_PAIR_NAME:-"openshift-dev"}

# OPERATOR_IMAGE defines where the WMCO image to test with is located. If $OPERATOR_IMAGE is already set, use its value.
# Setting $OPERATOR_IMAGE is required for local testing.
# In OpenShift CI $IMAGE_FORMAT will be set to a value such as registry.svc.ci.openshift.org/ci-op-<input-hash>/stable:${component}
# We need to replace ${component} with the name of the WMCO image.
# The stable namespace is used as it will use the last promoted version when the test is run in an repository other than this one.
# When running in this repository the image built in the pipeline will be used instead.
# https://steps.svc.ci.openshift.org/help/ci-operator#release
OPERATOR_IMAGE=${OPERATOR_IMAGE:-${IMAGE_FORMAT//\/stable:\$\{component\}//stable:windows-machine-config-operator-test}}

# Setup and run the operator
run_WMCO $OSDK

# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)
# Run the creation tests and skip deletion of the Windows VMs
OSDK_WMCO_test $OSDK "-run=TestWMCO/create -v -timeout=90m -node-count=$NODE_COUNT -skip-node-deletion -ssh-key-pair=$KEY_PAIR_NAME"

# Run the deletion tests while testing operator restart functionality. This will clean up VMs created
# in the previous step
OSDK_WMCO_test $OSDK "-run=TestWMCO/destroy -v -timeout=60m -ssh-key-pair=$KEY_PAIR_NAME"

# Get logs on success before cleanup
get_WMCO_logs

# Cleanup the operator resources
cleanup_WMCO $OSDK

exit 0