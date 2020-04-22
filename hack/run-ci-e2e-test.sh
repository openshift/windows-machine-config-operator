#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# Set up hybrid networking on the cluster, a requirement for Windows support in OpenShift
# TODO: This needs to be removed as part of https://issues.redhat.com/browse/WINC-351
oc patch network.operator cluster --type=merge -p '{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig":{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}]}}}}}'

NODE_COUNT=""
SKIP_NODE_DELETION=""
KEY_PAIR_NAME=""

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..

export CGO_ENABLED=0
export CLUSTER_ADDR=$(oc cluster-info | head -n1 | sed 's/.*\/\/api.//g'| sed 's/:.*//g')

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

# Download the operator-sdk binary only if it is not already available
# We do not validate the version of operator-sdk if it is available already
if ! type operator-sdk > /dev/null; then
  # TODO: Make this download the same version we have in go dependencies in gomod
  wget -O /tmp/operator-sdk https://github.com/operator-framework/operator-sdk/releases/download/v0.15.2/operator-sdk-v0.15.2-x86_64-linux-gnu && chmod +x /tmp/operator-sdk
  # To expand alias. We'll add an alias for operator-sdk to be in `/tmp/operator-sdk`
  shopt -s expand_aliases
  alias operator-sdk=/tmp/operator-sdk
fi

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
# The bool flags in golang does not respect key value pattern. They follow -flag=x pattern.
# -flag x is allowed for non-boolean flags only(https://golang.org/pkg/flag/)
operator-sdk test local ./test/e2e --debug --up-local --namespace=windows-machine-config-operator --local-operator-flags "--zap-level=debug --zap-encoder=console" --go-test-flags "-v -timeout=60m -node-count=$NODE_COUNT $SKIP_NODE_DELETION -ssh-key-pair=$KEY_PAIR_NAME"
oc delete -f deploy/namespace.yaml 

exit 0
