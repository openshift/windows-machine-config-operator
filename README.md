# Windows Machine Config Operator

## Introduction
The Windows Machine Config Operator is the entry point for OpenShift customers who want to run Windows workloads on
their clusters. The operator is configured to watch for [Machines](https://docs.openshift.com/container-platform/4.4/machine_management/creating_machinesets/creating-machineset-aws.html#machine-api-overview_creating-machineset-aws)
with a `machine.openshift.io/os-id: Windows` label. The way a customer will initiate the process is by creating a 
MachineSet with this label specifying an Windows image that has the container runtime installed. The operator will do
all the necessary steps to configure the underlying VM so that it can join the cluster as a worker node. More design
details can be explored in the [WMCO enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/windows-containers/windows-machine-config-operator.md).

Customers will eventually be able to install it using OperatorHub. These instructions are for those who want to try out
the latest version of the operator.

## Pre-requisites
- OpenShift 4.6+ cluster on Azure / AWS configured with
  [hybrid OVN Kubernetes networking](https://github.com/openshift/windows-machine-config-bootstrapper/blob/release-4.6/tools/ansible/docs/ocp-4-4-with-windows-server.md#bring-up-the-openshift-cluster-with-ovn-kubernetes).
- [Install](https://v0-18-x.sdk.operatorframework.io/docs/install-operator-sdk/) operator-sdk
  v0.18.1
  - Note that the doc sets the release to `v0.18.2`. Please use `v0.18.1` instead.
- The operator is written using operator-sdk [v0.18.1](https://github.com/operator-framework/operator-sdk/releases/tag/v0.18.1)
  and has the same [pre-requisites](https://v0-18-x.sdk.operatorframework.io/docs/install-operator-sdk/#prerequisites) as it
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

### Deploying the operator on a cluster
#### Openshift Console
This deployment method is currently not supported. Please use the [CLI](#cli)

#### CLI

Create the openshift-windows-machine-config-operator namespace:
```shell script
oc apply -f deploy/namespace.yaml
```

Switch to the openshift-windows-machine-config-operator project:
```shell script
oc project openshift-windows-machine-config-operator
```

##### Create private key Secret
In order to run the operator, you need to create a secret containing the private key that will be used to access the
Windows VMs. The private key should be in PEM encoded RSA format.

```shell script
# Change paths as necessary
oc create secret generic cloud-private-key --from-file=private-key.pem=$HOME/.ssh/$keyname
```

##### Running with bundle and index images
You can skip this step if you want to run the operator for [developer testing purposes only](#development-workflow)

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

### Creating a Windows MachineSet
Below is the example of an Azure Windows MachineSet which can create Windows Machines that the WMCO can react upon. Please
note that the `windows-user-data` secret will be created by the WMCO lazily when it is configuring the first Windows 
Machine. After that, the `windows-user-data` will be available for the subsequent MachineSets to be consumed. It might 
take around 10 minutes for the Windows VM to be configured so that it joins the cluster. Please note that the MachineSet
should have following labels:
- `machine.openshift.io/os-id: Windows`
- `machine.openshift.io/cluster-api-machine-role: worker` 
- `machine.openshift.io/cluster-api-machine-type: worker` 

The following label, needs to be added to the `Machine` spec with the `MachineSet` spec:
- `node-role.kubernetes.io/worker: ""`

Not having these labels will result in the Windows node not being marked as a worker.

##### Example

_\<infrastructureID\>_ should be replaced with the output of:
```shell script
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```
_\<location\>_ should be replaced with a valid Azure location like `centralus`.
_\<zone\>_ should be replaced with a valid Azure availability zone, i.e. `1`, `2` or `3`.

```
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: <infrastructureID> 
  name: <infrastructureID>-windows-worker-<zone>
  namespace: openshift-machine-api
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: <infrastructureID> 
      machine.openshift.io/cluster-api-machineset: <infrastructureID>-windows-worker-<zone>
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: <infrastructureID> 
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: <infrastructureID>-windows-worker-<zone>
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
      providerSpec:
        value:
          apiVersion: azureproviderconfig.openshift.io/v1beta1
          credentialsSecret:
            name: azure-cloud-credentials
            namespace: openshift-machine-api
          image:
            offer: WindowsServer
            publisher: MicrosoftWindowsServer
            resourceID: ""
            sku: 2019-Datacenter-with-Containers
            version: latest
          kind: AzureMachineProviderSpec
          location: <location>
          managedIdentity: <infrastructureID>-identity
          networkResourceGroup: <infrastructureID>-rg
          osDisk:
            diskSizeGB: 128
            managedDisk:
              storageAccountType: Premium_LRS
            osType: Windows
          publicIP: false
          resourceGroup: <infrastructureID>-rg
          subnet: <infrastructureID>-worker-subnet
          userDataSecret:
            name: windows-user-data
            namespace: openshift-machine-api
          vmSize: Standard_D2s_v3
          vnet: <infrastructureID>-vnet
          zone: "<zone>"
```

Example MachineSet for other cloud providers:
- [AWS](docs/machineset-aws.md).

### Windows nodes Kubernetes component upgrade

When a new version of WMCO is released that is compatible with the current cluster version, an operator upgrade will 
take place which will result in the Kubernetes components in the Windows Machine to be upgraded. For a non-disruptive 
upgrade, WMCO terminates the Windows Machines configured by previous version of WMCO and recreates them using the
current version. This is done by deleting the Machine object that results in the drain and deletion of the Windows node.
To facilitate an upgrade, WMCO adds a version annotation to all the configured nodes. During an upgrade, a mismatch in
version annotation will result in deletion and recreation of Windows Machine. In order to have minimal service 
disruption during an upgrade, WMCO makes sure that the cluster will have atleast 1 Windows Machine per MachineSet in the
running state.

WMCO is not responsible for Windows operating system updates. The cluster administrator provides the Window image while
creating the VMs and hence, the cluster administrator is responsible for providing an updated image. The cluster 
administrator can provide an updated image by changing the image in the MachineSet spec.
