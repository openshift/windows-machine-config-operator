# Creating an Azure Windows MachineSet

*\<infrastructureID\>* should be replaced with the output of:
```bash
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```
The following template variables need to be replaced as follows with values from your Azure environment:
* *\<location\>*: Azure location like *centralus*
* *\<zone\>*: Azure availability zone like *1*

Please note that on Azure, Windows VM names cannot be more than 15 characters long. The MachineSet name 
therefore cannot be more than 9 characters long, due to the way Machine names are generated from it.
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
