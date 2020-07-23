# Windows Machine Config Operator

## Pre-requisites
- [Install](https://sdk.operatorframework.io/docs/install-operator-sdk/) operator-sdk
  v0.18.1
- The operator is written using operator-sdk [v0.18.1](https://github.com/operator-framework/operator-sdk/releases/tag/v0.18.1)
  and has the same [pre-requisites](https://sdk.operatorframework.io/docs/install-operator-sdk/#prerequisites) as it
  does.
- Instructions assume that the user is using [Podman](https://podman.io/) container engine.

## Build
To build the operator image, execute:
```shell script
operator-sdk build quay.io/<insert username>/wmco:$VERSION_TAG --image-builder podman
```

The operator image needs to be pushed to a remote repository:
```shell script
podman push quay.io/<insert username>/wmco:$VERSION_TAG
```
## Development workflow
To run the operator on a cluster (for testing purpose only) or to run the e2e tests for WMCO against an OpenShift 
cluster set up on AWS, we need to setup the following environment variables.
```shell script
export KUBECONFIG=<path to kubeconfig>
export AWS_SHARED_CREDENTIALS_FILE=<path to aws credentials file>
export KUBE_SSH_KEY_PATH=<path to RSA type ssh key>
```

To run the operator on a cluster, use: 
```shell script
hack/olm.sh run -c "<OPERATOR_IMAGE>"
```
This command builds the operator image and pushes it to remote repository. Executing [Build](#build) step is not required. 

Inorder to build the operator ignoring the existing build image cache, run the above command with the `-i` option.

To clean-up the installation, use:
```shell script
hack/olm.sh cleanup
```

*Operator-sdk has a known bug while using `operator-sdk run/cleanup packagemanifests` where it shows failure on success. 
Track the issue [here](https://github.com/operator-framework/operator-sdk/issues/2938). The error does not imply that the operator will not work.*

### Running e2e tests on a cluster
We need to set up all the environment variables required in [Development workflow](#development-workflow) as well as: 
```shell script
export OPERATOR_IMAGE=<registry url for remote WMCO image>
```
Once the above variables are set, run the following script:
```shell script
hack/run-ci-e2e-test.sh -k "openshift-dev"
```
We assume that the developer uses `openshift-dev` as the key pair in the aws cloud

Additional flags that can be passed to `hack/run-ci-e2e-test.sh` are
- `-s` to skip the deletion of Windows nodes that are created as part of test suite run
- `-n` to represent the number of Windows nodes to be created for test run
- `-k` to represent the AWS specific key pair that will be used during e2e run and it should map to the private key
       that we have in `KUBE_SSH_KEY_PATH`. The default value points to `openshift-dev` which we use in our CI
       
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
operator-sdk generate csv --csv-version $NEW_VERSION --operator-name windows-machine-config-operator
```

You should replace `$NEW_VERSION` with the new semver.

Example: For CSV version 0.0.1, the command should be:
```shell script
operator-sdk generate csv --csv-version 0.0.1 --operator-name windows-machine-config-operator
``` 
This will update the manifests in directory: `deploy/olm-catalog/windows-machine-config-operator/manifests`
This directory will be used while [creating the bundle image](#creating-a-bundle-image)

After generating bundle, you need to update metadata as well. 
```shell script
operator-sdk bundle create --generate-only --channels alpha --default-channel alpha
```

### Creating a bundle image
You can skip this step if you want to run the operator locally [without bundle and index images](#running-without-bundle-and-index-images)

A bundle image can be created by editing the CSV in `deploy/olm-catalog/windows-machine-config-operator/manifests/`
and replacing `REPLACE_IMAGE` with the location of the WMCO operator image you wish to deploy.
See [the build instructions](#build) for more information on building the image.

You can then run the following command in the root of this git repository:
```shell script
operator-sdk bundle create $BUNDLE_REPOSITORY:$BUNDLE_TAG -d deploy/olm-catalog/windows-machine-config-operator/manifests \
--channels alpha --default-channel alpha --image-builder podman
```
The variables in the command should be changed to match the container image repository you wish to store the bundle in.
You can also change the channels based on the release status of the operator.
This command should create a new bundle image. Bundle image and operator image are two different images. 

You should then push the newly created bundle image to the remote repository:
```shell script
podman push $BUNDLE_REPOSITORY:$BUNDLE_TAG
```

You should verify that the new bundle is valid:
```shell script
operator-sdk bundle validate $BUNDLE_REPOSITORY:$BUNDLE_TAG --image-builder podman
```

### Creating a new operator index
You can skip this step if you want to run the operator locally [without bundle and index images](#running-without-bundle-and-index-images)

An operator index is a collection of bundles. Creating one is required if you wish to deploy your operator on your own
cluster.

```shell script
opm index add --bundles $BUNDLE_REPOSITORY:$BUNDLE_TAG --tag $INDEX_REPOSITORY:$INDEX_TAG --container-tool podman
```

You should then push the newly created index image to the remote repository:
```shell script
podman push $INDEX_REPOSITORY:$INDEX_TAG
```

#### Editing an existing operator index
An existing operator index can have bundles added to it:
```shell script
opm index add --from-index $INDEX_REPOSITORY:$INDEX_TAG
```
and removed from it:
```shell script
opm index rm --from-index $INDEX_REPOSITORY:$INDEX_TAG
```

### Deploying the operator on a local cluster
#### Openshift Console
This deployment method is currently not supported. Please use the [CLI](#cli)

#### CLI

Create the windows-machine-config-operator namespace:
```shell script
oc apply -f deploy/namespace.yaml
```

Switch to the windows-machine-config-operator project:
```shell script
oc project windows-machine-config-operator
```

##### Create Secrets
In order to run the operator locally, you need to create secrets before deploying the operator.

Create the cloud provider and cloud private key secrets. The cloud-private-key should match the keypair used in the
Windows Machine Config CR you will use and it should be of RSA key type.
```shell script
# Change paths as necessary
oc create secret generic cloud-credentials --from-file=credentials=$HOME/.aws/credentials
oc create secret generic cloud-private-key --from-file=private-key.pem=$HOME/.ssh/$keyname
```

##### Running with bundle and index images
You can skip this step if you want to run the operator locally [without bundle and index images](#running-without-bundle-and-index-images)

Change `deploy/olm-catalog/catalogsource.yaml` to point to the operator index created [above](#creating-a-new-operator-index). Now deploy it:
```shell script
oc apply -f deploy/olm-catalog/catalogsource.yaml
```

This will deploy a CatalogSource object in the `openshift-marketplace` namespace. You can check the status of it via:
```shell script
oc describe catalogsource wmco -n openshift-marketplace
```

Now wait 1-10 minutes for the catalogsource's `status.connectionState.lastObservedState` field to be set to READY.

Create the OperatorGroup for the namespace:
```shell script
oc apply -f deploy/olm-catalog/operatorgroup.yaml
```

Change `spec.startingCSV` in `deploy/olm-catalog/subscription.yaml` to match the version of the operator you wish to deploy.

Now create the subscription which will deploy the operator.
```shell script
oc apply -f deploy/olm-catalog/subscription.yaml
```
