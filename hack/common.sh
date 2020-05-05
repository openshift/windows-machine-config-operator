#!/bin/bash

get_operator_sdk() {
  # Download the operator-sdk binary only if it is not already available
  # We do not validate the version of operator-sdk if it is available already
  if type operator-sdk >/dev/null 2>&1; then
    which operator-sdk
    return
  fi

  DOWNLOAD_DIR=/tmp/operator-sdk
  # TODO: Make this download the same version we have in go dependencies in gomod
  wget -O $DOWNLOAD_DIR https://github.com/operator-framework/operator-sdk/releases/download/v0.17.0/operator-sdk-v0.17.0-x86_64-linux-gnu >/dev/null  && chmod +x /tmp/operator-sdk || return
  echo $DOWNLOAD_DIR
}
