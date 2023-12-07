# Windows Machine Config Operator

## Introduction
The Windows Machine Config Operator configures Windows instances into nodes, enabling Windows container workloads to
be ran within OKD/OCP clusters. Windows instances can be added either by creating a [MachineSet](https://docs.openshift.com/container-platform/4.5/machine_management/creating_machinesets/creating-machineset-aws.html#machine-api-overview_creating-machineset-aws),
or by specifying existing instances through a ConfigMap. The operator will do all the necessary steps to configure the instance so that it
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

#### Changing the private key secret
Changing the private key used by WMCO can be done by updating the contents of the existing `cloud-private-key` secret.
Some important things to note:
* Any existing Windows Machines will be destroyed and recreated in order to make use of the new key.
  This will be done one at a time, until all Machines have been handled.
* BYOH instances must be updated by the user, such that the new public key is present within the authorized_keys file.
  You are free to remove the previous key. If the new key is not authorized, WMCO will not be able to access any BYOH
  nodes. **Upgrade and Node removal functionality will not function properly until this step is complete.**

### Configuring BYOH (Bring Your Own Host) Windows instances

### Instance Pre-requisites
Any Windows instances that are to be attached to the cluster as a node must fulfill these [pre-requisites](docs/byoh-instance-pre-requisites.md).

### Adding instances
A ConfigMap named `windows-instances` must be created in the WMCO namespace, describing the instances that should be
joined to a cluster. The required information to configure an instance is:
* An address to SSH into the instance with. This can be a DNS name or an ipv4 address.
  * It is highly recommended that a DNS address is provided when instance IPs are assigned via DHCP. If not, it will be
    up to the user to update the windows-instances ConfigMap whenever an instance is assigned a new IP.
* The name of the administrator user set up as part of the [instance pre-requisites](#instance-pre-requisites).

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
  instance.example.com: |-
    username=core
```

#### Removing BYOH Windows instances
BYOH instances that are attached to the cluster as a node can be removed by deleting the instance's entry in the
ConfigMap. This process will revert instances back to the state they were in before, barring any logs and container
runtime artifacts.

In order for an instance to be cleanly removed, it must be accessible with the current private key provided
to WMCO.

For example, in order to remove the instance `10.1.42.1` from the above example, the ConfigMap would be changed to
the following:

```yaml
kind: ConfigMap
apiVersion: v1
metadata:
  name: windows-instances
  namespace: openshift-windows-machine-config-operator
data:
  instance.example.com: |-
    username=core
```

Deleting `windows-instances` is viewed as a request to deconfigure all Windows instances added as Nodes.

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
* *\<VM Network Name\>*: network name, must match the network name where other Linux workers are in the cluster
* *\<vCenter DataCenter Name\>*: datacenter name
* *\<Path to VM Folder in vCenter\>*: path where your OpenShift cluster is running
* *\<vCenter Datastore Name\>*: datastore name
* *\<vCenter Server FQDN/IP\>*: IP address or FQDN of the vCenter server

*IMPORTANT*:
- The VM template provided in the MachineSet must use a supported Windows Server
  version, as described in [vSphere prerequisites](docs/vsphere-prerequisites.md).
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
            name: windows-user-data
          workspace:
             datacenter: <vCenter DataCenter Name>
             datastore: <vCenter Datastore Name>
             folder: <Path to VM Folder in vCenter> # e.g. /DC/vm/ocp45-2tdrm
             server: <vCenter Server FQDN/IP>

```

Example MachineSet for other cloud providers:
- [AWS](docs/machineset-aws.md)
- [Azure](docs/machineset-azure.md)
- [GCP](docs/machineset-gcp.md)
- [Nutanix](docs/machineset-nutanix.md)

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

### Container Runtime
Windows instances brought up with WMCO are set up with the containerd container runtime. As WMCO installs and manages the container runtime,
it is recommended not to preinstall containerd in MachineSet or BYOH Windows instances.

### Cluster-wide proxy 
WMCO supports using a [cluster-wide proxy](https://docs.openshift.com/container-platform/latest/networking/enable-cluster-wide-proxy.html)
to route egress traffic from Windows nodes on OpenShift Container Platform.

## Limitations

### DeploymentConfigs
Windows Nodes do not support workloads created via DeploymentConfigs. Please use a normal Deployment, or other method to
deploy workloads.

### Storage
Windows Nodes are running csi-proxy and are ready to use CSI drivers, however Windows CSI driver DaemonSets are not
deployed as part of the product. In order to use persistent storage for Windows workloads, the cluster administrator
must deploy the appropriate Windows CSI driver Daemonset. This should be done by following the documentation given
by the chosen storage driver's provider. A list of drivers can be found [here](https://kubernetes-csi.github.io/docs/drivers.html#production-drivers).

### Pod Autoscaling
[Horizontal](https://docs.openshift.com/container-platform/latest/nodes/pods/nodes-pods-autoscaling.html) and
[Vertical](https://docs.openshift.com/container-platform/latest/nodes/pods/nodes-pods-vertical-autoscaler.html) Pod
autoscaling support are not available for Windows workloads.
### Other limitations
WMCO / Windows nodes does not work with the following products:
* [odo](https://docs.openshift.com/container-platform/latest/cli_reference/developer_cli_odo/understanding-odo.html)
* [OpenShift Builds](https://docs.openshift.com/container-platform/latest/cicd/builds/understanding-image-builds.html#understanding-image-builds)
* [OpenShift Pipelines](https://docs.openshift.com/container-platform/latest/cicd/pipelines/understanding-openshift-pipelines.html#understanding-openshift-pipelines)
* [OpenShift Service Mesh](https://docs.openshift.com/container-platform/latest/service_mesh/v2x/ossm-about.html)
* [Red Hat Cost Management](https://access.redhat.com/documentation/en-us/cost_management_service/2022/html/getting_started_with_cost_management/assembly-introduction-cost-management?extIdCarryOver=true&sc_cid=701f2000001OH74AAG#about-cost-management_getting-started)
* [Red Hat OpenShift Local](https://developers.redhat.com/products/openshift-local/overview)
* [OpenShift monitoring of user defined project](https://docs.openshift.com/container-platform/latest/monitoring/enabling-monitoring-for-user-defined-projects.html#enabling-monitoring-for-user-defined-projects)
* [HugePages](https://kubernetes.io/docs/tasks/manage-hugepages/scheduling-hugepages/)

### Accessing secure registries
Windows nodes managed by WMCO do not support pulling container images from secure private registries. 
It is recommended to use images from public registries or pre-pull the images in the VM image.

### Trunk port
WMCO does not support adding Windows nodes to a cluster through a trunk port. The only supported networking setup for adding Windows nodes is through an access port carrying the VLAN traffic.

## Running Windows workloads
Be sure to set the [OS field in the Pod spec](https://kubernetes.io/docs/concepts/workloads/pods/#pod-os) to Windows
when deploying Windows workloads. This field is used to authoritatively identify the pod OS for validation. 
In OpenShift, it is used when enforcing OS-specific pod security standards.

## Development

See [HACKING.md](docs/HACKING.md).
