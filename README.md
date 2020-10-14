# Windows Machine Config Operator

## Introduction
The Windows Machine Config Operator configures Windows Machines into nodes, enabling Windows container workloads to
be ran within OKD/OCP clusters. The operator is configured to watch for [Machines](https://docs.openshift.com/container-platform/4.5/machine_management/creating_machinesets/creating-machineset-aws.html#machine-api-overview_creating-machineset-aws) with a `machine.openshift.io/os-id: Windows`
label. The way a user will initiate the process is by creating a MachineSet which uses a Windows image with the Docker
container runtime installed. The operator will do all the necessary steps to configure the underlying VM so that it can join
the cluster as a worker node.

More design details can be explored in the [WMCO enhancement](https://github.com/openshift/enhancements/blob/master/enhancements/windows-containers/windows-machine-config-operator.md).

## Pre-requisites
* OKD/OCP 4.6 cluster running on Azure or AWS, configured with [hybrid OVN Kubernetes networking](docs/setup-hybrid-OVNKubernetes-cluster.md)

### Pre-configuration steps in case of vSphere cluster:
#### Windows VM assumptions:
- An updated version of Windows Server 1909 VM image and corresponding container images are used
- The Windows image is configured with SSH so that WMCO can access it:
  - SSH on the Windows VM is configured using the [powershell script](https://gist.githubusercontent.com/ravisantoshgudimetla/087bf49088f70edf7f97777386d12f18/raw/0f13048697130a0265ed1050591361fdebcee3ae/powershell.yaml)
  - A new file called C:\Users\Administrator\.ssh\authorized_keys containing the public key to be used is created
- VMWare tools is installed, configured and running on the Windows VM:
  - The [tools.conf](https://docs.vmware.com/en/VMware-Tools/11.1.0/com.vmware.vsphere.vmwaretools.doc/GUID-EA16729B-43C9-4DF9-B780-9B358E71B4AB.html) file at C:\ProgramData\VMware\VMware Tools\tools.conf has the following entry to ensure that the cloned vNIC generated on the Windows VM by hybrid-overlay won't be ignored and the VM has an IP Address in the vCenter
    - `exclude-nics=`
  - The tools Windows service is running in the Windows VM.
- The Windows image has been sysprep'ed and it is available for cloning using the following command:
`C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:<path_to_unattend.xml>`

Sample [unattend.xml](https://gist.github.com/ravisantoshgudimetla/9d8e26032810a8df0b71b8d67f4d906b)


- Generally, ignition config files are available on the internal API Server URL in case of IPI however in case of 
vSphere, the ignition files are available on both internal and external API Server URLs. WMCO as of now, downloads the
ignition files from internal API server. So, add a new DNS entry for `api-int.<cluster-name>.domain.com` which points
to external API server URL `api.<cluster-name>.domain.com`. The external api should have been created as part of 
[cluster install](https://docs.openshift.com/container-platform/4.5/installing/installing_vsphere/installing-vsphere-installer-provisioned.html)
This requirement will be removed in the future.

vSphere Windows VMs are cloned off an existing template or VM, for every VM the following steps need to be done. It is assumed that sysprep has been run on the VM:
- Change the hostname of the Windows VM to match Windows Machine you created. The following powershell command can be used to do so:
```
Rename-Computer -NewName <machine_name> -Force -Restart
```
This process is going to be automated in the future. This document will be updated accordingly.
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

### Creating a vSphere Windows MachineSet

The following label has to be added to the Machine spec within the MachineSet spec:
* *node-role.kubernetes.io/worker: ""*

Not having these labels will result in the Windows node not being marked as a worker.

`<infrastructureID>` should be replaced with the output of:
```
oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```

Please note that on vSphere Windows Machine names cannot be more than 15 characters long.
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
            - networkName: dev-segment
          numCPUs: 4
          numCoresPerSocket: 1
          snapshot: ""
          template: 1909-template-docker-ssh
          userDataSecret:
            name: worker-user-data-managed
          workspace:
            datacenter: xyz
            datastore: abc
            folder: folder_abc
            resourcePool: resource_abc
            server: vSphere_cluster
```


Example MachineSet for other cloud providers:
- [AWS](docs/machineset-aws.md)
- [Azure](docs/machineset-azure.md)

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