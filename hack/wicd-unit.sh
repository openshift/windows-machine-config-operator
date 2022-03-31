#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# Placeholder stub for WICD unit tests
# Currently requiring Windows VM env variables to be set, in order to validate release job functionality
if [ -z "$INSTANCE_ADDRESS" ]; then
    echo "Windows VM address not provided"
    return 1
fi
if [ -z "$INSTANCE_USERNAME" ]; then
    echo "Windows VM username not provided"
    return 1
fi
if [ -z "$KUBE_SSH_KEY_PATH" ]; then
    echo "env KUBE_SSH_KEY_PATH not found"
    return 1
fi

# Ensure the image used in the release job has ssh installed, as this will be used to remotely run tests against a
# Windows VM. Running a dummy command to ensure connection to the VM is possible
ssh -o StrictHostKeyChecking=no -i $KUBE_SSH_KEY_PATH $INSTANCE_USERNAME@$INSTANCE_ADDRESS "dir"

