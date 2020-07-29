#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

OUTPUT_DIR=${1:-}
GOFLAGS=${2:-}

if [[ -z "$OUTPUT_DIR" ]]; then
    echo "usage: $0 OUTPUT_DIR"
    exit 1
fi

PACKAGE="github.com/openshift/windows-machine-config-operator"
MAIN_PACKAGE="${PACKAGE}/cmd/manager"
BIN_NAME="windows-machine-config-operator"
BIN_DIR="${OUTPUT_DIR}/bin"

VERSION=$(get_version)

echo "building ${BIN_NAME}..."
mkdir -p "${BIN_DIR}"

# Account for environments where GOFLAGS is not set by setting goflags to an empty string if GOFLAGS is not set
goflags=${GOFLAGS:-}


CGO_ENABLED=0 GO111MODULE=on GOOS=linux go build ${GOFLAGS} -ldflags="-X 'github.com/openshift/windows-machine-config-operator/version.Version=${VERSION}'" -o ${BIN_DIR}/${BIN_NAME} ${MAIN_PACKAGE}
