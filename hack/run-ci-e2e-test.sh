#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

MACHINE_NODE_COUNT_OPTION=""
BYOH_NODE_COUNT_OPTION=""
SKIP_NODE_DELETION=""
WMCO_PATH_OPTION=""

export CGO_ENABLED=0

get_WMCO_logs() {
  retries=0
  until [ $retries -gt 4 ] || oc logs -l name=windows-machine-config-operator -n $WMCO_DEPLOY_NAMESPACE --tail=-1 >> "$ARTIFACT_DIR"/wmco.log
  do
    let retries+=1
    echo "failed to get WMCO logs"
    sleep 5
  done
}

TEST="all"
while getopts ":m:c:b:st:" opt; do
  case ${opt} in
    m ) # number of instances to create and configure using the Machine controller
      MACHINE_NODE_COUNT_OPTION="--machine-node-count=$OPTARG"
      ;;
    c ) # number of instances to create and configure using the ConfigMap controller
      BYOH_NODE_COUNT_OPTION="--byoh-node-count=$OPTARG"
      ;;
    s ) # process option for skipping deleting Windows VMs created by test suite
      SKIP_NODE_DELETION="true"
      ;;
    b ) # path to the WMCO binary, used for version validation
      WMCO_PATH_OPTION="-wmco-path=$OPTARG"
      ;;
    t ) # test to run. Defaults to all. Other options are basic and upgrade.
      TEST=$OPTARG
      if [[ "$TEST" != "all" && "$TEST" != "basic" && "$TEST" != "upgrade" ]]; then
        echo "Invalid -t option $TEST. Valid options are all, basic or upgrade"
        exit 1
      fi
      ;;
    \? )
      echo "Usage: $0 [-m] [-c] [-s] [-b] [-t]"
      exit 0
      ;;
  esac
done

# KUBE_SSH_KEY_PATH needs to be set in order to create the cloud-private-key secret
if [ -z "$KUBE_SSH_KEY_PATH" ]; then
    echo "env KUBE_SSH_KEY_PATH not found"
    return 1
fi

if ! [[ "$OPENSHIFT_CI" == "true" &&  "$TEST" = "upgrade" ]]; then
  OSDK=$(get_operator_sdk)
fi

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
# In CI $OPERATOR_IMAGE environment variable is declared through a dependency in test references declared at
# https://github.com/openshift/release/tree/master/ci-operator/step-registry/windows/e2e/operator/test
if [[ -z "$OPERATOR_IMAGE" ]]; then
  error-exit "The OPERATOR_IMAGE environment variable was not found."
fi

# generate the WMCO binary if we are not running the test through CI. This binary is used to validate WMCO version
# while running the validation test. For CI, we use different `wmcoPath` based on how we generate the container image.
if [[ "$OPENSHIFT_CI" != "true" ]]; then
  make build
fi

# Setup and run the operator
# Spinning up a cluster is a long operation and operator deployment through this method has been prone to transient
# errors. Retrying WMCO deployment allows us to save time in CI.
if ! [[ "$OPENSHIFT_CI" = "true" &&  "$TEST" = "upgrade" ]]; then
  retries=0
  while ! run_WMCO $OSDK
  do
      if [[ $retries -eq 5 ]]; then
           echo "Max retries reached, exiting"
           # Try to get the WMCO logs if possible
           get_WMCO_logs
           cleanup_WMCO $OSDK
           exit 1
      fi
      cleanup_WMCO $OSDK
      echo "Failed to deploy operator, retrying"
      sleep 5
      retries+=1
  done
fi

# WINDOWS_INSTANCES_DATA holds the windows-instances ConfigMap data section
WINDOWS_INSTANCES_DATA=${WINDOWS_INSTANCES_DATA:-}
# Check WINDOWS_INSTANCES_DATA and create the windows-instances ConfigMap, if
# any. The ConfigMap creation must takes place after the operator's deployment
# process is complete, because the namespace is removed as part of the cleanup
# step (cleanup_WMCO) while retrying the deployment.
if [[ -n "$WINDOWS_INSTANCES_DATA" ]]; then
  createWindowsInstancesConfigMap "${WINDOWS_INSTANCES_DATA}" || {
    error-exit "error creating windows-instances ConfigMap with ${WINDOWS_INSTANCES_DATA}"
  }
  # read BYOH from ConfigMap
  BYOH_NODE_COUNT=$(getWindowsInstanceCountFromConfigMap) || {
    error-exit "error getting windows-instances ConfigMap"
  }
  # update BYOH node count option or default to 0
  BYOH_NODE_COUNT_OPTION="--byoh-node-count=${BYOH_NODE_COUNT:-0}"
  echo "updated ${BYOH_NODE_COUNT_OPTION}"
fi

# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)

# Test that the operator is running when the private key secret is not present
printf "\n####### Testing operator deployed without private key secret #######\n" >> "$ARTIFACT_DIR"/wmco.log
go test ./test/e2e/... -run=TestWMCO/operator_deployed_without_private_key_secret -v -args $BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION

# Run the creation tests of the Windows VMs
printf "\n####### Testing creation #######\n" >> "$ARTIFACT_DIR"/wmco.log
go test ./test/e2e/... -run=TestWMCO/create -v -timeout=90m -args $BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION
# Get logs for the creation tests
printf "\n####### WMCO logs for creation tests #######\n" >> "$ARTIFACT_DIR"/wmco.log
get_WMCO_logs

if [[ "$TEST" = "all" || "$TEST" = "basic" ]]; then
  # Run the network tests
  printf "\n####### Testing network #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/network -v -timeout=20m -args $BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION
fi

if [[ "$TEST" = "all" || "$TEST" = "upgrade" ]]; then
  # Run the upgrade tests and skip deletion of the Windows VMs
  printf "\n####### Testing upgrade #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/upgrade -v -timeout=90m -args $BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION

  # Run the reconfiguration test
  # The reconfiguration suite must be run directly before the deletion suite. This is because we do not
  # currently wait for nodes to fully reconcile after changing the private key back to the valid key. Any tests
  # added/moved in between these two suites may fail.
  # This limitation will be removed with https://issues.redhat.com/browse/WINC-655
  printf "\n####### Testing reconfiguration #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/reconfigure -v -timeout=90m -args $BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH $WMCO_PATH_OPTION
fi

# Run the deletion tests while testing operator restart functionality. This will clean up VMs created
# in the previous step
if ! $SKIP_NODE_DELETION; then
  go test ./test/e2e/... -run=TestWMCO/destroy -v -timeout=60m -args --private-key-path=$KUBE_SSH_KEY_PATH
  # Get logs on success before cleanup
  PRINT_UPGRADE=""
  if [[ "$TEST" = "upgrade" ]]; then
    PRINT_UPGRADE="upgrade and"
  fi
  printf "\n####### WMCO logs for %s deletion tests #######\n" "$PRINT_UPGRADE" >> "$ARTIFACT_DIR"/wmco.log
  get_WMCO_logs
  # Cleanup the operator resources
  if ! [[ "$OPENSHIFT_CI" = "true" &&  "$TEST" = "upgrade" ]]; then
    cleanup_WMCO $OSDK
  fi
else
  # Get logs on success
  printf "\n####### WMCO logs for upgrade tests #######\n" >> "$ARTIFACT_DIR"/wmco.log
  get_WMCO_logs
fi

exit 0
