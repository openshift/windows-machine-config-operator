#!/bin/bash
# verify-ote-discovery.sh — build the OTE extension binary and verify that
# Ginkgo test discovery succeeds.  Catches signature errors (e.g. missing
# SpecContext parameter) that silently break all OTE test suites.
set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${WMCO_ROOT}"

echo "==> Running OTE callback-signature static checks..."
(cd ote && GOFLAGS="" GOWORK=off go test -v -run TestSpecTimeoutCallbackSignatures -count=1 ./cmd/wmco-tests-ext/)

echo "==> Building wmco-tests-ext..."
make build-tests-ext

BINARY="build/_output/bin/wmco-tests-ext"
if [ ! -x "${BINARY}" ]; then
    echo "ERROR: wmco-tests-ext binary not found at ${BINARY}"
    exit 1
fi

# The k8s test framework requires KUBECONFIG to be set; create a stub
# so the binary can initialize without a real cluster connection.
FAKE_KUBECONFIG=$(mktemp)
trap 'rm -f -- "$FAKE_KUBECONFIG"' EXIT
export KUBECONFIG="${FAKE_KUBECONFIG}"

echo "==> Verifying OTE component registration (list components)..."
COMPONENTS=$("${BINARY}" list components 2>&1) || {
    echo "ERROR: wmco-tests-ext list components failed"
    echo "${COMPONENTS}"
    exit 1
}

if ! echo "${COMPONENTS}" | grep -q "windows-machine-config-operator"; then
    echo "ERROR: expected component 'windows-machine-config-operator' not found"
    echo "${COMPONENTS}"
    exit 1
fi

echo "==> OTE discovery verification passed"
