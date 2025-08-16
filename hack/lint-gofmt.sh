#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..

GO_VERSION=($(go version))

if [[ -z $(echo "${GO_VERSION[2]}" | grep -E 'go1.24') ]]; then
  echo "Unknown go version '${GO_VERSION[2]}', skipping gofmt."
  exit 1
fi
cd "${WMCO_ROOT}"

# find_files identifies all the go files excluding some directories and
# binaries created as part of build
find_files() {
  find . -not \( \
      \( \
        -wholename './build' \
        -o -wholename './release' \
        -o -wholename './target' \
        -o -wholename './.git' \
        -o -wholename '*/vendor/*' \
        -o -wholename './cloud-provider-aws' \
        -o -wholename './cloud-provider-azure' \
        -o -wholename './containernetworking-plugins' \
        -o -wholename './containerd' \
        -o -wholename './hcsshim' \
        -o -wholename './kubelet' \
        -o -wholename './ovn-kubernetes' \
        -o -wholename './promu' \
        -o -wholename './windows_exporter' \
        -o -wholename './csi-proxy' \
      \) -prune \
    \) -name '*.go'
}

GOFMT="gofmt -s" 
bad_files=$(find_files | xargs $GOFMT -l)
if [[ -n "${bad_files}" ]]; then
  echo "!!! '$GOFMT' needs to be run on the following files: "
  echo "${bad_files}"
  exit 1
fi

bad_files=$(go run ./vendor/github.com/go-imports-organizer/goio -l)
if [[ -n "${bad_files}" ]]; then
        echo "!!! goio needs to be run on the following files:"
        echo "${bad_files}"
        echo "Try running 'make imports'"
        exit 1
fi

non_e2e_files=$(find_files | sed '/test\/e2e/d')
bad_files=""
for file in $non_e2e_files;do
  # || true required here as grep will cause the script to error out if a match isnt found
  match=$(grep -H "context.TODO()" $file |awk '{print $1}') || true
  if [[ -n $match ]]; then
    bad_files="${bad_files}${match}\n"
  fi
done
if [[ -n "${bad_files}" ]]; then
  echo "!!! 'context.TODO' was found in the following files: "
  printf "${bad_files}"
  echo "Switch to a context that handles system interrupt signals"
  exit 1
fi
