# vSphere pre-requisites

The vSphere cluster must be configured with [hybrid OVN Kubernetes networking](setup-hybrid-OVNKubernetes-cluster.md)
with a custom VXLAN port to work around the pod-to-pod connectivity between
hosts [issue](https://docs.microsoft.com/en-us/virtualization/windowscontainers/kubernetes/common-problems#pod-to-pod-connectivity-between-hosts-is-broken-on-my-kubernetes-cluster-running-on-vsphere).

In order to use the Windows MachineConfig Operator to successfully create
a Windows node, you will first need to create a Windows "golden image"
VM. These steps are an overview of the required configuration of the
golden image.

## vSphere Windows VM golden image creation
* Create the VM from an updated version of Windows Server 1909 VM image
  that includes the patch [KB4565351](https://support.microsoft.com/en-us/help/4565351/windows-10-update-kb4565351).
  This is required to get the OS feature that allows setting the VXLAN UDP port.
  Please note that this patch is not available for `Windows Server 2019`.
* Configure SSH on the VM:
  * You can install OpenSSH Server on the Windows VM by using the provided
  [powershell script](vsphere_ci/scripts/install-openssh.ps1), or by following the
  [Microsoft documentation](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse).
  * The file, `C:\Users\Administrator\.ssh\authorized_keys` must
  contain the public key corresponding to the private key in the
  [secret](https://github.com/openshift/windows-machine-config-operator#usage)
  on OpenShift.
  * Make sure the SSH service is running and that it will start on boot.
* Install and configure VMWare tools on the VM:
  * Version 11.0.6 or greater is a requirement
  * The
  [tools.conf](https://docs.vmware.com/en/VMware-Tools/11.2.0/com.vmware.vsphere.vmwaretools.doc/GUID-594192DA-0306-425D-B0CD-CB141C4C6874.html)
  file at `C:\ProgramData\VMware\VMware Tools\tools.conf` has the
  following entry to ensure that the cloned vNIC
    generated on the Windows VM by the hybrid-overlay won't be ignored
    and the VM has an IP Address in vCenter:
    * `exclude-nics=`
  * Ensure that the **VMTools** Windows service is running and will
  start on boot.
* Pull all the required Windows container base images
you'll need for your applications. The images you'll pull
is dependent on the Windows kernel being used. Please see
[the offical matrix](https://docs.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/container-base-images)
to determine which images you need.
* Sysprep the VM. Use an `unattend.xml` that will maintain all the
changes needed for the WMCO. The most important one being that the
Administrator's home directory stays intact with the SSH public key.

  Here is an example command to `sysprep`
  ```
  C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:<path_to_unattend.xml>`
  ```
  You can find an example [unattend.xml](unattend.xml) in this repo. It
  is provided as an example and it **should not** be used directly.

## vSphere environment changes
WMCO downloads the ignition files from the internal API server
endpoint. To enable this, add a new DNS entry for
`api-int.<cluster-name>.domain.com` which points to the external API
server URL `api.<cluster-name>.domain.com`.

This can be a CNAME or an additional A record.

This new DNS entry ensures that Windows VM can download the ignition
file from the internal API server and kubelet
on the configured VM can communicate with the internal API server. In
the case of Linux nodes, coreDNS is running
on every node which helps in resolving the internal API server URL. The
external API endpoint should have been
created as part of the
[cluster install](https://docs.openshift.com/container-platform/4.7/installing/installing_vsphere/installing-vsphere-installer-provisioned.html).

An example to automate the VM golden image creation can be found
[here](vsphere_ci/README.md)
