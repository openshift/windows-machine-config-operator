# Creating a GCP Windows MachineSet

_\<windows_server_image \>_ should be replaced with the full path to an image of a [supported version](wmco-prerequisites.md#supported-windows-server-versions) of Windows.
For example, to use the latest image from the `windows-2022-core` family in the `windows-cloud` project, you would use:
`projects/windows-cloud/global/images/family/windows-2022-core`


_\<infrastructure_id\>_ should be replaced with the output of:
```shell script
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```
_\<region\>_ should be replaced with a region, such as `us-central1`.

_\<zone\>_ should be replaced with a zone within the chosen region, such as `us-central1-a`.

_\<zone_suffix\>_ should be replaced with the suffix of the chosen zone, such as `a`.

_\<project_id\>_ should be replaced with the GCP project this cluster was created under.
```
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: <infrastructure_id>
  name: <infrastructure_id>-windows-worker-<zone_suffix>
  namespace: openshift-machine-api
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: <infrastructure_id>
      machine.openshift.io/cluster-api-machineset: <infrastructure_id>-windows-worker-<zone_suffix>
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: <infrastructure_id>
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: <infrastructure_id>-windows-worker-<zone_suffix>
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
      providerSpec:
        value:
          apiVersion: machine.openshift.io/v1beta1
          canIPForward: false
          credentialsSecret:
            name: gcp-cloud-credentials
          deletionProtection: false
          disks:
          - autoDelete: true
            boot: true
            image: <windows_server_image>
            sizeGb: 128
            type: pd-ssd
          kind: GCPMachineProviderSpec
          machineType: n1-standard-4
          networkInterfaces:
          - network: <infrastructure_id>-network
            subnetwork: <infrastructure_id>-worker-subnet
          projectID: <project_id>
          region: <region>
          serviceAccounts:
          - email: <infrastructure_id>-w@<project_id>.iam.gserviceaccount.com
            scopes:
            - https://www.googleapis.com/auth/cloud-platform
          tags:
          - <infrastructure_id>-worker
          userDataSecret:
            name: windows-user-data
          zone: <zone>
```
