#!/bin/bash
set -euo pipefail
# This script is used by the aws-e2e-operator-windows-e2e-operator-test periodic and works by taking the current WMCO
# manifests and generating community WMCO manifests to the tests ARTIFACTS directory.

# Include the common.sh
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..
source $WMCO_ROOT/common.sh

# Replace necessary fields with the yq tool.
replace() {
    local CO_CSV=$1
    local DESCRIPTION=$2
    local CO_ANNOTATIONS=$3
    local CREATED_AT=$(date +"%Y-%m-%dT%H:%M:%SZ")
    local CO_DESCRIPTION=$DESCRIPTION
    local DISPLAY_NAME="Community Windows Machine Config Operator"
    local MATURITY="preview"

    # Replace CSV fields
    yq eval --exit-status --inplace "
      .metadata.annotations.createdAt |= \"$CREATED_AT\" |
      .spec.description |= \"$CO_DESCRIPTION\" |
      .spec.displayName |= \"$DISPLAY_NAME\" |
      .spec.maturity |= \"$MATURITY\"
    " "${CO_CSV}"

    # Delete the subscription line as the community WMCO does not require a subscription.
    yq eval --exit-status --inplace 'del(.metadata.annotations."operators.openshift.io/valid-subscription")' "${CO_CSV}"

    # Replace annotations fields
    # TODO: use yq for the annotations.yaml replacement
    sed -i -e "s/"preview,stable"/$MATURITY/" -e "s/"stable"/$MATURITY/" "${CO_ANNOTATIONS}"
}

# Copy the WMCO bundle and its contents to the ARTIFACTS directory.
generate_manifests() {
  local BUNDLE_DIR=$1
  local DESCRIPTION=$2
  local OUTPUT_DIR=$3

  echo "Update operator manifests"
  cp -r "${BUNDLE_DIR}/manifests" "${BUNDLE_DIR}/metadata" "${BUNDLE_DIR}/windows-machine-config-operator.package.yaml" "${OUTPUT_DIR}"
  local CO_CSV="${OUTPUT_DIR}/manifests/windows-machine-config-operator.clusterserviceversion.yaml"
  local CO_ANNOTATIONS="${OUTPUT_DIR}/metadata/annotations.yaml"

  replace "$CO_CSV" "$DESCRIPTION" "$CO_ANNOTATIONS"
}

WMCO_VERSION="$1"
OUTPUT_DIR="$2"

if [ -z $WMCO_VERSION ]; then
  echo "WMCO_VERSION not set"
  exit 1
fi

if [ -z $OUTPUT_DIR ]; then
  echo "OUTPUT_DIR not set"
  exit 1
fi

COMMUNITY_VERSION="community-"$(get_OCP_version "$WMCO_VERSION")

# Inject appropriate community-version into the description
DESCRIPTION=$(cat hack/community/csv/description.md)
DESCRIPTION=${DESCRIPTION//COMMUNITY_VERSION/$COMMUNITY_VERSION}

BUNDLE_DIR=$(pwd)/bundle
generate_manifests "$BUNDLE_DIR" "$DESCRIPTION" "$OUTPUT_DIR"