# Windows Machine Config Operator

## Introduction
The Windows Machine Config Operator configures Windows instances into nodes, enabling Windows container workloads to
be ran within OKD/OCP clusters. Windows instances can be added either by creating a [MachineSet](https://docs.openshift.com/container-platform/4.5/machine_management/creating_machinesets/creating-machineset-aws.html#machine-api-overview_creating-machineset-aws),
or by specifying existing instances through a ConfigMap. Through either method, the Windows instance must have the
Docker container runtime installed. The operator will do all the necessary steps to configure the instance so that it
can join the cluster as a worker node.

More design details can be explored in the [WMCO enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/windows-containers/windows-machine-config-operator.md).

## Pre-requisites
- [Cluster and OS pre-requisites](docs/wmco-prerequisites.md)

## Usage

### Installation
The operator can be installed from the *community-operators* catalog on OperatorHub.
It can also be build and installed from source manually, see the [development instructions](docs/HACKING.md).

### Create a private key secret
Once the `openshift-windows-machine-config-operator` namespace has been created, a secret must be created containing
the private key that will be used to access the Windows instances:
```shell script
# Create secret containing the private key in the openshift-windows-machine-config-operator namespace
oc create secret generic cloud-private-key --from-file=private-key.pem=/path/to/key -n openshift-windows-machine-config-operator
```
We strongly recommend not using the same
[private key](https://docs.openshift.com/container-platform/4.6/installing/installing_azure/installing-azure-default.html#ssh-agent-using_installing-azure-default)
used when installing the cluster

### Configuring BYOH (Bring Your Own Host) Windows instances
WARNING: This is not a fully developed feature. Nodes can be removed from the cluster by deleting the Node object,
         but the changes made to the instance will not be undone. Use at your own risk.

A ConfigMap named `windows-instances` must be created in the WMCO namespace, describing the instances that should be
joined to a cluster. The required information to configure an instance is:
* An address to SSH into the instance with. This can be a DNS name or an ipv4 address.
* An administrator user with the [private key](#create-a-private-key-secret) set as an authorized SSH key. This must
  be done within the Windows instance by the user.

Each instance described in the ConfigMap must have the Docker container runtime installed.

Each entry in the data section of the ConfigMap should be formatted with the address as the key, and a value with the
format of username=\<username\>. Please see the example below:

```yaml
kind: ConfigMap
apiVersion: v1
metadata:
  name: windows-instances
  namespace: openshift-windows-machine-config-operator
data:
  10.1.42.1: |-
    username=Administrator
  instance.dns.com: |-
    username=core
```

Deleting `windows-instances` is viewed as a request to deconfigure all Windows instances added as Nodes and revert them
back to the state they were in before, barring any logs and container runtime artifacts.

### Configuring Windows instances provisioned through MachineSets
Below is an example of a vSphere Windows MachineSet which can create Windows Machines that the WMCO can react upon.
Please note that the windows-user-data secret will be created by the WMCO lazily when it is configuring the first
Windows Machine. After that, the windows-user-data will be available for the subsequent MachineSets to be consumed.
It might take around 10 minutes for the Windows instance to be configured so that it joins the cluster. Please note that
the MachineSet should have following labels:
* *machine.openshift.io/os-id: Windows*
* *machine.openshift.io/cluster-api-machine-role: worker*
* *machine.openshift.io/cluster-api-machine-type: worker*

The following label has to be added to the Machine spec within the MachineSet spec:
* *node-role.kubernetes.io/worker: ""*

Not having these labels will result in the Windows node not being marked as a worker.

`<infrastructureID>` should be replaced with the output of:
```shell script
oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```
The following template variables need to be replaced as follows with values from your vSphere environment:
* *\<Windows_VM_template\>*: template name
* *\<VM Network Name\>*: network name
* *\<vCenter DataCenter Name\>*: datacenter name
* *\<Path to VM Folder in vCenter\>*: path where your OpenShift cluster is running
* *\<vCenter Datastore Name\>*: datastore name
* *\<vCenter Server FQDN/IP\>*: IP address or FQDN of the vCenter server

*IMPORTANT*:
- The template used in the MachineSet must be a Windows Server 1909
  image as described in [vSphere prerequisites](docs/vsphere-prerequisites.md).
- On vSphere, Windows Machine names cannot be more than 15 characters long. The
  MachineSet name, therefore, cannot be more than 9 characters long, due to the
  way Machine names are generated from it.
```yaml
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
          apiVersion: vsphereprovider.openshift.io/v1beta1
          credentialsSecret:
            name: vsphere-cloud-credentials
          diskGiB: 128
          kind: VSphereMachineProviderSpec
          memoryMiB: 16384
          metadata:
            creationTimestamp: null
          network:
            devices:
            - networkName:  "<VM Network Name>"
          numCPUs: 4
          numCoresPerSocket: 1
          snapshot: ""
          template: <Windows_VM_template>
          userDataSecret:
            name: worker-user-data-managed
          workspace:
             datacenter: <vCenter DataCenter Name>
             datastore: <vCenter Datastore Name>
             folder: <Path to VM Folder in vCenter> # e.g. /DC/vm/ocp45-2tdrm
             server: <vCenter Server FQDN/IP>

```

Example MachineSet for other cloud providers:
- [AWS](docs/machineset-aws.md)
- [Azure](docs/machineset-azure.md)

Alternatively, the [hack/machineset.sh](hack/machineset.sh) script can be used to generate MachineSets for AWS and Azure platforms.
The hack script will generate a `MachineSet.yaml` file which can be edited before using or can be used as it is.
The script takes optional arguments `apply` and `delete` to directly create/delete MachineSet on the cluster without 
generating a `yaml` file.

Usage:
```shell script
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

## Enabled features

### Autoscaling Windows nodes
Cluster autoscaling is supported for Windows instances. 

- Define and deploy a [ClusterAutoscaler](https://docs.openshift.com/container-platform/latest/machine_management/applying-autoscaling.html#configuring-clusterautoscaler).
- Create a Windows node through a MachineSet (see spec in [Usage section](https://github.com/openshift/windows-machine-config-operator#usage)).
- Define and deploy a [MachineAutoscaler](https://docs.openshift.com/container-platform/latest/machine_management/applying-autoscaling.html#configuring-machineautoscaler), referencing a Windows MachineSet.

## Development

See [HACKING.md](docs/HACKING.md).
