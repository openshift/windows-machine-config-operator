# Creating a Nutanix Windows MachineSet

Replace _\<windows_server_image_name\>_ with the full name of an image of a [supported version](wmco-prerequisites.md#supported-windows-server-versions) of the Windows operating system that is pre-uploaded to the Prism-Central/Prism-Element where the Machine VMs will be created.
For example, `nutanix-windows-server-2022`

Replace _\<infrastructure_id\>_ with the output:
```shell script
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```

For the general Nutanix MachineSet `providerSpec` configuration, refer to the OpenShift document [creating machineset nutanix](https://docs.openshift.com/container-platform/latest/machine_management/creating_machinesets/creating-machineset-nutanix.html).
```
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: <infrastructure_id>
  name: <infrastructure_id>-windows-worker-<suffix>
  namespace: openshift-machine-api
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: <infrastructure_id>
      machine.openshift.io/cluster-api-machineset: <infrastructure_id>-windows-worker-<suffix>
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: <infrastructure_id>
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: <infrastructure_id>-windows-worker-<suffix>
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
      providerSpec:
        value:
          apiVersion: machine.openshift.io/v1
          bootType: ""
          categories: null
          cluster:
            type: uuid
            uuid: <prism_element_uuid>
          credentialsSecret:
            name: nutanix-credentials
          image:
            type: name
            name: <windows_server_image_name>
          kind: NutanixMachineProviderConfig
          memorySize: 16Gi
          project:
            type: ""
          subnets:
          - type: name
            name: <subnet_name>
          systemDiskSize: 120Gi
          userDataSecret:
            name: windows-user-data
          vcpuSockets: 4
          vcpusPerSocket: 1
```