# vSphere Platform Pre-Requisites

In order to successfully use the Windows Machine Config Operator (WMCO) on a vSphere Platform, 
the following pre-requisites are required:

* The vSphere cluster must be configured with [hybrid OVN Kubernetes networking with a custom VXLAN port](setup-hybrid-OVNKubernetes-cluster.md#vSphere)
  to work around the pod-to-pod connectivity between hosts [issue](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems#pod-to-pod-connectivity-between-hosts-is-broken-on-my-kubernetes-cluster-running-on-vsphere)
* Set up a [Windows Server Semi-Annual Channel (SAC): Windows Server 2004 VM golden image](vsphere-golden-image.md)

Alternatively, a programmatic approach for the creation of the Windows VM golden image can be found [here.](vsphere_ci/README.md)
