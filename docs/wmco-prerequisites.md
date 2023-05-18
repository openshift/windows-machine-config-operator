# Cluster and OS Pre-Requisites
Below are the Cluster and OS prerequisites for WMCO. Please also see the vSphere-specific section that 
may be relevant.

## Supported Cloud Providers based on OKD/OCP Version and WMCO version
| Cloud Provider            | Supported OKD/OCP Version | Supported WMCO version |
|---------------------------|---------------------------|------------------------|
| Amazon Web Services (AWS) | 4.6+                      | WMCO 1.0+              |
| VMware vSphere            | 4.7+                      | WMCO 2.0+              |
| Platform none (BYOH)      | 4.8+                      | WMCO 3.1.0+            |
| Azure                     | 4.11+                     | WMCO 6.0+              |

Note: We added Azure support in 4.6 but given that Microsoft has [stopped publishing Windows Server 2019 images with
Docker](https://techcommunity.microsoft.com/t5/containers/important-update-deprecation-of-docker-virtual-machine-images/ba-p/3646272),
we have decided to drop Azure support for releases older than 6.0.0. For 5.y.z and below it was a requirement for
the Windows Server 2019 images to have Docker pre-installed. From 6.0.0 onwards we are using containerd as the
runtime and it is WMCO's responsibility to manage that.

## Supported Windows Server versions
The following table outlines the supported
[Windows Server version](https://docs.microsoft.com/en-us/windows/release-health/release-information) based on the 
applicable cloud provider.

Note: Any unlisted Windows Server version are NOT supported, and will cause errors. To prevent 
these errors, only use the appropriate version according to the cloud provider in use. 

| Cloud Provider | Supported Windows Server version                                                                                                  |
|----------------|-----------------------------------------------------------------------------------------------------------------------------------|
| AWS            | Windows Server 2019, version 1809 Long-Term Servicing Channel (LTSC)                                                              |
| Azure          | - Windows Server 2019, version 1809 Long-Term Servicing Channel (LTSC)<br>- Windows Server 2022 Long-Term Servicing Channel (LTSC)|
| VMware vSphere | Windows Server 2022 Long-Term Servicing Channel (LTSC)                                                                            |

*Please note that the Windows Server 2022 image must contain the OS-level container networking patch [KB5012637](https://support.microsoft.com/en-us/topic/april-25-2022-kb5012637-os-build-20348-681-preview-2233d69c-d4a5-4be9-8c24-04a450861a8d).*

## Supported Networking
[OVNKubernetes hybrid networking](setup-hybrid-OVNKubernetes-cluster.md) is the only supported networking configuration.
The following tables outline the type of networking configuration and Windows Server versions to be used based on your 
cloud provider. The network configuration must be completed during the installation of the cluster.
  
Note: 
* OpenShiftSDN networking is the default network for OpenShift clusters. It is NOT supported by WMCO.
* Dual NIC is NOT supported by WMCO.

| Cloud Provider | Supported networking                                                                           |
|----------------|------------------------------------------------------------------------------------------------|
| AWS            | Hybrid OVNKubernetes                                                                           |
| Azure          | Hybrid OVNKubernetes                                                                           |
| VMware vSphere | Hybrid OVNKubernetes with a [Custom VXLAN port](setup-hybrid-OVNKubernetes-cluster.md#vsphere) |

| Hybrid OVNKubernetes | Supported Windows Server version                                                                                                  |
|----------------------|-----------------------------------------------------------------------------------------------------------------------------------|
| Default VXLAN port   | - Windows Server 2019, version 1809 Long-Term Servicing Channel (LTSC)<br>- Windows Server 2022 Long-Term Servicing Channel (LTSC)|
| Custom VXLAN port    | Windows Server 2022 Long-Term Servicing Channel (LTSC)                                                                            |

## Supported Installation method
* Installer-Provisioned Infrastructure installation method is the only supported installation method. This is 
consistent across all supported cloud providers.
  
* User-Provisioned Infrastructure is NOT supported.

## vSphere Specific Requirements
Please refer to [VMware vSphere pre-requisites](vsphere-prerequisites.md).
