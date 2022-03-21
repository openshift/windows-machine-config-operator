#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

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
WICD_UNIT_EXE="wicd_unit.exe"
# Build unit tests for Windows. Specifically targetting packages used by WICD.
GOOS=windows GOFLAGS=-v go test -c ./pkg/winsvc/... -o $WICD_UNIT_EXE

# Run the unit tests against the Windows host
scp -o StrictHostKeyChecking=no -i $KUBE_SSH_KEY_PATH $WICD_UNIT_EXE $INSTANCE_USERNAME@$INSTANCE_ADDRESS:$WICD_UNIT_EXE
ssh -o StrictHostKeyChecking=no -i $KUBE_SSH_KEY_PATH $INSTANCE_USERNAME@$INSTANCE_ADDRESS "$WICD_UNIT_EXE"

