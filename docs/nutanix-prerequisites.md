# Nutanix Platform Pre-Requisites

The following pre-requisites are required to use the Windows Machine Config Operator (WMCO) on a Nutanix platform: 

* Upload a [VM image with a compatible Windows Server version](wmco-prerequisites.md#supported-windows-server-versions) to the Prism-Central/Prism-Element where the Machine VMs will be created.
* Add a [DNS entry](#adding-a-dns-entry-in-nutanix-environment) for the internal API endpoint in the Nutanix environment.

## Adding a DNS entry in Nutanix environment

WMCO downloads the ignition files from the internal API server endpoint. To enable this download, add a new DNS entry for

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
and the `kubelet` on the configured VM can communicate with the internal API server. For Linux nodes,
CoreDNS runs on every node, which helps resolving the internal API server URL. The external API endpoint
should have been created as part of the [cluster install](https://docs.openshift.com/container-platform/latest/installing/installing_nutanix/installing-nutanix-installer-provisioned.html).
