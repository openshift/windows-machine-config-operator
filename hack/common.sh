#!/bin/bash

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
  local WMCO_VER_MAJOR=${WMCO_VERSION:0:1}
  # OCP 4.6 maps to WMCO 1.y.z making the WMCO major version always five
  # versions behind OCP Y version
  local DIFFERENCE=5
  local OCP_VER_MINOR=$(($DIFFERENCE+$WMCO_VER_MAJOR))
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

  DOWNLOAD_DIR=/tmp/operator-sdk
  # TODO: Make this download the same version we have in go dependencies in gomod
  wget --no-verbose -O $DOWNLOAD_DIR https://github.com/operator-framework/operator-sdk/releases/download/v1.15.0/operator-sdk_linux_amd64 -o operator-sdk && chmod +x /tmp/operator-sdk || return
  echo $DOWNLOAD_DIR
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

# returns the number of instances from `windows-instances` ConfigMap
getWindowsInstanceCountFromConfigMap() {
 oc get configmaps \
   windows-instances \
   -n "${WMCO_DEPLOY_NAMESPACE}" \
   -o json | jq '.data | length'
}
