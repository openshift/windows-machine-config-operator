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
- Update the keypair mentioned at [sshKeyPair](https://github.com/openshift/windows-machine-config-operator/blob/42593c5d2eb798b572c58b5debafc4c392d1f967/test/e2e/wmco_test.go#L59) to match with what 
is being used in KUBE_SSH_KEY_PATH. Please note that we're using libra as keypair
for our CI purposes.
- Ensure that /payload directory exists and is accessible by the user account. The directory needs to be populated with the following files. Please see the [Dockerfile](https://github.com/openshift/windows-machine-config-operator/blob/master/build/Dockerfile) for figuring where to download and build these binaries. It is up to the user to keep these files up to date.
```
/payload/
├── cni-plugins
│   ├── flannel.exe
│   ├── host-local.exe
│   ├── win-bridge.exe
│   └── win-overlay.exe
├── hybrid-overlay.exe
├── kube-node
│   ├── kubelet.exe
│   └── kube-proxy.exe
├── powershell
│   └── wget-ignore-cert.ps1
└── wmcb.exe
```
Once the above variables are set and the /payload directory has been populated, run the following script:
```shell script
hack/run-ci-e2e-test.sh
```

