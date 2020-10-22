#!/usr/bin/env bash

# machineset.sh - create a Windows MachineSet for Windows Machine Config Operator
# Creates a MachineSet.yaml file and apply/delete the MachineSet if optional action is provided
#
# USAGE
#    machineset.sh
# OPTIONS
#    $1      Action       (Optional) apply/delete the MachineSet
# PREREQUISITES
#    oc                   to fetch cluster info and apply/delete MachineSets on the cluster(cluster should be logged in)
#    aws                  to fetch Windows AMI id for AWS platform (only required for clusters running on AWS)
set -euo pipefail

ACTION=${1:-}

error-exit() {
    echo "Error: $*" >&2
    exit 1
}

# get_spec returns the template yaml common for all cloud providers
get_spec() {

  if [ "$#" -lt 3 ]; then
    error-exit incorrect parameter count for get_spec $#
  fi

  local infraID=$1
  local az=$2
  local provider=$3

  machineSetName="$infraID"-windows-worker-"$az"
  if [ "$provider" = "azure" ]; then
    # Shorter name for azure as VMs with more than 15 characters in name does not come up
    machineSetName="winworker"
  fi

  cat <<EOF
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: ${infraID}
  name: ${machineSetName}
  namespace: openshift-machine-api
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: ${infraID}
      machine.openshift.io/cluster-api-machineset: ${machineSetName}
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: ${infraID}
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: ${machineSetName}
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
EOF
}

# get_aws_ms creates a MachineSet for AWS Cloud Provider
get_aws_ms() {

  if [ "$#" -lt 4 ]; then
    error-exit incorrect parameter count for get_spec $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4

  # get the AMI id for the Windows VM
  ami_date="2020.09.09"
  ami_id=$(aws ec2 describe-images --filters Name=name,Values=Windows_Server-2019-English-Full-ContainersLatest-${ami_date} --region ${region} --query 'Images[*].[ImageId]' --output text)

  cat <<EOF
$(get_spec $infraID $az $provider)
      providerSpec:
        value:
          ami:
            id: ${ami_id}
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
            id: ${infraID}-worker-profile
          instanceType: m5a.large
          kind: AWSMachineProviderConfig
          placement:
            availabilityZone: ${az}
            region: ${region}
          securityGroups:
            - filters:
                - name: tag:Name
                  values:
                    - ${infraID}-worker-sg
          subnet:
            filters:
              - name: tag:Name
                values:
                  - ${infraID}-private-${az}
          tags:
            - name: kubernetes.io/cluster/${infraID}
              value: owned
          userDataSecret:
            name: windows-user-data
EOF
}

# get_azure_ms creates a MachineSet for Azure Cloud Provider
get_azure_ms() {

  if [ "$#" -lt 4 ]; then
    error-exit incorrect parameter count for get_spec $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4

  cat <<EOF
$(get_spec $infraID $az $provider)
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
          location: ${region}
          managedIdentity: ${infraID}-identity
          metadata:
            creationTimestamp: null
          networkResourceGroup: ${infraID}-rg
          osDisk:
            diskSizeGB: 128
            managedDisk:
              storageAccountType: Premium_LRS
            osType: Windows
          publicIP: false
          resourceGroup: ${infraID}-rg
          subnet: ${infraID}-worker-subnet
          userDataSecret:
            name: windows-user-data
            namespace: openshift-machine-api
          vmSize: Standard_D2s_v3
          vnet: ${infraID}-vnet
          zone: "${az}"
EOF
}

# Retrieves the Cloud Provider for the OpenShift Cluster
provider="$(oc -n openshift-kube-apiserver get configmap config -o json | jq -r '.data."config.yaml"' | jq '.apiServerArguments."cloud-provider"' | jq -r '.[]')"

# Gets the Infrastructure Id for the cluster like `pmahajan-azure-68p9l-gv45m`
infraID="$(oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster)"

# Determines the region based on existing MachinesSets like `us-east-1` for aws or `centralus` for azure
region="$(oc get machines -n openshift-machine-api | grep -w "Running" | awk '{print $4}' | head -1)"

# Determines the availability zone based on existing MachinesSets like `us-east-1a` for aws or `2` for azure
az="$(oc get machines -n openshift-machine-api | grep -w "Running" | awk '{print $5}' | head -1)"

# Creates/deletes a MachineSet for Cloud Provider
case "$provider" in
    aws)
      ms=$(get_aws_ms $infraID $region $az $provider)
    ;;
    azure)
      ms=$(get_azure_ms $infraID $region $az $provider)
    ;;
    *)
      error-exit "platform '$provider' is not yet supported by this script"
    ;;
esac

# If action like apply/delete is provided, directly apply the MachineSet else create a yaml file
if [ -n "$ACTION" ]; then
  if [[ ! "$ACTION" =~ ^apply|delete$ ]]; then
      echo "$ms" > MachineSet.yaml
      error-exit "Action (1st parameter) must be \"apply\" or \"delete\". Creating a yaml file"
  fi
  echo "$ms" | oc $ACTION -n openshift-machine-api -f -
else
  echo "$ms" > MachineSet.yaml
fi
