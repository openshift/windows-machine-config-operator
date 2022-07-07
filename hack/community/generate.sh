#!/bin/bash

# Given the current WMCO manifests, generate new community manifests.

# Must be ran within WMCO root.

# Example:
# Cut community v5.1.0 for community-4.10
# Run: bash ./hack/community/generate.sh 5.1.0 community-4.10 community-4.10-hash ../community-operators-prod

# replace necessary fields with the yq tool
replace() {
    local CO_CSV=$1
    local OPERATOR_VERSION=$2
    local DESCRIPTION=$3
    local QUAY_IMAGE=$4
    local CO_ANNOTATIONS=$5
    local CREATED_AT=$(date +"%Y-%m-%dT%H:%M:%SZ")
    local OPERATOR_NAME="community-windows-machine-config-operator"
    local NAME="${OPERATOR_NAME}.v$OPERATOR_VERSION"
    local CO_DESCRIPTION=$DESCRIPTION
    local DISPLAY_NAME="Community Windows Machine Config Operator"
    local IMAGE=$QUAY_IMAGE
    local MATURITY="preview"
    local VERSION="$OPERATOR_VERSION"

    # TODO: description contains \n literals, user will have to manually turn this into a multi-line string
    # Replace CSV fields
    yq eval --exit-status --inplace "
      .metadata.annotations.createdAt |= \"$CREATED_AT\" |
      .metadata.name |= \"$NAME\" |
      .spec.description |= \"$CO_DESCRIPTION\" |
      .spec.displayName |= \"$DISPLAY_NAME\" |
      .spec.install.spec.deployments[0].spec.template.spec.containers[0].env[2].value |= \"$OPERATOR_NAME\" |
      .spec.install.spec.deployments[0].spec.template.spec.containers[0].image |= \"$IMAGE\" |
      .spec.maturity |= \"$MATURITY\" |
      .spec.version |= \"$VERSION\"
    " "${CO_CSV}"

    # Delete the subscription line
    yq eval --exit-status --inplace 'del(.metadata.annotations."operators.openshift.io/valid-subscription")' "${CO_CSV}"

    # Delete the testing line
    yq eval --exit-status --inplace 'del(.annotations."operators.operatorframework.io.test.config.v1")' "${CO_ANNOTATIONS}"

    # Replace annotations fields
    # TODO: use yq for the annotations.yaml replacement
    sed -i -e "s/"preview,stable"/$MATURITY/" -e "s/"stable"/$MATURITY/" "${CO_ANNOTATIONS}"
}

# copy the WMCO bundle and its contents to a new community folder. Rename files
# as needed.
generate_manifests() {
  local OPERATOR_VERSION=$1
  local BUNDLE_DIR=$2
  local DESCRIPTION=$3
  local QUAY_IMAGE=$4
  local CO_OPERATOR_DIR="$(pwd)/operators/community-windows-machine-config-operator"

  echo "Update operator manifests"
  mkdir -p "${CO_OPERATOR_DIR}/${OPERATOR_VERSION}"
  cp -r "${BUNDLE_DIR}/manifests" "${BUNDLE_DIR}/metadata" "${CO_OPERATOR_DIR}/${OPERATOR_VERSION}"
  mv "${CO_OPERATOR_DIR}/${OPERATOR_VERSION}/manifests/windows-machine-config-operator.clusterserviceversion.yaml" "${CO_OPERATOR_DIR}/${OPERATOR_VERSION}/manifests/community-windows-machine-config-operator.${OPERATOR_VERSION}.clusterserviceversion.yaml"
  local CO_CSV="${CO_OPERATOR_DIR}/${OPERATOR_VERSION}/manifests/community-windows-machine-config-operator.${OPERATOR_VERSION}.clusterserviceversion.yaml"
  local CO_ANNOTATIONS="${CO_OPERATOR_DIR}/${OPERATOR_VERSION}/metadata/annotations.yaml"

  replace "$CO_CSV" "$OPERATOR_VERSION" "$DESCRIPTION" "$QUAY_IMAGE" "$CO_ANNOTATIONS"
}

# Create a signed commit for the community-operators-prod repo
commit() {
  local OPERATOR_VERSION=$1
  TITLE="operators community-windows-machine-config-operator.v$OPERATOR_VERSION"

  echo "commit changes"
  git add --all
  git commit -s -m "${TITLE}"
}

OPERATOR_VERSION="$1"
COMMUNITY_VERSION="$2"
QUAY_IMAGE="$3"
CO_DIR="$4"
OPERATOR_MANIFESTS="bundle/manifests"
OPERATOR_METADATA="bundle/metadata"

if [ -z "$QUAY_IMAGE" ]; then
    echo "Must set QUAY_IMAGE to latest community tag"
    exit 1
fi

if [ -z $CO_DIR ]; then
  echo "CO_DIR not set"
  exit 1
fi

# Check if yq is installed
if ! command -v which yq &> /dev/null; then
  echo "Please install yq before running generate.sh. Directions to install yq are in the readme.md"
  exit 1
fi

echo "Cutting community release for v${OPERATOR_VERSION}"

# Inject appropriate community-version into the description
DESCRIPTION=$(cat hack/community/csv/description.md)
DESCRIPTION=${DESCRIPTION//COMMUNITY_VERSION/$COMMUNITY_VERSION}

BUNDLE_DIR=$(pwd)/bundle
pushd "${CO_DIR}"
git checkout -B "${OPERATOR_VERSION}"
generate_manifests "$OPERATOR_VERSION" "$BUNDLE_DIR" "$DESCRIPTION" "$QUAY_IMAGE"
commit "$OPERATOR_VERSION"
popd