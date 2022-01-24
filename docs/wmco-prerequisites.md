# Cluster and OS Pre-Requisites
Below are the Cluster and OS prerequisites for WMCO. Please also see the vSphere-specific section that 
may be relevant.

## Supported Cloud Providers based on OKD/OCP Version and WMCO version
| Cloud Provider           | Supported OKD/OCP Version   | Supported WMCO version   |
| -----------              | -----------                 | -----------              |
| Amazon Web Services (AWS)| 4.6+                        | WMCO 1.0+                |
| Azure                    | 4.6+                        | WMCO 1.0+                |
| VMware vSphere           | 4.7+                        | WMCO 2.0+                |
| Platform none (BYOH)     | 4.8+                        | WMCO 3.1.0+              |

## Supported Windows Server versions
The following table outlines the supported
[Windows Server version](https://docs.microsoft.com/en-us/windows/release-health/release-information) based on the 
applicable cloud provider.

Note: Any unlisted Windows Server version are NOT supported, and will cause errors. To prevent 
these errors, only use the appropriate version according to the cloud provider in use. 

| Cloud Provider      | Supported Windows Server version                                        |
| -----------         | -----------                                                             |
| AWS                 | Windows Server Long-Term Servicing Channel (LTSC): Windows Server 2019  |
| Azure               | Windows Server Long-Term Servicing Channel (LTSC): Windows Server 2019  |
| VMware vSphere      | Windows Server Semi-Annual Channel (SAC): Windows Server 20H2|

## Supported Networking
[OVNKubernetes hybrid networking](setup-hybrid-OVNKubernetes-cluster.md) is the only supported networking configuration.
The following tables outline the type of networking configuration and Windows Server versions to be used based on your 
cloud provider. The network configuration must be completed during the installation of the cluster.
  
Note: OpenShiftSDN networking is the default network for OpenShift clusters. It is NOT supported by WMCO.

| Cloud Provider      | Supported networking                                                                          |
| -----------         | -----------                                                                                   | 
| AWS                 | Hybrid OVNKubernetes                                                                          |
| Azure               | Hybrid OVNKubernetes                                                                          |
| VMware vSphere      | Hybrid OVNKubernetes with a [Custom VXLAN port](setup-hybrid-OVNKubernetes-cluster.md#vsphere)|

| Hybrid OVNKubernetes      | Supported Windows Server version                                      |
| -----------               | -----------                                                           |
| Default VXLAN port        | Windows Server Long-Term Servicing Channel (LTSC): Windows Server 2019|
| Custom VXLAN port         | Windows Server Semi-Annual Channel (SAC): Windows Server 20H2|

## Supported Installation method
* Installer-Provisioned Infrastructure installation method is the only supported installation method. This is 
consistent across all supported cloud providers.
  
* User-Provisioned Infrastructure is NOT supported.

## vSphere Specific Requirements
Please refer to [VMware vSphere pre-requisites](vsphere-prerequisites.md).
