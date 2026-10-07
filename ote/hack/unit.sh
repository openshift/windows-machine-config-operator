#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
OTE_ROOT=$(dirname "${BASH_SOURCE}")/..
cd "${OTE_ROOT}"

# KUBECONFIG must be set to prevent Origin's compatibility harness from exiting before tests run.
KUBECONFIG=/dev/null GOFLAGS="" GOWORK=off go test -v ./test/e2e -count=1
