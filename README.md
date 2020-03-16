# Windows Machine Config Operator

## Pre-requisites
- [Install](https://github.com/operator-framework/operator-sdk/blob/v0.15.x/doc/user/install-operator-sdk.md) operator-sdk
  v0.15.2
- The operator is written using operator-sdk [v0.15.2](https://github.com/operator-framework/operator-sdk/releases/tag/v0.15.2)
  and has the same [pre-requisites](https://github.com/operator-framework/operator-sdk/tree/v0.15.x#prerequisites) as it
  does.

## Build
To build the operator image, execute:
```shell script
operator-sdk build quay.io/<insert username>/wmco:latest
```

## Testing locally
To run the e2e tests for WMCO locally against an OpenShift cluster set up on AWS, we need to setup the following environment variables.
```shell script
export KUBECONFIG=<path to kubeconfig>
export AWS_SHARED_CREDENTIALS_FILE=<path to aws credentials file>
export CLUSTER_ADDR=<cluster_name, eg: ravig211.devcluster.openshift.com>
export KUBE_SSH_KEY_PATH=<path to ssh key>
```
Once the above variables are set:
```shell script
hack/run-ci-e2e-test.sh
```

