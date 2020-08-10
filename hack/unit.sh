#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
cd "${WMCO_ROOT}"
GOFLAGS=${1:-}

# Run tests from all packages excluding e2e package, as it consists of e2e tests.
go test -v ./cmd/... ./pkg/... ${GOFLAGS} -count=1
exit 0
