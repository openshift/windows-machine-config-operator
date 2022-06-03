# vSphere Platform Pre-Requisites

In order to successfully use the Windows Machine Config Operator (WMCO) on a vSphere Platform, 
the following pre-requisites are required:

* The vSphere cluster must be configured with [hybrid OVN Kubernetes networking with a custom VXLAN port](setup-hybrid-OVNKubernetes-cluster.md#vSphere)
  to work around the pod-to-pod connectivity between hosts [issue](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems#pod-to-pod-connectivity-between-hosts-is-broken-on-my-kubernetes-cluster-running-on-vsphere)
* Set up a [VM golden image with a compatible Windows Server version](vsphere-golden-image.md#1-select-a-compatible-windows-server-version).
* Add a [DNS entry](#adding-a-dns-entry-in-vsphere-environment) for the internal API endpoint in the vSphere environment

Alternatively, a programmatic approach for the creation of the Windows VM golden image can be found [here.](vsphere_ci/README.md)

## Adding a DNS entry in vSphere environment

WMCO downloads the ignition files from the internal API server endpoint. To enable this, add a new DNS entry for

```
api-int.<cluster-name>.domain.com
```

which points to the external API server URL

```
api.<cluster-name>.domain.com
```

where `<cluster-name>` is the name of the Openshift cluster.

Note: The DNS entry can be a CNAME or an additional A record.

The above DNS entry ensures that Windows VM can download the ignition file from the internal API server 
and the `kubelet` on the configured VM can communicate with the internal API server. In the case of Linux nodes,
CoreDNS is running on every node which helps in resolving the internal API server URL. The external API endpoint
should have been created as part of the [cluster install](https://docs.openshift.com/container-platform/latest/installing/installing_vsphere/installing-vsphere-installer-provisioned.html).
