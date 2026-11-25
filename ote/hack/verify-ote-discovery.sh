#!/bin/bash
# verify-ote-discovery.sh — build the OTE extension binary and verify that
# Ginkgo test discovery succeeds. Catches signature errors (e.g. a missing
# SpecContext parameter) that silently break every OTE test suite.
set -o errexit
set -o nounset
set -o pipefail

OTE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${OTE_ROOT}"

# Keep every temporary file in one directory, removed on exit even if a later
# step fails.
WORK_DIR=$(mktemp -d)
trap 'rm -rf -- "${WORK_DIR}"' EXIT

# The k8s test framework requires KUBECONFIG to be set; create a stub so the
# AST test and the binary can initialize without a real cluster connection.
FAKE_KUBECONFIG="${WORK_DIR}/kubeconfig"
: > "${FAKE_KUBECONFIG}"
export KUBECONFIG="${FAKE_KUBECONFIG}"

echo "==> Running OTE callback-signature static checks..."
GOFLAGS="" GOWORK=off go test -v -run TestSpecTimeoutCallbackSignatures -count=1 ./cmd/wmco-tests-ext/

echo "==> Building wmco-tests-ext..."
BINARY="${WORK_DIR}/wmco-tests-ext"
# GO_COMPLIANCE_POLICY=exempt_all is required for test binaries per ART compliance policy.
GOFLAGS="" GOWORK=off GO_COMPLIANCE_POLICY="exempt_all" go build -o "${BINARY}" ./cmd/wmco-tests-ext

echo "==> Verifying OTE component registration (list components)..."
COMPONENTS=$("${BINARY}" list components 2>&1) || {
    echo "ERROR: wmco-tests-ext list components failed"
    echo "${COMPONENTS}"
    exit 1
}

# Match the component's JSON name field, not any log line that mentions it.
if ! grep -Eq '"name": *"windows-machine-config-operator"' <<< "${COMPONENTS}"; then
    echo "ERROR: expected component 'windows-machine-config-operator' not found"
    echo "${COMPONENTS}"
    exit 1
fi

echo "==> OTE discovery verification passed"
