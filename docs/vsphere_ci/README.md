# Building the vSphere Windows VM Golden Image programmatically

This document focuses on building Windows VM golden image in vSphere 6.7 and later, to be used in CI but can be 
used as example for other vSphere environments. We propose [Packer](https://github.com/hashicorp/packer) to automate 
installation and configuration of the Windows VM golden image. 

Packer generates a vSphere VM template that can be converted to a virtual machine. After converting the 
template to virtual machine, the `machine-api` can use this newly created virtual machine (the golden image) in Power-Off state for 
subsequent VM cloning. 

The above golden image name is the one we use in the Machine Set's `providerSpec.template`. The following steps need 
to be executed from the bastion host or any instance that has access to the vSphere environment.


## Installing Packer

In case of our vSphere environment, we need to install Packer 1.6.6 on the bastion host. The installation steps are:

- Download [Packer](https://www.packer.io/downloads)
    - `curl -o packer.zip https://releases.hashicorp.com/packer/1.6.6/packer_1.6.6_linux_amd64.zip`
- Unzip using zip utility on the host
    - `gunzip -S .zip packer.zip`
- Add execution permission to the Packer binary
    - `chmod +x packer`
- Update the `$PATH` environment variable to include the Packer binary
    - `PATH=$PATH:<path/to/binary>`

## Prerequisite files

Please ensure the `scripts` directory is present in the location where you are running Packer from on the 
bastion host and has the following files:

    - authorized_keys
    - autounattend.xml
    - install-vm-tools.cmd
    - configure-vm-tools.ps1
    - install-openssh.ps1
    - install-firewall-rules.ps1
    - install-kb5012637.ps1

The [authorized_keys](scripts/authorized_keys) file must contain a public key, where the private key 
associated with this public key is what will be used by WMCO to configure VMs created from Windows VM. After 
deploying WMCO, this private key will be provided by the user in the form of a Secret.

The [autounattend.xml](scripts/autounattend.xml) file must be edited to update the value of 
`WindowsPassword` with a user provided password. Then, it executes the following steps:

- Runs `install-vm-tools.cmd` script which installs VMWare tools
- Runs `configure-vm-tools.ps1` script which configures VMWare tools
- Runs `install-openssh.ps1` script which installs and configures OpenSSH Server
- Runs `install-firewall-rules.ps1` script which configures the firewall rules
- Runs `install-kb5012637.ps1` script which installs a required Windows OS-level container networking patch

The above [autounattend.xml](scripts/autounattend.xml) script is different from the [unattend.xml](../unattend.xml)
as this script does Windows OS installation as well.

## Packer build configuration file

Packer needs a build file which specifies how the virtual machine template should be built. You can find a [reference 
build file](build.json) in the repository.

### Variables

In order to use the provided [reference build file](build.json) as a valid configuration with Packer, you **must** 
adjust the following variables:

- `<vmtools-iso-path>` Path where VMWare Tools ISO is available in vSphere datacenter
  (default: `[] /usr/lib/vmware/isoimages/windows.iso`)
- `<os-iso-path>` Path where Windows OS ISO is available in vSphere datacenter
- `<vm-template-folder>` Name of the folder where the VM templates will be created by Packer
- `<vm-template-name>` Name of the VM template that will be created by Packer
- `<vm-elevated-password>` Password for the Windows virtual machine Administrator user,
  must match with the password entered in the [autounattend.xml](scripts/autounattend.xml) script
- `<vsphere-cluster>` Name of the vSphere cluster
- `<vsphere-datacenter>` Name of the vSphere datacenter
- `<vsphere-datastore>` Name of the vSphere datastore
- `<vsphere-server>` The vCenter server hostname, with no scheme (`https://`) nor path separators (`/`).
  For example: `vcenter.example.com`.
  See [Packer documentation](https://www.packer.io/docs/builders/vsphere/vsphere-iso) for more information
- `<vsphere-user>` The vCenter username
- `<vsphere-password>` The vCenter password

## Building with Packer

Packer relies on a [build file](build.json) for virtual machine template creation.

To build:
```bash
  packer build build.json
```

To forcefully rebuild the template:
```bash
  packer build -force build.json
```

To enable detailed logging:
```bash
  PACKER_LOG=1 packer build build.json
```

## What actually happens during build

Packer mounts the Windows iso and starts the VM. 
- All the files in `floppy_files` section of your build file will be copied to the floppy disk of the mounted iso 
 which is represented as `a:\` drive in the Windows VM
- [autounattend.xml](scripts/autounattend.xml) is a special file in Windows which gets automatically executed once the
VM starts. You can specify all the commands that needs to executed on first boot.
  
## Using the virtual machine template

Once the Packer build completes successfully, a new VM template with name `<vm-template-name>` will be created in
the folder `<vm-template-folder>` following the [Variables](#variables). The later VM template is ready to use as a
golden image, as described in [the documentation](../vsphere-golden-image.md#9-using-the-virtual-machine-template).

## References
- [Sample autounattend](https://github.com/guillermo-musumeci/packer-vsphere-iso-windows/blob/master/win2019.base/win2019.base.json)
- [Packer unattended windows installs](https://www.packer.io/guides/automatic-operating-system-installs/autounattend_windows)
