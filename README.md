# Windows Machine Config Operator

## Introduction
The Windows Machine Config Operator configures Windows Machines into nodes, enabling Windows container workloads to
be ran within OKD/OCP clusters. The operator is configured to watch for [Machines](https://docs.openshift.com/container-platform/4.5/machine_management/creating_machinesets/creating-machineset-aws.html#machine-api-overview_creating-machineset-aws) with a `machine.openshift.io/os-id: Windows`
label. The way a user will initiate the process is by creating a MachineSet which uses a Windows image with the Docker
container runtime installed. The operator will do all the necessary steps to configure the underlying VM so that it can join
the cluster as a worker node.

More design details can be explored in the [WMCO enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/windows-containers/windows-machine-config-operator.md).

## Pre-requisites
- OKD/OCP 4.6 cluster running on Azure or AWS, configured with [hybrid OVN Kubernetes networking](docs/setup-hybrid-OVNKubernetes-cluster.md)
- [Install](https://v0-19-x.sdk.operatorframework.io/docs/install-operator-sdk/) operator-sdk
  v0.19.4
- The operator is written using operator-sdk [v0.19.4](https://github.com/operator-framework/operator-sdk/releases/tag/v0.19.4)
  and has the same [pre-requisites](https://v0-19-x.sdk.operatorframework.io/docs/install-operator-sdk/#prerequisites) as it
  does.

## Usage
The operator can be installed from the *community-operators* catalog on OperatorHub.
It can also be build and installed from source manually, see the [development instructions](docs/HACKING.md).

Once the `openshift-windows-machine-config-operator` namespace has been created, a secret must be created containing
the private key that will be used to access the Windows VMs. The private key should be in PEM encoded RSA format.
The following commands will create a RSA key and a secret containing the key, please change path as necessary:
```
# Create a 2048-bit RSA SSH key with empty passphrase
ssh-keygen -q -t rsa -b 2048 -P "" -f /path/to/key

# Create secret containing key in openshift-windows-machine-config-operator namespace
oc create secret generic cloud-private-key --from-file=private-key.pem=/path/to/key -n openshift-windows-machine-config-operator
```

Below is the example of an Azure Windows MachineSet which can create Windows Machines that the WMCO can react upon.
Please note that the windows-user-data secret will be created by the WMCO lazily when it is configuring the first
Windows Machine. After that, the windows-user-data will be available for the subsequent MachineSets to be consumed.
It might take around 10 minutes for the Windows VM to be configured so that it joins the cluster. Please note that
the MachineSet should have following labels:
* *machine.openshift.io/os-id: Windows*
* *machine.openshift.io/cluster-api-machine-role: worker*
* *machine.openshift.io/cluster-api-machine-type: worker*

The following label has to be added to the Machine spec within the MachineSet spec:
* *node-role.kubernetes.io/worker: ""*

Not having these labels will result in the Windows node not being marked as a worker.

`<infrastructureID>` should be replaced with the output of:
```
oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```

`<location>` should be replaced with a valid Azure location like `centralus`.

`<zone>` should be replaced with a valid Azure availability zone like `us-east-1a`.

Please note that on Azure, Windows Machine names cannot be more than 15 characters long.
The MachineSet name can therefore not be more than 9 characters long, due to the way Machine names are generated from it.
```
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: <infrastructureID>
  name: winworker
  namespace: openshift-machine-api
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: <infrastructureID>
      machine.openshift.io/cluster-api-machineset: winworker
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: <infrastructureID>
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: winworker
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
- [AWS](docs/machineset-aws.md)

Alternatively, the [hack/machineset.sh](hack/machineset.sh) script can be used to generate MachineSets for AWS and Azure platforms.
The hack script will generate a `MachineSet.yaml` file which can be edited before using or can be used as it is.
The script takes optional arguments `apply` and `delete` to directly create/delete MachineSet on the cluster without 
generating a `yaml` file.

Usage:
```
./hack/machineset.sh                 # to generate yaml file
./hack/machineset.sh apply/delete    # to create/delete MachineSet directly on cluster
```

## Windows nodes Kubernetes component upgrade

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

## Development

See [HACKING.md](docs/HACKING.md).