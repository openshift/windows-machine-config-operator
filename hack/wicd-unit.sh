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
for dir in ./pkg/daemon/*/;do
    package=$(basename $dir)
    GOOS=windows GOFLAGS=-v go test -c $dir... -o ${package}_$WICD_UNIT_EXE
done

# Run the unit tests against the Windows host
for file in ./*_$WICD_UNIT_EXE; do
    exe=$(basename $file)
    echo running tests in $exe
    scp -o StrictHostKeyChecking=no -i $KUBE_SSH_KEY_PATH $exe $INSTANCE_USERNAME@$INSTANCE_ADDRESS:$exe
    ssh -o StrictHostKeyChecking=no -i $KUBE_SSH_KEY_PATH $INSTANCE_USERNAME@$INSTANCE_ADDRESS "$exe"
done

