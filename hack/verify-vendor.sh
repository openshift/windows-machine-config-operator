#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

WMCO_ROOT=$(pwd)

go mod tidy
go mod vendor
# check if any of go mod related files have changed
if [ -n "$(git status vendor/ go.mod go.sum --porcelain)" ]; then
  exit 1
fi
