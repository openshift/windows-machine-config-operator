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

# Copy the cloud credentials and KUBESSH key path so that they can be used by operator
cp $AWS_SHARED_CREDENTIALS_FILE /etc/cloud/credentials
cp $KUBE_SSH_KEY_PATH /etc/private-key/private-key.pem

OSDK=$(get_operator_sdk)

# Set default values for the flags. Without this operator-sdk flags are getting
# polluted. For example, if KEY_PAIR_NAME is not passed or passed as empty value
# the value is literally taken as "" instead of empty-string so default values we
# specified in main_test.go has literally no effect. Not sure, if this is because of
# the way operator-sdk testing is done using `go test []string{}
NODE_COUNT=${NODE_COUNT:-1}
SKIP_NODE_DELETION=${SKIP_NODE_DELETION:-"-skip-node-deletion=false"}
KEY_PAIR_NAME=${KEY_PAIR_NAME:-"libra"}

cd $WMCO_ROOT
oc create -f deploy/namespace.yaml
sleep 60m
# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)
$OSDK test local ./test/e2e --debug --up-local --operator-namespace=windows-machine-config-operator --local-operator-flags "--zap-level=debug --zap-encoder=console" --go-test-flags "-v -timeout=60m -node-count=$NODE_COUNT $SKIP_NODE_DELETION -ssh-key-pair=$KEY_PAIR_NAME"
oc delete -f deploy/namespace.yaml 

exit 0
