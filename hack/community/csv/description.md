## WARNING
Community distribution of the Windows Machine Config Operator.
This is a preview build, and is not meant for production workloads.
Issues with this distribution of WMCO can be opened against the [WMCO repository](https://github.com/openshift/windows-machine-config-operator).
Please read through the [troubleshooting doc](https://github.com/openshift/windows-machine-config-operator/blob/COMMUNITY_VERSION/docs/TROUBLESHOOTING.md)
before opening an issue.
Please ensure that when installing this operator the starting CSV you subscribe to is supported on the
version of OKD/OCP you are using. This CSV is meant for OKD/OCP COMMUNITY_VERSION.
## Documentation
### Introduction
The Windows Machine Config Operator configures Windows instances into nodes, enabling Windows container workloads
to be ran within OKD/OCP clusters. Windows instances can be added either by creating a [MachineSet](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/windows_container_support_for_openshift/creating-windows-machine-sets#creating-windows-machineset-aws),
or by specifying existing instances through a [ConfigMap](https://docs.redhat.com/en/documentation/openshift_container_platform/4.18/html/windows_container_support_for_openshift/byoh-windows-instance).
The operator will do all the necessary steps to configure the instance so that it can join the cluster as a worker node.
### Pre-requisites
- [Cluster and OS pre-requisites](https://github.com/openshift/windows-machine-config-operator/blob/COMMUNITY_VERSION/docs/wmco-prerequisites.md)
### Usage
Please see the usage section of [README.md](https://github.com/openshift/windows-machine-config-operator/blob/COMMUNITY_VERSION/README.md#usage).
### Limitations
#### DeploymentConfigs
Windows Nodes do not support workloads created via DeploymentConfigs. Please use a normal Deployment, or other method to
deploy workloads.
