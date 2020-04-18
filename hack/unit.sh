#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
cd "${WMCO_ROOT}"

# Account for environments where GOFLAGS is not set by setting goflags to an empty string if GOFLAGS is not set
goflags=${GOFLAGS:-}

# The golang 1.13 image used in CI enforces vendoring. Workaround that by unsetting it.
if [[ "$goflags" == *"-mod=vendor"* ]]; then
  unset GOFLAGS
fi

# Run tests from all packages excluding e2e package, as it consists of e2e tests.
go test -v ./cmd/... ./pkg/... -count=1
exit 0
