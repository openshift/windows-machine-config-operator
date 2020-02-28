#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

OUTPUT_DIR=${1:-}
if [[ -z "$OUTPUT_DIR" ]]; then
    echo "usage: $0 OUTPUT_DIR"
    exit 1
fi

PACKAGE="github.com/openshift/windows-machine-config-operator"
MAIN_PACKAGE="${PACKAGE}/cmd/manager"
BIN_NAME="windows-machine-config-operator"
BIN_DIR="${OUTPUT_DIR}/bin"

echo "building ${BIN_NAME}..."
mkdir -p "${BIN_DIR}"

# The golang 1.13 image used in CI enforces vendoring. Workaround that by
# unsetting it.
if [[ "$GOFLAGS" == *"-mod=vendor"* ]]; then
  unset GOFLAGS
fi
CGO_ENABLED=0 GO111MODULE=on GOOS=linux go build -o ${BIN_DIR}/${BIN_NAME} ${MAIN_PACKAGE}
