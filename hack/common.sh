#!/bin/bash

# defines the operator-sdk version used across the hack scripts, e.g. pre-release.sh
export OPERATOR_SDK_VERSION=v1.32.0

# Default is false unless test is running in COMMUNITY branch
COMMUNITY=${COMMUNITY:-false}

# Location of the manifests file
MANIFEST_LOC=bundle/
if [ "$COMMUNITY" = "true" ]; then
  MANIFEST_LOC="$ARTIFACT_DIR"
fi

# define namespace
WMCO_DEPLOY_NAMESPACE=${WMCO_DEPLOY_NAMESPACE:="openshift-windows-machine-config-operator"}

error-exit() {
    echo "Error: $*" >&2
    exit 1
}

# Accepts one argument, the WMCO version.
get_OCP_version() {
  if [ "$#" -ne 1 ]; then
    echo incorrect parameter count for get_OCP_version $#
    return 1
  fi
  local WMCO_VERSION=$1
  local OCP_VER_MAJOR=4
  local WMCO_VER_MAJOR=$(echo $WMCO_VERSION | cut -d. -f1)
  # OCP 4.6 maps to WMCO 1.y.z making the WMCO major version always five
  # versions behind OCP Y version
  local DIFFERENCE=5
  local OCP_VER_MINOR=$(($DIFFERENCE+$WMCO_VER_MAJOR))
  # starting on WMCO 10.y.z, the WMCO y-stream follows OCP y-stream
  if [ "$WMCO_VER_MAJOR" -ge 10 ]; then
    WMCO_VER_MINOR=$(echo $WMCO_VERSION | cut -d. -f2)
    OCP_VER_MINOR=${WMCO_VER_MINOR}
  fi
  echo $OCP_VER_MAJOR.$OCP_VER_MINOR
}

# Accepts one argument, the OCP version.
get_rhel_version(){
  if [ "$#" -lt 1 ]; then
    echo incorrect parameter count for get_rhel_version $#
    return 1
  fi
  version=$(echo "$1" | tr -d '.')
  if [[ $version -ge 413 ]]; then
    echo "9"
  else
    echo "8"
  fi
}

check_git_branch() {
  # Return if the working branch is dirty
  if ! git diff --quiet; then
    echo "branch dirty, exiting to not overwrite work"
    return 1
  fi
}

# This function creates a git branch for automating commits
# Takes n arguments
# 1st arg: the branch name prefix, such as submodule-update
# args [2, n]: the versions to branch from
# usage: create_git_branch_list submodule-update release-4.10 release-4.12 release-4.13
create_git_branch_list() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for create_git_branch_list $#
    return 1
  fi

  branch_name_prefix=$1
  new_branch_list=()

  # skip the first argument because that's the branch ID
  shift
  for branch in "$@"; do
    new_branch="$branch-$branch_name_prefix-$(date +%m-%d)"
    new_branch_list+=($new_branch)
  done
  echo "${new_branch_list[@]}"
}

# This function switches the current git branch, and deletes branches with duplicate names.
# takes 2 arguments
# the base branch, and the branch that is going to be created
# usage: switch_git_branch master new_branch_name
switch_git_branch() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for switch_git_branch $#
    return 1
  fi
  local base_branch=$1
  local new_branch=$2

  if git branch -D $new_branch; then
    echo "Deleted branch $new_branch"
  fi
  git checkout $base_branch -b $new_branch
} 

# This function adds files and generates a commit message 
# takes n arguments
# 1st arg: the commit message to add to the commits 
# args [2, n]: the directories to add to the commit 
# usage: generate_commit "good commit message" hack bundle 
generate_commit() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for generate_commit $#
    return 1
  fi

  local commit_message="$1"
  shift
  for directory in "$@"; do
    git add $directory
  done

  # Commit changes if there are any
  git commit -m "$commit_message"
}

# This function displays a message to remind the user to push their changes. 
# takes 1 arg
# 1st arg: the list of branches to display 
# usage: display_push_message ${new_branch_list[@]}
display_push_message() {
  new_branch_list=("$@")
  echo "****"
  echo "New branches created:"
  echo "    [${new_branch_list[@]}]"
  echo ""
  echo "For each branch, you may push to your fork and create a PR against openshift/windows-machine-config-operator"
  echo "example:"
  echo "# assumes you have your fork set as the remote 'origin'"
  echo "git push origin ${new_branch_list[0]}"

}

get_operator_sdk() {
  # Download the operator-sdk binary only if it is not already available
  # We do not validate the version of operator-sdk if it is available already
  if type operator-sdk >/dev/null 2>&1; then
    which operator-sdk
    return
  fi

  OPERATOR_SDK_DOWNLOAD_DIR=$(mktemp -d)
  OPERATOR_SDK_DOWNLOAD_URL="https://github.com/operator-framework/operator-sdk/releases/download"
  # TODO: Make this download the same version we have in go dependencies in gomod
  wget --no-verbose "${OPERATOR_SDK_DOWNLOAD_URL}"/"${OPERATOR_SDK_VERSION}"/operator-sdk_linux_amd64 \
    -O "${OPERATOR_SDK_DOWNLOAD_DIR}"/operator-sdk \
    -o "${OPERATOR_SDK_DOWNLOAD_DIR}"/operator-sdk.log || {
      echo "Failed to download operator-sdk version ${OPERATOR_SDK_VERSION}"
      return
  }
  chmod +x "${OPERATOR_SDK_DOWNLOAD_DIR}"/operator-sdk || {
    echo "Failed to make operator-sdk executable"
    return
  }
  echo "${OPERATOR_SDK_DOWNLOAD_DIR}/operator-sdk"
}

get_packagemanifests_version() {
  # Find the line that has a semver pattern in it, such as v2.0.0
  local VERSION=$(grep -o 'v[0-9]\+\.[0-9]\+\.[0-9]\+' $MANIFEST_LOC/windows-machine-config-operator.package.yaml)
  # return the version without the 'v' at the beginning.
  echo ${VERSION:1}
}

# This function runs operator-sdk run --olm/cleanup depending on the given parameters
# Parameters:
# 1: command to run [run/cleanup]
# 2: path to the operator-sdk binary to use
# 3: OPTIONAL path to the directory holding the temporary CSV with image field replaced with operator image
OSDK_WMCO_management() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for OSDK_WMCO_management $#
    return 1
  fi
  if [[ "$1" != "run" && "$1" != "cleanup" ]]; then
    echo $1 does not match either run or cleanup
    return 1
  fi

  local COMMAND=$1
  local OSDK_PATH=$2

  if [[ "$COMMAND" = "run" ]]; then
    local version=$(get_packagemanifests_version)
    $OSDK_PATH run packagemanifests $MANIFEST_LOC \
      --namespace $WMCO_DEPLOY_NAMESPACE \
      --install-mode=OwnNamespace \
      --version $version \
      --timeout 5m
  fi
  if [[ "$COMMAND" = "cleanup" ]]; then
    $OSDK_PATH cleanup windows-machine-config-operator \
      --namespace $WMCO_DEPLOY_NAMESPACE \
      --timeout 5m
  fi
}

build_WMCO() {
  local OSDK=$1
  
  if [ -z "$OPERATOR_IMAGE" ]; then
      error-exit "OPERATOR_IMAGE not set"
  fi

  $CONTAINER_TOOL build . -t "$OPERATOR_IMAGE" -f build/Dockerfile $noCache
  if [ $? -ne 0 ] ; then
      error-exit "failed to build operator image"
  fi

  $CONTAINER_TOOL push "$OPERATOR_IMAGE"
  if [ $? -ne 0 ] ; then
      error-exit "failed to push operator image to remote repository"
  fi
}

# Updates the manifest file with the operator image, prepares the cluster to run the operator and
# runs the operator on the cluster using OLM
# Parameters:
# 1: path to the operator-sdk binary to use
# 2 (optional): private key path. This is typically used only from olm.sh, to avoid having to manually create the key.
run_WMCO() {
  if [ "$#" -lt 1 ]; then
      echo incorrect parameter count for run_WMCO $#
      return 1
  fi

  local OSDK=$1
  local PRIVATE_KEY=""
  if [ "$#" -eq 2 ]; then
      local PRIVATE_KEY=$2
  fi

  transform_csv REPLACE_IMAGE $OPERATOR_IMAGE
  transform_csv "imagePullPolicy: IfNotPresent" "imagePullPolicy: Always"

  # Validate the operator bundle manifests
  $OSDK bundle validate $MANIFEST_LOC
  if [ $? -ne 0 ] ; then
      error-exit "operator bundle validation failed"
  fi


  # create the deploy namespace if it does not exist
  if ! oc get ns $WMCO_DEPLOY_NAMESPACE; then
      if ! oc create ns $WMCO_DEPLOY_NAMESPACE; then
          return 1
      fi
  fi

  # required labels for WMCO namespace
  declare -a NS_LABELS=(
    # enable monitoring
    "openshift.io/cluster-monitoring=true"
    # turn on the automatic label synchronization required for PodSecurity admission
    "security.openshift.io/scc.podSecurityLabelSync=true"
    # set pods security profile to privileged. See https://kubernetes.io/docs/concepts/security/pod-security-admission/#pod-security-levels
    "pod-security.kubernetes.io/enforce=privileged"
  )
  # apply required labels to WMCO namespace
  if ! oc label ns "${WMCO_DEPLOY_NAMESPACE}" "${NS_LABELS[@]}" --overwrite; then
    error-exit "error setting labels ${NS_LABELS[@]} in namespace ${WMCO_DEPLOY_NAMESPACE}"
  fi

  if [ -n "$PRIVATE_KEY" ]; then
      if ! oc get secret cloud-private-key -n $WMCO_DEPLOY_NAMESPACE; then
          echo "Creating private-key secret"
          if ! oc create secret generic cloud-private-key --from-file=private-key.pem="$PRIVATE_KEY" -n $WMCO_DEPLOY_NAMESPACE; then
              return 1
          fi
      fi
  fi

  # Run the operator in the given namespace
  OSDK_WMCO_management run $OSDK

  enable_debug_logging

  # Additional guard that ensures that operator was deployed given the SDK flakes in error reporting
  if ! oc rollout status deployment windows-machine-config-operator -n $WMCO_DEPLOY_NAMESPACE --timeout=5s; then
    return 1
  fi
}

# Reverts the changes made in manifests file and cleans up the installation of operator from the cluster and deletes the namespace
# Parameters:
# 1: path to the operator-sdk binary to use
cleanup_WMCO() {
  local OSDK=$1

  # Cleanup the operator and revert changes made to the csv
  if ! OSDK_WMCO_management cleanup $OSDK; then
      revert_csv
      error-exit "operator cleanup failed"
  fi
  revert_csv

  # Remove the declared namespace
  oc delete ns $WMCO_DEPLOY_NAMESPACE
}

# Given two parameters, replaces the value in first parameter with the second in the csv.
# Parameters:
# 1: parameter to determine value to be replaced in the csv
# 2: parameter with new value to be replaced with in the csv
transform_csv() {
  if [ "$#" -lt 2 ]; then
    echo incorrect parameter count for replace_csv_value $#
    return 1
  fi
  sed -i "s|$1|$2|g" $MANIFEST_LOC/manifests/windows-machine-config-operator.clusterserviceversion.yaml
}

# Revert the CSV back to it's original form
revert_csv() {
    transform_csv $OPERATOR_IMAGE REPLACE_IMAGE
    transform_csv "imagePullPolicy: Always" "imagePullPolicy: IfNotPresent"
}

# creates the `windows-instances` ConfigMap
# Parameters:
#  1: the ConfigMap data section
createWindowsInstancesConfigMap() {
  DATA=$1
  if [[ -z "$DATA" ]]; then
    error-exit "ConfigMap data cannot be empty"
  fi
  cat <<EOF | oc apply -f -
kind: ConfigMap
apiVersion: v1
metadata:
  name: windows-instances
  namespace: ${WMCO_DEPLOY_NAMESPACE}
${DATA}
EOF
}

# returns the number of instances from `windows-instances` ConfigMap by
# counting the number of values in the data section
getWindowsInstanceCountFromConfigMap() {
 oc get configmaps \
   windows-instances \
   -n "${WMCO_DEPLOY_NAMESPACE}" \
   -o jsonpath='{.data.*}' | wc -w
}

# creates the a job and required RBAC to check the number of Windows nodes performing
# parallel upgrade in the test cluster
createParallelUpgradeCheckerResources() {
  winNodesCount=$(oc get nodes -l kubernetes.io/os=windows  -o jsonpath='{.items[*].metadata.name}' | wc -w)
  if [[ winNodesCount -lt 2 ]]; then
    echo "Skipping parallel upgrade checker job, requires 2 or more nodes. Found ${winNodesCount} nodes"
    return
  fi
  # get the latest tools image from the image stream
  export TOOLS_IMAGE=$(oc get imagestreamtag tools:latest -n openshift -o jsonpath='{.tag.from.name}')
  # set job' container image and create the job
  JOB=$(sed -e "s|REPLACE_WITH_OPENSHIFT_TOOLS_IMAGE|${TOOLS_IMAGE}|g" hack/e2e/resources/parallel-upgrade-checker-job.yaml)
  cat <<EOF | oc apply -f -
${JOB}
EOF
}

# creates the a job and required RBAC to check the number of Windows nodes performing
# parallel upgrade in the test cluster
deleteParallelUpgradeCheckerResources() {
  oc delete -f hack/e2e/resources/parallel-upgrade-checker-job.yaml || {
    echo "error deleting parallel upgrade checker job"
  }
}


enable_debug_logging() {
  if [[ $(oc get -n $WMCO_DEPLOY_NAMESPACE pod -l name=windows-machine-config-operator -ojson) == *"--debugLogging"* ]]; then
     echo "Debug logging already enabled"
    return 0
  fi

  # Detect OLM version and patch accordingly
  WMCO_SUB=$(oc get sub -n "$WMCO_DEPLOY_NAMESPACE" --no-headers 2>/dev/null | awk '{print $1}')
  if [[ -n "$WMCO_SUB" ]]; then
    echo "Detected OLMv0, patching subscription $WMCO_SUB"
    oc patch subscription $WMCO_SUB -n $WMCO_DEPLOY_NAMESPACE --type=merge -p '{"spec":{"config":{"env":[{"name":"ARGS","value":"--debugLogging"}]}}}'
    # delete the deployment to ensure the changes are picked up in a timely matter
    oc delete deployment -n $WMCO_DEPLOY_NAMESPACE windows-machine-config-operator
  elif oc get clusterextension windows-machine-config-operator &>/dev/null; then
    echo "Detected OLMv1, patching deployment directly..."
    # Add debug env variable to the WMCO manager container
    oc set env deployment/windows-machine-config-operator -n "$WMCO_DEPLOY_NAMESPACE" ARGS="--debugLogging" -c manager
    # force restart to pick up the env variable change
    oc scale deployment/windows-machine-config-operator -n "$WMCO_DEPLOY_NAMESPACE" --replicas=0
    oc scale deployment/windows-machine-config-operator -n "$WMCO_DEPLOY_NAMESPACE" --replicas=1
  else
    echo "Error: Unable to detect OLM version; no subscription or clusterextension found"
    return 1
  fi

  retries=0
  debug_logging_enabled=0
  until [[ $debug_logging_enabled -eq 1 || $retries -gt 30 ]]; do
    pod_json=$(oc get -n $WMCO_DEPLOY_NAMESPACE pod -l name=windows-machine-config-operator -ojson)
    pod_count=$(echo $pod_json |jq '.items | length')
    if [[ $pod_count -ne 1 ]]; then
      echo "Found $pod_count WMCO pod(s), waiting for 1"
      sleep 10
      retries=$((retries+1))
      continue
    fi
    if [[ $pod_json != *"--debugLogging"* ]]; then
      echo "Waiting for debugLogging to be set"
      sleep 10
      retries=$((retries+1))
      continue
    fi
    debug_logging_enabled=1
  done
  if [[ $debug_logging_enabled -ne 1 ]]; then
    echo "Error enabling debug logging"
    exit 1
  fi
  # Final wait to ensure the pod is fully running
  oc wait --timeout=10m --for condition=Available -n $WMCO_DEPLOY_NAMESPACE deployment windows-machine-config-operator
}