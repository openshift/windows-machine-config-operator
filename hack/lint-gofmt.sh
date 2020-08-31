#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail
WMCO_ROOT=$(dirname "${BASH_SOURCE}")/..

GO_VERSION=($(go version))

if [[ -z $(echo "${GO_VERSION[2]}" | grep -E 'go1.14') ]]; then
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
