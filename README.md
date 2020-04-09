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
hack/run-ci-e2e-test.sh -k "openshift-dev"
```
We assume that the developer uses `openshift-dev` as the key pair in the aws cloud

Additional flags that can be passed to `hack/run-ci-e2e-test.sh` are
- `-s` to skip the deletion of Windows nodes that are created as part of test suite run
- `-n` to represent the number of Windows nodes to be created for test run
- `-k` to represent the AWS specific key pair that will be used during e2e run and it should map to the private key
       that we have in `KUBE_SSH_KEY_PATH`. The default value points to `libra` which we use in our CI
       
Example command to spin up 2 Windows nodes and retain them after test run:
```
hack/run-ci-e2e-test.sh -s -k "openshift-dev" -n 2      
```
        
## Bundling the Windows Machine Config Operator
This directory contains resources related to installing the WMCO onto a cluster using OLM.

### Pre-requisites
[opm](https://github.com/operator-framework/operator-registry/) has been installed on the localhost.
All previous [pre-requisites](#pre-requisites) must be satisfied as well.

### Generating a new bundle
This step should be done in the case that changes have been made to any of the yaml files in `deploy/`.

If changes need to be made to the bundle spec, a new bundle can be generated with:
```shell script
operator-sdk generate csv --csv-channel alpha --csv-version $NEW_VERSION --default-channel --operator-name windows-machine-config-operator --update-crds --from-version $PREV_VERSION
```

You should replace `$NEW_VERSION` and `$PREV_VERSION` with the new semver and the previous semver respectively.
This will create a new directory: `deploy/olm-catalog/windows-machine-config-operator/$NEW_VERSION`
You should use this new directory when [creating the bundle image](#creating-a-bundle-image)

### Creating a bundle image
A bundle image can be created by editing the CSV in deploy/bundle/windows-machine-config-operator/manifests/
and replacing `REPLACE_IMAGE` with the location of the WMCO operator image you wish to deploy.
See [the build instructions](#build) for more information on building the image.

You can then run the following command in the root of this git repository
```shell script
operator-sdk bundle create $BUNDLE_REPOSITORY:$VERSION_TAG -d deploy/olm-catalog/windows-machine-config-operator/0.0.0 \
--channels alpha --default-channel alpha --image-builder podman
```
The variables in the command should be changed to match the container image repository you wish to store the bundle in.
You can also change the channels based on the release status of the operator.

You should then push the image to the remote repository
```shell script
podman push $BUNDLE_REPOSITORY:$BUNDLE_VERSION
```

You should verify that the new bundle is valid:
```shell script
operator-sdk bundle validate $BUNDLE_REPOSITORY:$BUNDLE_VERSION --image-builder podman
```

### Creating a new operator index
An operator index is a collection of bundles. Creating one is required if you wish to deploy your operator on your own
cluster.

```shell script
opm index add --bundles $BUNDLE_REPOSITORY:$VERSION_TAG --tag $INDEX_VERSION
```

#### Editing an existing operator index
An existing operator index can have bundles added to it:
```shell script
opm index add --from-index $INDEX_VERSION
```
and removed from it:
```shell script
opm index rm --from-index $INDEX_VERSION
```

### Deploying the operator on a local cluster
#### Openshift Console
This deployment method is currently not supported. Please use the [CLI](#cli)

#### CLI
Change `deploy/olm-catalog/catalogsource.yaml` to point to the operator index created above. Now deploy it:
```shell script
oc apply -f deploy/olm-catalog/catalogsource.yaml
```

This will deploy a CatalogSource object in the `openshift-marketplace` namespace. You can check the status of it via:
```shell script
oc describe catalogsource wmco -n openshift-marketplace
```

Now wait 1-10 minutes for the catalogsource's `status.connectionState.lastObservedState` field to be set to READY.

Create the windows-machine-config-operator namespace:
```shell script
oc apply -f deploy/namespace.yaml
```

Switch to the windows-machine-config-operator project:
```shell script
oc project windows-machine-config-operator
```

Create the OperatorGroup for the namespace:
```shell script
oc apply -f deploy/olm-catalog/operatorgroup.yaml
```

Create the cloud provider and cloud private key secrets. The cloud-private-key should match the keypair used in the
Windows Machine Config CR you will use.
```shell script
# Change paths as necessary
oc create secret generic cloud-credentials --from-file=credentials=/home/$user/.aws/credentials
oc create secret generic cloud-private-key --from-file=private-key.pem=/home/$user/.ssh/$keyname
```

Put the kubeconfig you wish to use in a secret. In order to use the permissions of the windows-machine-config-operator
service account the kubeconfig should be generated from the service account.
```shell script
# Change paths as necessary
oc create secret generic kubeconfig --from-file=kubeconfig=/path/to/kubeconfig
```

Put the cluster address in a secret:
```shell script
CLUSTER_ADDR=$(oc cluster-info | head -n1 | sed 's/.*\/\/api.//g'| sed 's/:.*//g')
oc create secret generic cluster-address --from-literal=cluster-address=$CLUSTER_ADDR
```

Change `spec.startingCSV` in `deploy/olm-catalog/subscription.yaml` to match the version of the operator you wish to deploy.

Now create the subscription which will deploy the operator.
```shell script
oc apply -f deploy/olm-catalog/subscription.yaml
```
