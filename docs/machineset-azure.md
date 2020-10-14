# Creating an Azure Windows MachineSet

_\<infrastructureID\>_ should be replaced with the output of:
```shell script
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```

<location> should be replaced with a valid Azure location like centralus.

<zone> should be replaced with a valid Azure availability zone like us-east-1a.

Please note that on Azure, Windows Machine names cannot be more than 15 characters long. The MachineSet name can therefore not be more than 9 characters long, due to the way Machine names are generated from it.

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
