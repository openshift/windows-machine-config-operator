#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# returns the operator version in `Version-GitHash` format
# Takes a semver as an argument. This should be the version present in the WMCO CSV
add_git_data_to_version() {
  local WMCO_SEMVER=$1
  GIT_COMMIT=$(git rev-parse --short HEAD)
  # The commit hash retreived varies in length, so we will standardize to length received from the operator image
  GIT_COMMIT="${GIT_COMMIT:0:7}"
  FULL_VERSION="${WMCO_SEMVER}-${GIT_COMMIT}"

  # If any files that affect the building of the operator binary have been changed, append "-dirty" to the version.
  if [ -n "$(git status version tools.go go.mod go.sum vendor Makefile build main.go hack pkg controllers --porcelain)" ]; then
    FULL_VERSION="${FULL_VERSION}-dirty"
  fi

  echo $FULL_VERSION
}

WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/hack/common.sh

OUTPUT_DIR=${1:-}
WMCO_SEMVER=${2}
GOFLAGS=${3:-}

if [[ -z "$OUTPUT_DIR" ]] || [[ -z "$WMCO_SEMVER" ]] ; then
    echo "usage: $0 OUTPUT_DIR OPERATOR_VERSION"
    exit 1
fi

WMCO_CMD_DIR="github.com/openshift/windows-machine-config-operator/cmd/operator"
BIN_NAME="windows-machine-config-operator"
BIN_DIR="${OUTPUT_DIR}/bin"

VERSION=$(add_git_data_to_version $WMCO_SEMVER)

echo "building ${BIN_NAME}..."
mkdir -p "${BIN_DIR}"

# Account for environments where GOFLAGS is not set by setting goflags to an empty string if GOFLAGS is not set
goflags=${GOFLAGS:-}


CGO_ENABLED=0 GO111MODULE=on GOOS=linux go build ${GOFLAGS} -ldflags="-X 'github.com/openshift/windows-machine-config-operator/version.Version=${VERSION}'" -o ${BIN_DIR}/${BIN_NAME} ${WMCO_CMD_DIR}
