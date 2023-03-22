# Hacking on the WMCO

This guide will walk you through the workflows required to build, deploy, and test your changes on the Windows Machine
Config Operator (WMCO).

## Cluster Configuration

Before running tests, your cluster must be properly configured with the correct networking and be running the proper
version of OpenShift for use with the version of WMCO you are testing against. For example, when testing a 4.10 release
of the WMCO, your cluster should be running a 4.10 version of OpenShift.
Changes made against master should be tested against the latest CI version of OpenShift.

If you already have a configured cluster, move on to [Build](#build)

### Pre-cluster checklist

- Download the correct OpenShift installer
  - [OpenShift Installer Mirror](<https://mirror.openshift.com/pub/openshift-v4/clients/ocp/>)
  - [OpenShift Nightly Builds](<https://amd64.ocp.releases.ci.openshift.org/#4-dev-preview>)
- [Initialize git submodules](<https://git-scm.com/book/en/v2/Git-Tools-Submodules>)
- Verify push/pull access to [quay.io](<https://quay.io>)
- Create an install-config.yaml
  - [AWS](https://docs.openshift.com/container-platform/latest/installing/installing_aws/installing-aws-default.html)
  - [GCP](https://docs.openshift.com/container-platform/latest/installing/installing_gcp/installing-gcp-default.html)
  - [Azure](https://docs.openshift.com/container-platform/latest/installing/installing_azure/installing-azure-default.html)
  - [vSphere](https://docs.openshift.com/container-platform/latest/installing/installing_vsphere/installing-vsphere-installer-provisioned.html)
  - [None](https://docs.openshift.com/container-platform/latest/installing/installing_bare_metal/installing-bare-metal.html)
- [Set Up OVNKubernetes networking](https://docs.openshift.com/container-platform/latest/networking/ovn_kubernetes_network_provider/configuring-hybrid-networking.html)

## Build

To manually build, see the subheading
[below](#build-an-operator-image). To build and deploy automatically using the OLM hack script, jump ahead to
[Deploy](#deploy-from-source). 
If you want to build and deploy automatically using the e2e tests, jump ahead to 
[running the e2e tests](#running-the-e2e-tests). 

### Build an operator image

If you want to deploy the operator manually without the OLM, you need to build an operator image that we can deploy to
the cluster.
From the top level of your WMCO directory, run

```shell script
podman build -t quay.io/<quay username>/wmco:<operator name> -f build/Dockerfile .
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

### Push an operator image

Push your image to your container registry with podman

`podman push quay.io/<quay username>/wmco:<operator name>`

## Deploy from source 

Before deploying, ensure your environment is properly configured.

#### Set private key

in order to SSH into the instance, you'll need to set the `KUBE_SSH_KEY_PATH` to your private key.

```shell script
export KUBE_SSH_KEY_PATH=<path to pem>
```

#### Set Image Name

This is the name of the operator image that will be pushed to the container registry.

```shell script
export OPERATOR_IMAGE=quay.io/<quay username>/wmco:<identifying tag>
```

#### Set CI to false

This is to tell the tests that this test isn't running in CI.

```shell script
export OPENSHIFT_CI=false
```

#### Set shared credentials file

If you are running on AWS or GCP, set your shared credentials file.

```shell script
export AWS_SHARED_CREDENTIALS_FILE=<path to aws credentials file>
```

```shell script 
export GOOGLE_CREDENTIALS=<path to google credentials file>
```

#### Set KUBECONFIG

To run tests, you should have a running cluster, and your KUBECONFIG set.

```shell script
export KUBECONFIG=<path to kubeconfig> 
```

The operator can now be deployed through one of these methods.
- [Manually](#deploying-the-operator-manually)
- [Using the OLM](#running-the-olm-hack-script)
- [Using the e2e tests](#running-the-e2e-tests)
- [Using the e2e tests on platform agnostic infrastructure](#running-e2e-tests-on-platform-agnostic-infrastructure)

### Deploying the operator manually

```shell script
make deploy IMG=$OPERATOR_IMAGE
```

The above command will directly deploy all the resources required for the operator to run, as well as the WMCO pod.

#### Cleaning up a manual deployment  

To remove the installed resources:

```shell script
make undeploy
```

### Deploying using OLM 

This command builds the operator image, pushes it to a remote repository and uses OLM to launch the operator.

```shell script
hack/olm.sh run -k KUBE_SSH_KEY_PATH
```
#### Cleaning up an OLM deployment

```shell script
hack/olm.sh cleanup
```
### Deploying using a custom CatalogSource
#### Generate new bundle manifests

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
the WMCO was pushed to in the [build step](#Build an operator image)

```shell script
make bundle IMG=$OPERATOR_IMAGE
```

#### Create a bundle image

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

#### Creating a new operator index
##### Prerequisites

[opm](https://github.com/operator-framework/operator-registry/) has been installed on the build system.
All [agnostic prerequisites](#agnostic-e2e-prerequisites) must be satisfied as well.

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
#### Create a CatalogSource using the index

See the OpenShift docs found
[here](https://docs.openshift.com/container-platform/latest/operators/admin/olm-managing-custom-catalogs.html#olm-creating-catalog-from-index_olm-managing-custom-catalogs)

#### Create a subscription object 

To use the custom CatalogSource you need to change the subscription object to reference the CatalogSource you are 
introducing. See the docs 
[here](https://docs.openshift.com/container-platform/4.5/operators/understanding/olm/olm-understanding-olm.html#olm-subscription_olm-understanding-olm)
for an example subscription. 

Edit your subscription yaml to match your CatalogSource. 
For example, 
```
  source: < new catalogSource > 
  sourceNamespace: < namespace where CatalogSource was deployed >
```

Apply the yaml to your cluster, which will deploy the WMCO. 

## Running the e2e tests

The e2e tests also automatically build and deploy the WMCO onto your cluster.

The e2e tests can be run with the hack script `run-ci-e2e-test.sh`

```shell script
hack/run-ci-e2e-test.sh -t basic -m 1
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

### Running e2e tests on platform-agnostic infrastructure

#### Agnostic e2e Prerequisites
Please see the [BYOH](byoh-instance-pre-requisites.md) doc for info. 

##### Specify number of Windows instances

To run the WMCO e2e tests on a bare metal or other platform-agnostic infrastructure (platform=none), where there is no
cloud provider specification, the desired number of Windows instances must be provisioned beforehand.

##### Specify windows instances data

Find the corresponding [windows-instances ConfigMap](https://github.com/openshift/windows-machine-config-operator#adding-instances)
and export it as an environment variable. For example:

```shell script
export WINDOWS_INSTANCES_DATA='
data:
  10.1.42.1: |-
    username=Administrator
'
```
#### Cleaning up e2e tests
#### Delete Machinesets

```shell script
oc delete machineset e2e -n openshift-machine-api
oc delete machineset e2e-wm -n openshift-machine-api
oc delete configmap windows-instances -n openshift-windows-machine-config-operator
```

##### Run the cleanup OLM script

Clean up the artifacts by running the OLM hack script

```shell script
hack/olm.sh cleanup
oc delete ns wmco-test
```

## Deploy a workload 

If you are not running e2e tests, it is likely that you will want to deploy a workload to test your changes with.
Find more information [here](custom-workload.md)

## Updating git submodules 

This project contains git submodules for the following components:
<<<<<<< HEAD
- windows-machine-config-bootstrapper
=======

>>>>>>> bd70beef ([docs] updates HACKING.md)
- kubernetes
- ovn-kubernetes
- containernetworking-plugins
- promu
- windows_exporter
- containerd 
- hcsshim 
- kube-proxy 
- cloud-provider-azure 

To update the git submodules you can use the script hack/update_submodules.sh
Use the help command for up to date instructions:

```shell script
hack/update_submodules.sh -h
```

## Preventing Windows Machines from being configured by WMCO

If the Machine spec has the label `windowsmachineconfig.openshift.io/ignore=true`, the Machine will be ignored by
WMCO's Machine controller, and will not be configured into a Windows node. This can be helpful when debugging userdata
changes.
