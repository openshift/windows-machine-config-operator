#!/bin/bash
set -o errexit
set -o nounset
set -o pipefail

# The purpose of this script is to check that all generated files related to operator-sdk are kept up to date

make bundle
# check if any changes to the config/ or bundle/ directories have occured
status=$(git status config/ bundle/ --porcelain)
if [ -n "$status" ]; then
  echo "bundle lint failure"
  echo "$status"
  exit 1
fi
