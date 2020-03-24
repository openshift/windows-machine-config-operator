#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# Set up hybrid networking on the cluster, a requirement for Windows support in OpenShift
# TODO: This needs to be removed as part of https://issues.redhat.com/browse/WINC-351
oc patch network.operator cluster --type=merge -p '{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig":{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}]}}}}}'

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..

export CGO_ENABLED=0
export CLUSTER_ADDR=$(oc cluster-info | head -n1 | sed 's/.*\/\/api.//g'| sed 's/:.*//g')

# Copy the cloud credentials and KUBESSH key path so that they can be used by operator
cp $AWS_SHARED_CREDENTIALS_FILE /etc/cloud/credentials
cp $KUBE_SSH_KEY_PATH /etc/private-key/private-key.pem

# Get operator-sdk binary.
# TODO: Make this to download the same version we have in go dependencies in gomod
wget -O /tmp/operator-sdk https://github.com/operator-framework/operator-sdk/releases/download/v0.15.2/operator-sdk-v0.15.2-x86_64-linux-gnu && chmod +x /tmp/operator-sdk
cd $WMCO_ROOT
oc create -f deploy/namespace.yaml 
/tmp/operator-sdk test local ./test/e2e --up-local --namespace=windows-machine-config-operator --go-test-flags "-v -timeout 60m"
oc delete -f deploy/namespace.yaml 

exit 0
