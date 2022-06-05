# Creating an AWS Windows MachineSet

_\<infrastructureID\>_ should be replaced with the output of:
```shell script
 oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster
```
_\<region\>_ should be replaced with a valid AWS region like `us-east-1`.

_\<zone\>_ should be replaced with a valid AWS availability zone like `us-east-1a`.

_\<windows_ami_id\>_ should be replaced with the output of:
```shell script
 aws ec2 describe-images \
    --region ${region} \
    --filters "Name=name,Values=Windows_Server-2022*English*Full*Base*" "Name=is-public,Values=true" \
    --query "reverse(sort_by(Images,&CreationDate))[*].{name: Name, id: ImageId}" \
    --output json \
    | jq -r '.[0].id'
```
where `${region}` is the selected AWS Region. Refer to the [supported Windows Server versions](https://github.com/openshift/windows-machine-config-operator/blob/master/docs/wmco-prerequisites.md#supported-windows-server-versions).

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
          ami:
            id: <windows_ami_id>
          apiVersion: awsproviderconfig.openshift.io/v1beta1
          blockDevices:
            - ebs:
                iops: 0
                volumeSize: 120
                volumeType: gp2
          credentialsSecret:
            name: aws-cloud-credentials
          deviceIndex: 0
          iamInstanceProfile:
            id: <infrastructureID>-worker-profile 
          instanceType: m5a.large
          kind: AWSMachineProviderConfig
          placement:
            availabilityZone: <zone>
            region: <region>
          securityGroups:
            - filters:
                - name: tag:Name
                  values:
                    - <infrastructureID>-worker-sg 
          subnet:
            filters:
              - name: tag:Name
                values:
                  - <infrastructureID>-private-<zone>
          tags:
            - name: kubernetes.io/cluster/<infrastructureID> 
              value: owned
          userDataSecret:
            name: windows-user-data
            namespace: openshift-machine-api
```
