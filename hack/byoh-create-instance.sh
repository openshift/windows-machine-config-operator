#!/bin/bash
#
# Create a Windows BYOH (Bring Your Own Host) instance in OpenShift 4.x
#
# This script creates a Windows Server VM on that can be used as a Windows BYOH node.
# It automatically extracts configuration and creates or updates the
# windows-instances configmap with the private IP and the username of the new instance.
#
# Prerequisites:
# - WMCO installed and running with windows-user-data secret created
# - Azure CLI (az) installed and authenticated
# - OpenShift CLI (oc) installed and authenticated to target cluster
# - jq utility for JSON parsing
# - base64 utility for decoding user data
#
# Usage:
#   ./byoh-create-instance.sh
#
# Environment Variables (optional):
#   WINDOWS_SERVER_VERSION - Windows Server version (default: 2019)
#   DEBUG_FLAG - Set to "--debug" to enable debug output (default: "")
#

# exit the script if any command fails
set -eo pipefail

# check platform
PLATFORM=$(oc get infrastructure cluster -o jsonpath='{.spec.platformSpec.type}')
case "$PLATFORM" in
    "Azure")
        echo "Platform: $PLATFORM"
        ;;
    *)
        echo "Error: platform not supported: $PLATFORM" >&2
        exit 1
        ;;
esac

# Read Windows Server version from environment variable, default to 2019
WINDOWS_SERVER_VERSION=${WINDOWS_SERVER_VERSION:-2019}

INSTANCE_NAME_SUFFIX=$(dd if=/dev/urandom bs=1 count=100 2>/dev/null | tr -dc 'a-z0-9' | head -c 4)
INSTANCE_NAME="byoh-${WINDOWS_SERVER_VERSION}-${INSTANCE_NAME_SUFFIX}"
SKU="${WINDOWS_SERVER_VERSION}-datacenter-smalldisk"
PUBLISHER="MicrosoftWindowsServer"
OFFER="WindowsServer"

# default to latest version for Windows Server 2022
VERSION="latest"
if [ "$WINDOWS_SERVER_VERSION" = "2019" ]; then
    # fix version for Windows Server 2019 otherwise
    VERSION="17763.6293.240905"
fi

# build image name
IMAGE="${PUBLISHER}:${OFFER}:${SKU}:${VERSION}"

# fetch platform and network configuration from existing Linux worker machine
LINUX_WORKER_SPECS=$(oc get machines -n openshift-machine-api -l machine.openshift.io/cluster-api-machine-role=worker -ojsonpath={.items[0].spec})
REGION=$(echo "$LINUX_WORKER_SPECS" | jq -r .providerSpec.value.location)
VNET_NAME=$(echo "$LINUX_WORKER_SPECS" | jq -r .providerSpec.value.vnet)
SUBNET_NAME=$(echo "$LINUX_WORKER_SPECS" | jq -r .providerSpec.value.subnet)
ZONE_NUMBER=$(echo "$LINUX_WORKER_SPECS" | jq -r .providerSpec.value.zone)

# Decode Windows PowerShell bootstrap script
WINDOWS_USER_DATA=$(oc get secret windows-user-data -n openshift-machine-api -o jsonpath='{.data.userData}' | base64 -d)
# remove powershell tags
WINDOWS_USER_DATA=$(echo "$WINDOWS_USER_DATA" | sed 's/<powershell>//' | sed 's/<\/powershell>//')
# remove persist tags and content
WINDOWS_USER_DATA=$(echo "${WINDOWS_USER_DATA}" | sed 's/<persist>true<\/persist>//')

RESOURCE_GROUP=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.azure.resourceGroupName}')

ADMIN_USERNAME="capi"
ADMIN_PASSWORD=$(dd if=/dev/urandom bs=1 count=101 2>/dev/null | tr -dc 'a-z0-9A-Z' | head -c 18)!

DEBUG_FLAG=${DEBUG_FLAG:-""}


echo "Creating VM '${INSTANCE_NAME}' with image '${IMAGE}' in resource group '${RESOURCE_GROUP}'"
az vm create ${DEBUG_FLAG}  \
    --resource-group "${RESOURCE_GROUP}" \
    --name "${INSTANCE_NAME}" \
    --image "${IMAGE}" \
    --location "${REGION}" \
    --admin-username "${ADMIN_USERNAME}" \
    --admin-password "${ADMIN_PASSWORD}" \
    --public-ip-address "" \
    --public-ip-sku Standard \
    --size "Standard_B2s" \
    --os-disk-size-gb 128 \
    --vnet-name "${VNET_NAME}" \
    --subnet "${SUBNET_NAME}" \
    --zone "${ZONE_NUMBER}"


echo "Installing user data..."

USER_DATA_OUTPUT=$(az vm run-command invoke \
    --resource-group "${RESOURCE_GROUP}" \
    --name "${INSTANCE_NAME}" \
    --command-id "RunPowerShellScript" \
    --scripts "${WINDOWS_USER_DATA}"
)

if [ -n "${DEBUG_FLAG}" ]; then
    echo "User data installation output:"
    echo "${USER_DATA_OUTPUT}" | jq -r '.value[].message' | sed 's/\\n/\n/g'
    echo "${USER_DATA_OUTPUT}" | jq -r '.value[].code'
    echo "${USER_DATA_OUTPUT}" | jq -r '.value[].displayStatus'
fi


PRIVATE_IPS=$(az vm show \
    --resource-group "${RESOURCE_GROUP}" \
    --name "${INSTANCE_NAME}" \
    --show-details \
    --query "privateIps" \
    --output tsv)

PRIVATE_IP=${PRIVATE_IPS%% *}
if [ -n "${DEBUG_FLAG}" ]; then
    echo "Found private IPs: ${PRIVATE_IPS}"
    echo "Using private IP: ${PRIVATE_IP}"
fi


echo "Adding entry to windows-instances configmap with address ${PRIVATE_IP} and username ${ADMIN_USERNAME}"

if oc get configmap windows-instances -n openshift-windows-machine-config-operator >/dev/null 2>&1; then
    oc patch configmap windows-instances -n openshift-windows-machine-config-operator \
        --type merge \
        -p "{\"data\":{\"${PRIVATE_IP}\": \"username=${ADMIN_USERNAME}\"}}"
else
    echo "Creating new windows-instances ConfigMap..."
    envsubst <<EOF | oc apply -f -
kind: ConfigMap
apiVersion: v1
metadata:
  name: windows-instances
  namespace: openshift-windows-machine-config-operator
data:
   ${PRIVATE_IP}: |-
    username=${ADMIN_USERNAME}
EOF
fi


echo ""
echo ""
echo "To remove the instance from the cluster, run the following command:       "
echo ""
echo "   oc delete configmap windows-instances -n openshift-windows-machine-config-operator"
echo ""
echo ""

echo ""
echo ""
echo "To delete the instance, run the following command:    "
echo ""
echo "   az vm delete --resource-group ${RESOURCE_GROUP} --name ${INSTANCE_NAME} --yes --no-wait"
echo ""
echo ""

# success
exit 0
