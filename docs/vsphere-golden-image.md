# Creating the vSphere Windows VM Golden Image

This guide describes the thought process of creating a Windows virtual machine base image (the Golden Image) in vSphere.

## 1. Select a compatible Windows Server version

Currently, the Windows Machine Config Operator (WMCO) stable version only supports Windows Server Semi-Annual
Channel (SAC): Windows Server 2004, that includes the patch [KB4565351](https://support.microsoft.com/en-us/help/4565351/windows-10-update-kb4565351), 
required by the operating system to allow using the [hybrid OVN Kubernetes networking with a 
custom VXLAN port](setup-hybrid-OVNKubernetes-cluster.md#vSphere) feature.

*Please note that Windows Server Long-Term Servicing Channel (LTSC): Windows Server 1809 cannot be used, since 
the patch is not available.*

## 2. Create the virtual machine

To start with, create a new virtual machine in vSphere from the selected Windows Server distribution using the ISO image.
See [vSphere documentation](https://docs.vmware.com/en/VMware-vSphere/6.0/com.vmware.vsphere.hostclient.doc/GUID-7834894B-DD17-4D59-A9BF-A33D02478521.html)

### Setup VMware Tools

Install VMware Tools version 11.0.6 or greater. See the [VMware Tools documentation](https://docs.vmware.com/en/VMware-Tools/11.2.0/com.vmware.vsphere.vmwaretools.doc/GUID-D8892B15-73A5-4FCE-AB7D-56C2C90BD951.html) for more information.

Enable the VMware Tools configuration file [tools.conf](https://docs.vmware.com/en/VMware-Tools/11.2.0/com.vmware.vsphere.vmwaretools.doc/GUID-EA16729B-43C9-4DF9-B780-9B358E71B4AB.html) within the Windows virtual machine.

For example in Windows Server version 2004 the VMware Tools configuration file can be found at

```
    C:\ProgramData\VMware\VMware Tools\tools.conf
```

In case the configuration file does not exist, VMware Tools installs an example configuration file in the same 
directory as the location above, or you can download it from the
[open-vm-tools](https://raw.githubusercontent.com/vmware/open-vm-tools/master/open-vm-tools/tools.conf) repository.

Ensure the virtual machine has a valid IP address in vCenter. Refer to the *IP Addresses* section in the *Summary* tab 
of the vSphere Web Client or run the `ipconfig` command in the Windows VM.

Ensure the *VMware Tools* Windows service is running and will start on boot. Refer to [VMware Tools service 
documentation](https://docs.vmware.com/en/VMware-vSphere/6.0/com.vmware.vsphere.vm_admin.doc/GUID-0BD592B1-A300-4C09-808A-BB447FAE2C2A.html) 
or run the following PowerShell command:

```powershell
    Get-Service -Name VMTools | Select Status, StartType 
```

### Exclude the network interface in the VMware Tools configuration

Enable the following entry in the VMware Tools configuration file to ensure that the cloned vNIC generated on 
the Windows VM by the hybrid-overlay won't be ignored.

```bash
    exclude-nics=
``` 

Alternatively, you can run the following PowerShell script that downloads the example configuration file, excludes 
the network interface and creates the `tools.conf` file.

```powershell
    Invoke-WebRequest -O tools.conf https://raw.githubusercontent.com/vmware/open-vm-tools/master/open-vm-tools/tools.conf
    
    (Get-Content -Path tools.conf) -replace '#exclude-nics=','exclude-nics=' | Set-Content -Force -Path tools.conf
    
    mv -Force tools.conf 'C:\ProgramData\VMware\VMware Tools\tools.conf'
```

## 3. Set up SSH

Install and configure the OpenSSH Server on the Windows virtual machine with automatic startup and key based-authentication.
See [Microsoft documentation](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse)
to install using PowerShell.

Alternatively, the provided PowerShell script [install-openssh.ps1](vsphere_ci/scripts/install-openssh.ps1) aims to 
programmatically install and configure the OpenSSH Server with the public key file from section [Add the Administrator 
Public Key](#add-the-administrator-public-key). For example, you can run the following command:

```
    ./install-openssh.ps1 <path/to/public_key_file>
```

where, `<path/to/public_key_file>` is the path to the public key file corresponding to the [private key.](../README.md#create-a-private-key-secret)

Ensure the SSH service is running and that it will start on boot by running the following PowerShell command:

```powershell
    Get-Service -Name "ssh*" | Select Name, Status, StartType 
```

where, both installed services *ssh-agent* and *sshd* must have status *Running* and start type *Automatic*.

Ensure the OpenSSH Server installation successfully created an inbound firewall rule enabling access to the above 
service (sshd) by running the following PowerShell command:

```powershell
    Get-NetFirewallRule -DisplayName "*ssh*"
```

In case no firewall rule exist, you must create it by running the following PowerShell command:

```powershell
    New-NetFirewallRule -DisplayName 'OpenSSH Server (sshd)' -LocalPort 22 -Enabled True -Direction Inbound -Protocol TCP -Action Allow 
```

### Add the Administrator Public Key

This section provides an overview of how to use key-based authentication with OpenSSH server, similar to Linux 
environments commonly setup of public-key/private-key pairs to drive authentication without passwords. Key pairs refer 
to the public and private key files that are used by certain authentication protocols.

**Important**: You need to have OpenSSH Server installed first. Please see [Set up SSH](#3-set-up-ssh).

#### Private key generation

To use key-based authentication, you first need to generate public/private key pairs. 

As the private key file let's reuse the exiting [the secret](../README.md#create-a-private-key-secret) created while 
installing WMCO, along with the corresponding public key file.

#### Deploying the public key

The contents of the public key file needs to be placed on the Windows VM as a text file called 
`administrators_authorized_keys` in the *C:\ProgramData\ssh\\* directory to allow administration access via SSH, as 
describe by the [Microsoft documentation.](https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_keymanagement#administrative-user) 

## 4. Set up the container runtime

Install the `docker` container runtime following the [Microsoft's documentation](https://docs.microsoft.com/en-us/virtualization/windowscontainers/quick-start/set-up-environment?tabs=Windows-Server).

Alternatively, you can run the provided PowerShell script [install-docker.ps1](vsphere_ci/scripts/install-docker.ps1) 
to programmatically install `docker` in the Windows VM.

### Set up for disconnected network environment

If you plan to use the golden image in an air-gapped or disconnected network environment, you must pre-pull a
compatible [Pause Image](https://kubernetes.io/docs/setup/production-environment/windows/intro-windows-in-kubernetes/#pause-image)
container, since it's the only external resource required by Windows Machine Config Bootstrapper (WMCB).

To pre-pull the container image run the following command:
```bash
    docker pull mcr.microsoft.com/oss/kubernetes/pause:3.4.1
 ```

## 5. Set up incoming connection for container logs

Create a new firewall rule in the Windows VM to allow incoming connections for container logs, usually 
on TCP port `10250` by running the following PowerShell command:

```powershell
    New-NetFirewallRule -DisplayName "ContainerLogsPort" -LocalPort 10250 -Enabled True -Direction Inbound -Protocol TCP -Action Allow -EdgeTraversalPolicy Allow
```

## 6. Generalize the virtual machine installation

To deploy the Windows VM as a reusable image, you have to first generalize the VM removing computer-specific information 
such as installed drivers. Running the `sysprep` command with a *unattend* answer file generalizes the image and 
makes it ready for future deployments, maintaining all the changes needed for the WMCO installation. 

You can find an example [unattend.xml](unattend.xml) file in this repository. It is just an example and, it **should 
not** be used directly. You must customize the example to fit your needs.

To execute the `sysprep` command use:

```cmd
    C:\Windows\System32\Sysprep\sysprep.exe /generalize /oobe /shutdown /unattend:<path/to/unattend.xml>
```

where `<path/to/unattend.xml>` is the path to the customized answer file.

Note: There is [a limit](https://docs.microsoft.com/en-us/windows-hardware/manufacture/desktop/sysprep--generalize--a-windows-installation#limits-on-how-many-times-you-can-run-sysprep)
on how many times you can run the `sysprep` command.

## 7. Set up the virtual machine golden image

Once the `sysprep` command completes the Windows virtual machine will power off. 

You **must not** use this Windows virtual machine anymore. 

Add the cloned Windows virtual machine as a template in the [machineset](../README.md#configuring-windows-instances-provisioned-through-machinesets).

```yaml
    apiVersion: machine.openshift.io/v1beta1
    kind: MachineSet
    spec:
      template:
        spec:
          providerSpec:
            template: <Cloned Windows Virtual Machine>
```

If you want to update the virtual machine golden image, make the changes in the original Windows virtual machine, 
clone it and run `sysprep` command again.

## Supporting information

* https://docs.microsoft.com/en-us/windows/release-health/release-information
* https://docs.microsoft.com/en-us/windows-server/get-started/windows-server-release-info
* https://docs.microsoft.com/en-us/windows-server/get-started-19/servicing-channels-19
