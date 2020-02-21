# Windows Machine Config Operator

## Pre-requisites
- [Install](https://github.com/operator-framework/operator-sdk/blob/v0.15.x/doc/user/install-operator-sdk.md) operator-sdk
  v0.15.2
- The operator is written using operator-sdk [v0.15.2](https://github.com/operator-framework/operator-sdk/releases/tag/v0.15.2)
  and has the same [pre-requisites](https://github.com/operator-framework/operator-sdk/tree/v0.15.x#prerequisites) as it
  does.

## Build
To build the operator image, execute:
```shell script
operator-sdk build quay.io/<insert username>/wmco:latest
```
