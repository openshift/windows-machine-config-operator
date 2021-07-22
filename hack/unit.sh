#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
cd "${WMCO_ROOT}"
GOFLAGS=${1:-}

# Run tests from all packages excluding e2e package, as it consists of e2e tests.
go test -v ./pkg/... ${GOFLAGS} -count=1
# version.Get() is required for unit tests, and will return "" unless a value is passed in a build time
go test -v ./controllers/... ${GOFLAGS} -ldflags="-X 'github.com/openshift/windows-machine-config-operator/version.Version=TEST'" -count=1
go test -v ./main.go ./main_test.go ${GOFLAGS} -count=1
exit 0
