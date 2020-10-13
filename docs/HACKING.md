⚠⚠⚠ THIS IS A LIVING DOCUMENT AND LIKELY TO CHANGE QUICKLY ⚠⚠⚠

# Hacking on the WMCO

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
cluster set up, we need to setup the following environment variables.
```shell script
export KUBECONFIG=<path to kubeconfig>

# SSH key to be used for creation of cloud-private-key secret 
export KUBE_SSH_KEY_PATH=<path to RSA type ssh key>

# on AWS only:
export AWS_SHARED_CREDENTIALS_FILE=<path to aws credentials file>
```

To run the operator on a cluster, use: 
```shell script
hack/olm.sh run -c "<OPERATOR_IMAGE>"
```
This command builds the operator image and pushes it to remote repository. Executing [Build](#build) step is not required. 

In order to build the operator ignoring the existing build image cache, run the above command with the `-i` option.

To clean-up the installation, use:
```shell script
hack/olm.sh cleanup
```

### Running e2e tests on a cluster
We need to set up all the environment variables required in [Development workflow](#development-workflow) as well as: 
```shell script
export OPERATOR_IMAGE=<registry url for remote WMCO image>
```
Once the above variables are set, run the following script:
```shell script
hack/run-ci-e2e-test.sh
```

Additional flags that can be passed to `hack/run-ci-e2e-test.sh` are
- `-s` to skip the deletion of Windows nodes that are created as part of test suite run
- `-n` to represent the number of Windows nodes to be created for test run
- `-b` gives an alternative path to the WMCO binary. This option overridden in OpenShift CI.
       When building the operator locally, the WMCO binary is created as part of the operator image build process and
       can be found at `build/_output/bin/windows-machine-config-operator`, this is the default value of this option.

       
Example command to spin up 2 Windows nodes and retain them after test run:
```
hack/run-ci-e2e-test.sh -s -n 2      
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
You can skip this step if you want to run the operator for [developer testing purposes only](#development-workflow)

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
You can skip this step if you want to run the operator for [developer testing purposes only](#development-workflow)

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