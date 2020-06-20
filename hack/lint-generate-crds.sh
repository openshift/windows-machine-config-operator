#!/bin/bash
set -o errexit
set -o pipefail

WMCO_ROOT=$(pwd)
source $WMCO_ROOT/hack/common.sh

# The golang 1.13 image used in CI enforces vendoring. Workaround that by unsetting it.
if [[ "${OPENSHIFT_CI}" == "true" ]]; then
  unset GOFLAGS
fi

# Ensure all generated files are up to date
OSDK=$(get_operator_sdk)

WMC_CRD_PATH=$WMCO_ROOT/deploy/crds/wmc.openshift.io_windowsmachineconfigs_crd.yaml
WMC_CRD=$(cat $WMC_CRD_PATH)
# operator-sdk v0.18.x generates v1 CRD by default. Using `--crd-version` flag to generate v1beta1 CRD
CRD_GEN="$OSDK generate crds --crd-version=v1beta1"

# Run generator and read new state
$CRD_GEN
GENERATED_WMC_CRD=$(cat $WMC_CRD_PATH)

if [ "$WMC_CRD" != "$GENERATED_WMC_CRD" ]; then
  echo $WMC_CRD_PATH is not up to date. $CRD_GEN needs to be ran
  # Return CRD back to original state
  echo "$WMC_CRD" > $WMC_CRD_PATH
  exit 1
fi
