#!/usr/bin/env bash

# machineset.sh - create a Windows MachineSet for Windows Machine Config Operator
# Creates a MachineSet.yaml file and apply/delete the MachineSet if optional action is provided
#
# USAGE
#    machineset.sh
# OPTIONS
#    -b                      Set 'windowsmachineconfig.openshift.io/ignore' label for BYOH use case. Default: false
#    -w                      Windows Server version (optional) 2019 or 2022. Default: 2022
#    $1/$2 (if -w is used)   Action                 (optional) apply/delete the MachineSet
# PREREQUISITES
#    oc                   to fetch cluster info and apply/delete MachineSets on the cluster(cluster should be logged in)
#    aws                  to fetch Windows AMI id for AWS platform (only required for clusters running on AWS)
set -euo pipefail

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

# get_spec returns the template yaml common for all cloud providers
get_spec() {

  if [ "$#" -lt 4 ]; then
    error-exit incorrect parameter count for get_spec $#
  fi

  local infraID=$1
  local az=$2
  local provider=$3
  local byoh=$4

  # set machineset name, short name for Azure and vSphere due to
  # the limit in the number of characters for VM name
  machineSetName="winworker"
  # update machineset name for BYOH to avoid conflicts
  if [ "$byoh" = "true" ]; then
    machineSetName="winbyoh"
  fi
  # check provider
  if [ "$provider" = "aws" ] || [ "$provider" = "gce" ]; then
    # improve machineset name for aws/gcp provider
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
        windowsmachineconfig.openshift.io/ignore: "${byoh}"
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
EOF
}

# get_aws_ms creates a MachineSet for AWS Cloud Provider
get_aws_ms() {

  if [ "$#" -lt 6 ]; then
    error-exit incorrect parameter count for get_aws_ms $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4
  local winver=$5
  local byoh=$6

  local filter="Windows_Server-2022-English-Core-Base-????.??.??"
  if [ "$winver" == "2019" ]; then
    filter="Windows_Server-2019-English-Core-Base-????.??.??"
  fi

  # get the AMI id for the Windows VM
  ami_id=$(aws ec2 describe-images --region "${region}" --filters "Name=name,Values=$filter" "Name=is-public, Values=true" --query "reverse(sort_by(Images, &CreationDate))[*].{name: Name, id: ImageId}" --output json | jq -r '.[0].id')
  if [ -z "$ami_id" ]; then
        error-exit "unable to find AMI ID for Windows Server 2019 1809"
  fi

  cat <<EOF
$(get_spec $infraID $az $provider $byoh)
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
                    - ${infraID}-node
          subnet:
            filters:
              - name: tag:Name
                values:
                  - ${infraID}-subnet-private-${az}
          tags:
            - name: kubernetes.io/cluster/${infraID}
              value: owned
          userDataSecret:
            name: windows-user-data
EOF
}

# get_azure_ms creates a MachineSet for Azure Cloud Provider
get_azure_ms() {

  if [ "$#" -lt 6 ]; then
    error-exit incorrect parameter count for get_azure_ms $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4
  local winver=$5
  local byoh=$6

  local sku="2022-datacenter-smalldisk"
  local release="latest"
  if [ "$winver" == "2019" ]; then
		# 2019 images without the containers feature pre-installed cannot be used due to
		# https://issues.redhat.com/browse/OCPBUGS-13244
    sku="2019-datacenter-with-containers-smalldisk"
    release="17763.6293.240905"   
  fi

  cat <<EOF
$(get_spec $infraID $az $provider $byoh)
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
            sku: $sku
            version: $release
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

# get_gcp_ms creates a MachineSet for Google Cloud Platform
get_gcp_ms() {
  if [ "$#" -lt 6 ]; then
    error-exit incorrect parameter count for get_gcp_ms $#
  fi

  local infraID=$1
  local region=$2
  local az=$3
  local provider=$4
  local winver=$5
  local byoh=$6

  local image="projects/windows-cloud/global/images/family/windows-2022-core"
  if [ "$winver" == "2019" ]; then
    image="projects/windows-cloud/global/images/family/windows-2019-core"
  fi

  # For GCP the zone field returns the region + zone, like: `us-central1-a`.
  # Installer created MachineSets only append the `-a` portion, so we should do the same.
  local az_suffix=$(echo $az |awk -F "-" '{print $NF}')
  local projectID=$(oc get infrastructure cluster -ojsonpath={.status.platformStatus.gcp.projectID})

  cat <<EOF
$(get_spec $infraID $az_suffix $provider $byoh)
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
            image: $image
            sizeGb: 128
            type: pd-ssd
          kind: GCPMachineProviderSpec
          machineType: n1-standard-4
          networkInterfaces:
          - network: ${infraID}-network
            subnetwork: ${infraID}-worker-subnet
          projectID: ${projectID}
          region: ${region}
          serviceAccounts:
          - email: ${infraID}-w@${projectID}.iam.gserviceaccount.com
            scopes:
            - https://www.googleapis.com/auth/cloud-platform
          tags:
          - ${infraID}-worker
          userDataSecret:
            name: windows-user-data
          zone: ${az}
EOF
}

# get_vsphere_ms creates a MachineSet for vSphere Cloud Provider
get_vsphere_ms() {

  if [ "$#" -lt 3 ]; then
    error-exit incorrect parameter count for get_vsphere_ms $#
  fi

  local infraID=$1
  local provider=$2
  local byoh=$3

  # set golden image template name
  # TODO: read from parameter
  template="windows-golden-images/windows-server-2022-template-ipv6-disabled"

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
$(get_spec $infraID "" $provider $byoh)
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

# get_nutanix_ms creates a MachineSet for Nutanix
get_nutanix_ms() {

  if [ "$#" -lt 3 ]; then
    error-exit incorrect parameter count for get_nutanix_ms $#
  fi

  local infraID=$1
  local provider=$2
  local byoh=$3

  # set Windows Server 2022 image name
  imageName="nutanix-windows-server-openshift.qcow2"
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
  clusterId=$(echo "${providerSpec}" | jq -r '.cluster.uuid')
  credentialsSecret=$(echo "${providerSpec}" | jq -r '.credentialsSecret.name')
  projectType=$(echo "${providerSpec}" | jq -r '.project.type')
  subnetId=$(echo "${providerSpec}" | jq -r '.subnets[0].uuid')
  # build machineset
  cat <<EOF
$(get_spec $infraID "" $provider $byoh)
      providerSpec:
        value:
          apiVersion: machine.openshift.io/v1
          bootType: ""
          categories: null
          cluster:
            type: uuid
            uuid: ${clusterId}
          credentialsSecret:
            name: ${credentialsSecret}
          failureDomain: null
          image:
            name: ${imageName}
            type: name
          kind: NutanixMachineProviderConfig
          memorySize: 16Gi
          project:
            type: ${projectType}
          subnets:
          - type: uuid
            uuid: ${subnetId}
          systemDiskSize: 120Gi
          userDataSecret:
            name: windows-user-data
          vcpuSockets: 4
          vcpusPerSocket: 1
EOF
}

winver="2022"
byoh=false
while getopts ":w:b" opt; do
  case ${opt} in
    w ) # Windows Server version to use in the MachineSet. Defaults to 2022. Other option is 2019.
      winver="$OPTARG"
      if [[ ! "$winver" =~ 2019|2022$ ]]; then
        echo "Invalid -w option $winver. Valid options are 2019 or 2022"
        exit 1
      fi
      ;;
    b )
      byoh=true
      ;;
    \? )
      echo "Usage: $0 -w <2019/2022> -b apply/delete"
      exit 0
      ;;
  esac
done

# Remove all options parsed by getopts
shift $((OPTIND -1))
ACTION=${1:-}

if [ -n "$ACTION" ]; then
  if [[ ! "$ACTION" =~ ^apply|delete$ ]]; then
    error-exit "Action (1st parameter) must be \"apply\" or \"delete\""
  fi
fi

# Retrieves the Cloud Provider for the OpenShift Cluster
platform="$(oc get infrastructure cluster -ojsonpath={.spec.platformSpec.type})"

# Gets the Infrastructure Id for the cluster like `pmahajan-azure-68p9l-gv45m`
infraID="$(oc get -o jsonpath='{.status.infrastructureName}{"\n"}' infrastructure cluster)"

# Determines the region based on existing MachinesSets like `us-east-1` for aws or `centralus` for azure
region="$(oc get machines -n openshift-machine-api | grep -w "Running" | awk '{print $4}' | head -1)"

# Determines the availability zone based on existing MachinesSets like `us-east-1a` for aws or `2` for azure
az="$(oc get machines -n openshift-machine-api | grep -w "Running" | awk '{print $5}' | head -1)"

# Creates/deletes a MachineSet for Cloud Provider
case "$platform" in
    AWS)
      ms=$(get_aws_ms $infraID $region $az $platform $winver $byoh)
    ;;
    Azure)
      ms=$(get_azure_ms $infraID $region $az $platform $winver $byoh)
    ;;
    GCP)
      ms=$(get_gcp_ms $infraID $region $az $platform $winver $byoh)
    ;;
    VSphere)
      ms=$(get_vsphere_ms $infraID $platform $byoh)
    ;;
    Nutanix)
      ms=$(get_nutanix_ms $infraID $platform $byoh)
    ;;
    *)
      error-exit "platform '$platform' is not yet supported by this script"
    ;;
esac

# If action like apply/delete is provided, directly apply the MachineSet else create a yaml file
if [ -n "$ACTION" ]; then
  echo "$ms" | oc "$ACTION" -n openshift-machine-api -f -
else
  echo "$ms" > MachineSet.yaml
fi
