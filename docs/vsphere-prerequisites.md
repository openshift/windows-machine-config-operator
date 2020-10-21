# vSphere pre-requisites
## vSphere Windows VM golden image creation
* Create the VM from an updated version of Windows Server 1909 VM image which includes the [patch](https://support.microsoft.com/en-us/help/4565351/windows-10-update-kb4565351)
* Configure SSH on the VM:
  * SSH on the Windows VM is configured using the [powershell script](powershell.ps1)
  * A file, *C:\Users\Administrator\.ssh\authorized_keys* containing the public key corresponding to the private key in the [secret](https://github.com/openshift/windows-machine-config-operator#usage)
* Install and configure VMWare tools on the VM:
  * Version 11.0.6 or greater is a requirement
  * The [tools.conf](https://docs.vmware.com/en/VMware-Tools/11.2.0/com.vmware.vsphere.vmwaretools.doc/GUID-594192DA-0306-425D-B0CD-CB141C4C6874.html)
    file at *C:\ProgramData\VMware\VMware Tools\tools.conf* has the following entry to ensure that the cloned vNIC
    generated on the Windows VM by the hybrid-overlay won't be ignored and the VM has an IP Address in vCenter:
    * `exclude-nics=`
  * Ensure that the *VMTools* Windows service is running
* Pull all the required [Windows container base images](https://docs.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/container-base-images)
* Sysprep the VM to make it read for cloning using the following command:
  ```
  C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:<path_to_unattend.xml>`
  ```
  An example [unattend.xml](unattend.xml) is provided however the fields in the example require customization and it
  should not be used directly.

## vSphere environment changes
WMCO downloads the ignition files from the internal API server endpoint. To enable this, add a new DNS entry for
`api-int.<cluster-name>.domain.com` which points to the external API server URL `api.<cluster-name>.domain.com`.
This new DNS entry ensures that Windows VM can download the ignition file from the internal API server and kubelet
on the configured VM can communicate with the internal API server. In the case of Linux nodes, coreDNS is running
on every node which helps in resolving the internal API server URL. The external API endpoint should have been
created as part of the
[cluster install](https://docs.openshift.com/container-platform/4.5/installing/installing_vsphere/installing-vsphere-installer-provisioned.html).
