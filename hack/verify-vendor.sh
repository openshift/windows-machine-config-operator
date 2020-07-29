#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(pwd)

# Create a temporary location to put our new vendor tree
mkdir -p "${WMCO_ROOT}/_tmp"
_tmpdir="$(mktemp -d "${WMCO_ROOT}/_tmp/wmco-vendor.XXXXXX")"

# Copy the contents of the WMCO directory into the temporary location
_wmcotmp="${_tmpdir}/WMCO"
mkdir -p "${_wmcotmp}"
tar --exclude=.git --exclude="./_*" -c . | (cd "${_wmcotmp}" && tar xf -)
# Clean up the temp directory
trap "rm -rf ${WMCO_ROOT}/_tmp" EXIT

export GO111MODULE=on

pushd "${_wmcotmp}" > /dev/null 2>&1
# Destroy deps in the copy of the WMCO tree
rm -rf ./vendor 

# Recreate the vendor tree using the clean set we just downloaded
go mod vendor
popd > /dev/null 2>&1

ret=0

pushd "${WMCO_ROOT}" > /dev/null 2>&1
if ! _out="$(diff -Naupr -x "BUILD" -x "AUTHORS*" -x "CONTRIBUTORS*" vendor "${_wmcotmp}/vendor")"; then
   echo "Your Vendored results are different:" >&2
   echo "${_out}" >&2
   echo "Vendor verify failed." >&2
   echo "${_out}" > vendordiff.patch
   echo "If you're seeing this, run the below command to fix your directories:" >&2
   echo "go mod vendor" >&2
   ret=1
fi
popd > /dev/null 2>&1


exit ${ret}
