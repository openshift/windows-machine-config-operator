⚠⚠⚠ THIS IS A LIVING DOCUMENT AND LIKELY TO CHANGE QUICKLY ⚠⚠⚠

# Hacking on the WMCO
## Pre-requisites
- [Cluster and OS pre-requisites](wmco-prerequisites.md)
- [Install](https://sdk.operatorframework.io/docs/installation/) operator-sdk v1.15.0
- The operator is written using operator-sdk [v1.15.0](https://github.com/operator-framework/operator-sdk/releases/tag/v1.15.0)
  and has the same [pre-requisites](https://sdk.operatorframework.io/docs/installation/#prerequisites) as it does.
- Git submodules need to be initialized and up to date before running the build command. Please refer to
  [updating Git submodules](#updating-git-submodules).
- Verify push/pull access to a container registry from where the cluster can pull image from.

## User Workflows

### I want to install an officially released version of WMCO on my OpenShift/OKD cluster
* Instructions to install the community operator can be found in the [README](/README.md).
  Customers with a Red Hat subscription should instead look at the [OpenShift docs.](https://docs.openshift.com/container-platform/latest/windows_containers/enabling-windows-container-workloads.html)

### I want to try the unreleased WMCO
* [Build the operator](#build)
* [Deploy the operator directly](#deploying-the-operator-directly)

### I want to try the unreleased WMCO, and deploy it using OLM
* [Build the operator](#build)
* [Deploy the operator through OLM](#deploying-the-operator-through-olm)

### I want to try the unreleased WMCO, and deploy it mimicking the official deployment method
* [Build the operator](#build)
* [Generate bundle manifests](#generating-new-bundle-manifests)
* [Build the bundle image](#creating-a-bundle-image)
* [Build an operator index](#creating-a-new-operator-index)
* [Create a CatalogSource using the index](https://docs.openshift.com/container-platform/latest/operators/admin/olm-managing-custom-catalogs.html#olm-creating-catalog-from-index_olm-managing-custom-catalogs)
* [Follow the normal procedure to deploy an operator](https://docs.openshift.com/container-platform/latest/operators/admin/olm-adding-operators-to-cluster.html)

## Build
To build and push the operator image, execute:
```shell script
export OPERATOR_IMAGE=quay.io/<insert username>/wmco:$VERSION_TAG
hack/olm.sh build
```

Building the full WMCO image is a time consuming process given we are building all the submodules. If you are just
changing WMCO and not the submodules, you can separately build the submodules as a base image and then just build the
WMCO  image from this base image:
```shell script
make base-img
make wmco-img IMG=$OPERATOR_IMAGE
```
`make base-img` will not push the base image as a push is not needed. `make wmco-img` will make and push the
`$OPERATOR_IMAGE`

## Development workflow
With the Operator image built and pushed to a remote repository, the operator can be deployed either directly,
or through OLM.

For each deployment method a kubeconfig for an OpenShift or OKD cluster must be exported
```shell script
export KUBECONFIG=<path to kubeconfig>
```

### Deploying the operator directly
```shell script
make deploy IMG=$OPERATOR_IMAGE
```
The above command will directly deploy all the resources required for the operator to run, as well as the WMCO pod.
To remove the installed resources:
```shell script
make undeploy
```

### Deploying the operator through OLM
```shell script
hack/olm.sh run -k "<PRIVATE_KEY.PEM>"
```
Where `"<PRIVATE_KEY.PEM>"` is the private key which will be used to configure Windows nodes.
This command builds the operator image, pushes it to remote repository and uses OLM to launch the operator. Executing
the [Build](#build) step is not required.

In order to build the operator ignoring the existing build image cache, run the above command with the `-i` option.

To clean-up the installation, use:
```shell script
hack/olm.sh cleanup
```

### Running e2e tests on a cluster
The following environment variables must be set:
```shell script
# SSH key to be used for creation of cloud-private-key secret
export KUBE_SSH_KEY_PATH=<path to RSA type ssh key>
export OPERATOR_IMAGE=quay.io/<insert username>/wmco:$VERSION_TAG
export OPENSHIFT_CI=false
# on AWS only:
export AWS_SHARED_CREDENTIALS_FILE=<path to aws credentials file>
```
Once the above variables are set, run the following script:
```shell script
hack/run-ci-e2e-test.sh
```

Additional flags that can be passed to `hack/run-ci-e2e-test.sh` are
- `-s` to skip the deletion of Windows nodes that are created as part of test suite run
- `-m` to represent the number of Windows instances to be configured by the Windows Machine controller
- `-c` to represent the number of Windows instances to be configured by the ConfigMap controller
- `-b` gives an alternative path to the WMCO binary. This option overridden in OpenShift CI.
       When building the operator locally, the WMCO binary is created as part of the operator image build process and
       can be found at `build/_output/bin/windows-machine-config-operator`, this is the default value of this option.
- `-t` specify the test to run. All tests are run if the option is not used. The allowed options are:
  - `all` all the tests are run
  - `basic` creation, network and deletion tests are run
  - `upgrade` creation, upgrade, reconfiguration and deletion tests are run

Example command to run the full test suite with 2 instances configured by the Windows Machine controller, and 1
configured by the ConfigMap controller, skipping the deletion of all the nodes.
```shell script
hack/run-ci-e2e-test.sh -s -m 2 -c 1
```
Please note that you do not need to run `hack/olm.sh run` before `hack/run-ci-e2e-test.sh`.

#### Running e2e tests on platform-agnostic infrastructure

To run the WMCO e2e tests on a bare metal or other platform-agnostic infrastructure (platform=none), where there
is no cloud provider specification, the desired number of Windows instances must be provisioned beforehand, and the
instance(s) information must be provided to the e2e test suite through the `WINDOWS_INSTANCES_DATA` environment
variable.

To deploy machines using the [infrastructure providers supported by WMCO](wmco-prerequisites.md#supported-cloud-providers-based-on-okdocp-version-and-wmco-version),
refer to the specific provider documentation. See Windows instance [pre-requisites](https://github.com/openshift/windows-machine-config-operator/blob/master/docs/byoh-instance-pre-requisites.md).

The information you need to collect from each Windows instance is:
- the internal IP address
- the Windows administrator username

Export `WINDOWS_INSTANCES_DATA` as an environment variable with the corresponding [windows-instances ConfigMap](https://github.com/openshift/windows-machine-config-operator#adding-instances)
data section, and a new `windows-instances` ConfigMap will be created during the execution of the e2e test suite.

For example:
```shell
export WINDOWS_INSTANCES_DATA='
data:
  10.1.42.1: |-
    username=Administrator
'
```
where `10.1.42.1` is the IP address and `Administrator` is the Windows' administrator username of the Windows instance.

After the `WINDOWS_INSTANCES_DATA` environment variable is set and exported you can run:
```shell
hack/run-ci-e2e-test.sh
```

## Bundling the Windows Machine Config Operator
This directory contains resources related to installing the WMCO onto a cluster using OLM.

### Generating new bundle manifests
This step should be done in the case that changes have been made to any of the RBAC requirements, or if any changes
were made to the files in config/.

If the operator version needs to be changed, the `WMCO_VERSION` variable at the top of the repo's Makefile should
be set to the new version.

If the bundle channel needs to be changed, the `CHANNELS` and/or `DEFAULT_CHANNELS` variable within the Makefile
should be changed to the desired values.

New bundle manifests can then be generated with:
```shell script
make bundle
```

Within the bundle manifests the `:latest` tag must be manually removed in order for the e2e tests to work.

If you plan on building a bundle image using these bundle manifests, the image should be set to reflect where
the WMCO was pushed to in the [build step](#build)
```shell script
make bundle IMG=$OPERATOR_IMAGE
```

### Creating a bundle image
You can skip this step if you want to run the operator for [developer testing purposes only](#development-workflow)

Once the manifests have been generated properly, you can run the following command in the root of this git repository:
```shell script
make bundle-build BUNDLE_IMG=<BUNDLE_REPOSITORY:$BUNDLE_TAG>
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

#### Pre-requisites
[opm](https://github.com/operator-framework/operator-registry/) has been installed on the build system.
All previous [pre-requisites](#pre-requisites) must be satisfied as well.

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

## Updating Git submodules
This project contains git submodules for the following components:
- kubernetes
- ovn-kubernetes
- containernetworking-plugins
- promu
- windows_exporter

To update git submodules you can use the script hack/update_submodules.sh
Use the help command for up to date instructions:
```shell script
hack/update_submodules.sh -h
```

Alternatively, it can be done manually via:
```shell script
# To update all git submodules
git submodule update --recursive

# To update a specific submodule
git submodule update --remote <path_to_submodule>
```

## Preventing Windows Machines from being configured by WMCO

If the Machine spec has the label `windowsmachineconfig.openshift.io/ignore=true`, the Machine will be ignored by
WMCO's Machine controller, and will not be configured into a Windows node. This can be helpful when debugging userdata
changes.
