#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

NODE_COUNT=""
SKIP_NODE_DELETION=""
WMCO_PATH_OPTION=""

export CGO_ENABLED=0

get_WMCO_logs() {
  oc logs -l name=windows-machine-config-operator -n openshift-windows-machine-config-operator --tail=-1
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

  if ! $OSDK_PATH test local ./test/e2e --no-setup --debug --operator-namespace=openshift-windows-machine-config-operator --go-test-flags "$TEST_FLAGS"; then
    get_WMCO_logs
    return 1
  fi
}

while getopts ":n:k:b:s" opt; do
  case ${opt} in
    n ) # process option for the node count
      NODE_COUNT=$OPTARG
      ;;
    s ) # process option for skipping deleting Windows VMs created by test suite
      SKIP_NODE_DELETION="true"
      ;;
    b ) # path to the WMCO binary, used for version validation
      WMCO_PATH_OPTION="-wmco-path=$OPTARG"
      ;;
    \? )
      echo "Usage: $0 [-n] [-k] [-s] [-b]"
      exit 0
      ;;
  esac
done

# KUBE_SSH_KEY_PATH needs to be set in order to create the cloud-private-key secret
if [ -z "$KUBE_SSH_KEY_PATH" ]; then
    echo "env KUBE_SSH_KEY_PATH not found"
    return 1
fi

OSDK=$(get_operator_sdk)

# Set default values for the flags. Without this operator-sdk flags are getting
# polluted, i.e. if a flag is not passed or passed as empty value
# the value is literally taken as "" instead of empty-string so default values we
# specified in main_test.go has literally no effect. Not sure, if this is because of
# the way operator-sdk testing is done using `go test []string{}
NODE_COUNT=${NODE_COUNT:-2}
SKIP_NODE_DELETION=${SKIP_NODE_DELETION:-"false"}

# If ARTIFACT_DIR is not set, create a temp directory for artifacts
ARTIFACT_DIR=${ARTIFACT_DIR:-}
if [ -z "$ARTIFACT_DIR" ]; then
  ARTIFACT_DIR=`mktemp -d`
  echo "ARTIFACT_DIR is not set. Artifacts will be stored in: $ARTIFACT_DIR"
  export ARTIFACT_DIR=$ARTIFACT_DIR
fi

# OPERATOR_IMAGE defines where the WMCO image to test with is located. If $OPERATOR_IMAGE is already set, use its value.
# Setting $OPERATOR_IMAGE is required for local testing.
# In OpenShift CI $IMAGE_FORMAT will be set to a value such as registry.svc.ci.openshift.org/ci-op-<input-hash>/stable:${component}
# We need to replace ${component} with the name of the WMCO image.
# The stable namespace is used as it will use the last promoted version when the test is run in an repository other than this one.
# When running in this repository the image built in the pipeline will be used instead.
# https://steps.svc.ci.openshift.org/help/ci-operator#release
OPERATOR_IMAGE=${OPERATOR_IMAGE:-${IMAGE_FORMAT//\/stable:\$\{component\}//stable:windows-machine-config-operator-test}}

# Setup and run the operator
if ! run_WMCO $OSDK; then
  # Try to get the WMCO logs if possible
  get_WMCO_logs
  cleanup_WMCO $OSDK
  exit 1
fi

# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)

# Test that the operator is running when the private key secret is not present
OSDK_WMCO_test $OSDK "-run=TestWMCO/operator_deployed_without_private_key_secret -v -node-count=$NODE_COUNT --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION"

# Run the creation tests of the Windows VMs
OSDK_WMCO_test $OSDK "-run=TestWMCO/create -v -timeout=90m -node-count=$NODE_COUNT --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION"
# Get logs for the creation tests
printf "\n####### WMCO logs for creation tests #######\n"
get_WMCO_logs

# Run the upgrade tests and skip deletion of the Windows VMs
OSDK_WMCO_test $OSDK "-run=TestWMCO/upgrade -v -timeout=90m -node-count=$NODE_COUNT --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION"

# Run the deletion tests while testing operator restart functionality. This will clean up VMs created
# in the previous step
if ! $SKIP_NODE_DELETION; then
  OSDK_WMCO_test $OSDK "-run=TestWMCO/destroy -v -timeout=60m --private-key-path=$KUBE_SSH_KEY_PATH"
  # Get logs on success before cleanup
  printf "\n####### WMCO logs for upgrade and deletion tests #######\n"
  get_WMCO_logs
  # Cleanup the operator resources
  cleanup_WMCO $OSDK
else
  # Get logs on success
  printf "\n####### WMCO logs for upgrade tests #######\n"
  get_WMCO_logs
fi

exit 0
