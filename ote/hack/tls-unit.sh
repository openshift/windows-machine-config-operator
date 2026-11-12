#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

OTE_ROOT=$(dirname "${BASH_SOURCE}")/..
cd "${OTE_ROOT}"

# KUBECONFIG must be set to prevent Origin's compatibility harness from exiting before tests run.
KUBECONFIG=${KUBECONFIG:-/dev/null} GOFLAGS="" GOWORK=off \
  go test ./test/e2e \
    -run 'Test(CommandOutputWithContext|ClusterOperatorsSettled|PollForTLSRecovery|TLSRecoveryAfterRestore|TLSRecoveryRefusalClassification|ReadyWMCOPodName|PollForWMCOManagerTLSLogs)$' \
    -count=1
