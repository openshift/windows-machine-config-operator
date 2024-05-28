#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# This script depends on the oc client. In CI, the oc cli is injected according
# to the given OCP version. See https://github.com/openshift/release/pull/26395
# ensure oc client is available
which oc || {
  # fails otherwise
  echo "cannot find oc binary in PATH"
  exit 1
}
# print oc client version
oc version

# If ARTIFACT_DIR is not set, create a temp directory for artifacts
ARTIFACT_DIR=${ARTIFACT_DIR:-}
if [ -z "$ARTIFACT_DIR" ]; then
  ARTIFACT_DIR=`mktemp -d`
  echo "ARTIFACT_DIR is not set. Artifacts will be stored in: $ARTIFACT_DIR"
  export ARTIFACT_DIR=$ARTIFACT_DIR
fi

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

MACHINE_NODE_COUNT_OPTION=""
BYOH_NODE_COUNT_OPTION=""
SKIP_NODE_DELETION=""

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
# WINDOWS_SERVER_VERSION will be set in the CI config. If it is not set, default to 2022.
WIN_VER=${WINDOWS_SERVER_VERSION:-"2022"}

while getopts ":m:c:b:st:w:" opt; do
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
    t ) # test to run. Defaults to all. Other options are basic and upgrade.
      TEST=$OPTARG
      if [[ "$TEST" != "all" && "$TEST" != "basic" && "$TEST" != "upgrade" && "$TEST" != "upgrade-setup" && "$TEST" != "upgrade-test" ]]; then
        echo "Invalid -t option $TEST. Valid options are all, basic, upgrade, upgrade-setup, and upgrade-test"
        exit 1
      fi
      ;;
    w ) # Windows Server version to test against. Defaults to 2022. Other option is 2019.
      WIN_VER=$OPTARG
      ;;
    \? )
      echo "Usage: $0 [-m] [-c] [-s] [-b] [-t] [-w]"
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

# Setup and run the operator if it is not already deployed
wmco_deployed_by_script=false
if  ! oc get deploy/windows-machine-config-operator -n $WMCO_DEPLOY_NAMESPACE > /dev/null; then
  # OPERATOR_IMAGE defines where the WMCO image to test with is located. If $OPERATOR_IMAGE is already set, use its value.
  # Setting $OPERATOR_IMAGE is required for local testing.
  # In CI $OPERATOR_IMAGE environment variable is declared through a dependency in test references declared at
  # https://github.com/openshift/release/tree/master/ci-operator/step-registry/windows/e2e/operator/test
  if [[ -z "$OPERATOR_IMAGE" ]]; then
    error-exit "The OPERATOR_IMAGE environment variable was not found."
  fi

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
  wmco_deployed_by_script=true
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

echo "Testing against Windows Server $WIN_VER"

# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)

GO_TEST_ARGS="$BYOH_NODE_COUNT_OPTION $MACHINE_NODE_COUNT_OPTION --private-key-path=$KUBE_SSH_KEY_PATH --wmco-namespace=$WMCO_DEPLOY_NAMESPACE --windows-server-version=$WIN_VER"
# Test that the operator is running when the private key secret is not present
printf "\n####### Testing operator deployed without private key secret #######\n" >> "$ARTIFACT_DIR"/wmco.log
go test ./test/e2e/... -run=TestWMCO/operator_deployed_without_private_key_secret -v -args $GO_TEST_ARGS

if [[ "$TEST" != "upgrade-setup" && "$TEST" != "upgrade-test" ]]; then
  # Run the creation tests of the Windows VMs
  printf "\n####### Testing creation #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/create -v -timeout=90m -args $GO_TEST_ARGS
  # Get logs for the creation tests
  printf "\n####### WMCO logs for creation tests #######\n" >> "$ARTIFACT_DIR"/wmco.log
  get_WMCO_logs
fi

if [[ "$TEST" = "all" || "$TEST" = "basic" ]]; then
  printf "\n####### Testing image mirroring #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/image_mirroring -v -timeout=10m -args $GO_TEST_ARGS
  printf "\n####### Testing network #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/network -v -timeout=20m -args $GO_TEST_ARGS
  printf "\n####### Testing storage #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/storage -v -timeout=10m -args $GO_TEST_ARGS
  printf "\n####### Testing service reconciliation #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/service_reconciliation -v -timeout=20m -args $GO_TEST_ARGS
  printf "\n####### Testing cluster-wide proxy #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/cluster-wide_proxy -v -timeout=20m -args $GO_TEST_ARGS
fi

if [[ "$TEST" = "all" || "$TEST" = "upgrade" ]]; then
  # Run the upgrade tests and skip deletion of the Windows VMs
  printf "\n####### Testing upgrade #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/upgrade -v -timeout=90m -args $GO_TEST_ARGS

  # Run the reconfiguration test
  printf "\n####### Testing reconfiguration #######\n" >> "$ARTIFACT_DIR"/wmco.log
  go test ./test/e2e/... -run=TestWMCO/reconfigure -v -timeout=90m -args $GO_TEST_ARGS
fi

if [[ "$TEST" = "upgrade-setup" ]]; then
  go test ./test/e2e/... -run=TestWMCO/create/Creation -v -timeout=90m -args $GO_TEST_ARGS
  go test ./test/e2e/... -run=TestWMCO/create/Nodes_ready_and_schedulable -v -timeout=90m -args $GO_TEST_ARGS
  # Run the storage test, skipping deletion of the created workload in order to test that it persists across the upgrade
  go test ./test/e2e/... -run=TestWMCO/storage -v -timeout=15m -args $GO_TEST_ARGS --skip-workload-deletion=true
  go test ./test/e2e/... -run=TestWMCO/create/Node_Logs -v -timeout=10m -args $GO_TEST_ARGS
  createParallelUpgradeCheckerResources
fi

if [[ "$TEST" = "upgrade-test" ]]; then
  trap deleteParallelUpgradeCheckerResources EXIT
  go test ./test/e2e/... -run=TestUpgrade -v -timeout=30m -args $GO_TEST_ARGS
fi


# Run the deletion tests while testing operator restart functionality. This will clean up VMs created
# in the previous step
if ! $SKIP_NODE_DELETION; then
  go test ./test/e2e/... -run=TestWMCO/destroy -v -timeout=60m -args $GO_TEST_ARGS
  # Get logs on success before cleanup
  PRINT_UPGRADE=""
  if [[ "$TEST" = "upgrade" ]]; then
    PRINT_UPGRADE="upgrade and"
  fi
  printf "\n####### WMCO logs for %s deletion tests #######\n" "$PRINT_UPGRADE" >> "$ARTIFACT_DIR"/wmco.log
  get_WMCO_logs
  # Cleanup the operator resources
  if [ "$wmco_deployed_by_script" = "true" ]; then
    cleanup_WMCO $OSDK
  fi
else
  # Get logs on success
  printf "\n####### WMCO logs for upgrade tests #######\n" >> "$ARTIFACT_DIR"/wmco.log
  get_WMCO_logs
fi

exit 0
