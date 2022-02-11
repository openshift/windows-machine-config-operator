#!/usr/bin/env bash

set -euo pipefail

function help() {
    echo "USAGE: machineset.sh [OPTIONS]... (apply|delete)"
    echo "Creates a Windows MachineSet.yaml file for Windows Machine Config Operator on an OpenShift cluster"
    echo ""
    echo "OPTIONS:"
    echo "-v=        Windows Server version of VMs associated with the resulting MachineSet. Defaults to 2022"
    echo "-a         create or apply changes to the MachineSet resource"
    echo "-d         delete the MachineSet resource"
    echo ""
    echo "PREREQUISITES:"
    echo "oc         to fetch cluster info and apply/delete MachineSets on the cluster(cluster should be logged in)"
    echo "aws        to fetch Windows AMI id for AWS platform (only required for clusters running on AWS)"
    echo ""
    echo "Examples:"
    echo "machineset.sh                 # create Windows Server 2022 MachineSet yaml"
    echo "machineset.sh -a              # create Windows Server 2022 MachineSet yaml and apply changes to k8s resource"
    echo "machineset.sh -v 2019 -a      # create Windows Server 2019 MachineSet yaml and apply changes to k8s resource"
}

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

# get_spec returns the template yaml common for all cloud providers
get_spec() {

  if [ "$#" -lt 3 ]; then
    error-exit incorrect parameter count for get_spec $#
  fi

  local infraID=$1
  local az=$2
  local provider=$3

  # set machineset name, short name for Azure and vSphere due to
  # the limit in the number of characters for VM name
  machineSetName="winworker"
  # check provider
  if [ "$provider" = "aws" ]; then
    # improve machineset name for aws provider
    machineSetName="$infraID"-"$machineSetName"-"$az"
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
    error-exit incorrect parameter count for get_aws_ms $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4

  # get proper Name for platform-compatible image
  image_name=""
  case "${WIN_SRV,,}" in
    2022)
      image_name="Windows_Server-2022*English*Full*Containers"
    ;;
    2019)
      image_name="Windows_Server-2019*English*Full*Containers"
    ;;
    20h2)
      echo "Note: Windows Server ${WIN_SRV} is not supported for platform ${provider}"
      image_name="Windows_Server-20H2*English*Core*Containers"
    ;;
    *)
      error-exit "Unsupported Windows Server version: ${WIN_SRV}"
    ;;
  esac

  # get the AMI id for the Windows VM
  ami_id=$(aws ec2 describe-images --region ${region} --filters "Name=name,Values=${image_name}*" "Name=is-public,Values=true" --query "reverse(sort_by(Images, &CreationDate))[*].{name: Name, id: ImageId}" --output json | jq -r '.[0].id')
  if [ -z "$ami_id" ]; then
        error-exit "unable to find AMI ID for Windows Server ${WIN_SRV}"
  fi

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
    error-exit incorrect parameter count for get_azure_ms $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4
  
  # get proper SKU for platform-compatible image
  sku=""
  case "${WIN_SRV,,}" in
    2022)
      # has containers GA: https://docs.microsoft.com/en-us/windows-server/get-started/editions-comparison-windows-server-2022
      sku="2022-datacenter-core"
    ;;
    2019)
      sku="2019-Datacenter-with-Containers"
    ;;
    20h2)
      echo "Note: Windows Server ${WIN_SRV} is not supported for platform ${provider}"
      sku="datacenter-core-20h2-with-containers-smalldisk"
    ;;
    *)
      error-exit "Unsupported Windows Server version: ${WIN_SRV}"
    ;;
  esac
  
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
            sku: ${sku}
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

# get_vsphere_ms creates a MachineSet for vSphere Cloud Provider
get_vsphere_ms() {

  if [ "$#" -lt 2 ]; then
    error-exit incorrect parameter count for get_vsphere_ms $#
  fi

  local infraID=$1
  local provider=$2

  # set golden image template name
  template=""
  case "${WIN_SRV,,}" in
    2022)
      template="windows-golden-images/windows-server-2022-template"
    ;;
    2019)
      echo "Note: Windows Server ${WIN_SRV} is not supported for platform ${provider}"
      template="windows-golden-images/vm-winsrv-1909-golden-image"
    ;;
    20h2)
      template="jvaldes/windows-server-20h2-template"
    ;;
    *)
      error-exit "Unsupported Windows Server version: ${WIN_SRV}"
    ;;
  esac

  # TODO: Reduce the number of API calls, make just one call
  #       to `oc get machines` and pass the data around. This is the
  #       3rd call being introduced across the script and can be avoided
  providerSpec=$(oc get machines \
                -n openshift-machine-api \
                -l machine.openshift.io/cluster-api-machine-role=worker \
                -o jsonpath="{.items[0].spec.providerSpec.value}" \
  ) || {
    error-exit "error getting providerSpec for ${provider} cluster ${infraID}"
  }
  if [ -z "$providerSpec" ]; then
    error-exit "cannot find providerSpec for ${provider} cluster ${infraID}"
  fi
  # get credentialsSecret
  credentialsSecret=$(echo "${providerSpec}" | jq -r '.credentialsSecret.name')
  # get network name TODO: review when devices > 1
  networkName=$(echo "${providerSpec}" | jq -r '.network.devices[0].networkName')
  # get workspace specs
  datacenter=$(echo "${providerSpec}" | jq -r '.workspace.datacenter')
  datastore=$(echo "${providerSpec}" | jq -r '.workspace.datastore')
  folder=$(echo "${providerSpec}" | jq -r '.workspace.folder')
  resourcePool=$(echo "${providerSpec}" | jq -r '.workspace.resourcePool')
  server=$(echo "${providerSpec}" | jq -r '.workspace.server')
  # build machineset
  cat <<EOF
$(get_spec $infraID "" $provider)
      providerSpec:
        value:
          apiVersion: vsphereprovider.openshift.io/v1beta1
          credentialsSecret:
            name: ${credentialsSecret}
          diskGiB: 128
          kind: VSphereMachineProviderSpec
          memoryMiB: 16384
          network:
            devices:
            - networkName: ${networkName}
          numCPUs: 4
          numCoresPerSocket: 1
          snapshot: ""
          template: ${template}
          workspace:
            datacenter: ${datacenter}
            datastore: ${datastore}
            folder: ${folder}
            resourcePool: ${resourcePool}
            server: ${server}
EOF
}

WIN_SRV="2022"
ACTION=""
while getopts "v:adh" opt; do
    case "$opt" in
    v) WIN_SRV=$OPTARG;;
    a) ACTION="apply";;
    d) ACTION="delete";;
    h) help; exit 0;;
    ?) help; exit 1;;
    esac
done

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
    vsphere)
      ms=$(get_vsphere_ms $infraID $provider)
    ;;
    *)
      error-exit "platform '$provider' is not yet supported by this script"
    ;;
esac

# If action like apply/delete is provided, directly apply the MachineSet else create a yaml file
if [ -n "$ACTION" ]; then
  if [[ ! "$ACTION" =~ ^apply|delete$ ]]; then
      echo "$ms" > MachineSet.yaml
      error-exit "Action (2nd parameter) must be \"apply\" or \"delete\". Creating a yaml file"
  fi
  echo "$ms" | oc $ACTION -n openshift-machine-api -f -
else
  echo "$ms" > MachineSet.yaml
fi
