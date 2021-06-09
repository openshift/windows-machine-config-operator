# Building Windows VM images in vSphere

## Goals
This document focuses on building Windows images in vSphere 6.7 environment used in CI but can be used as example for
other vSphere environments. We use Packer to automate installation and configuration of the Windows images. Packer
generates a vSphere VM template that can be converted to a VM. After converting the template to VM, the machine-api
can use this shutdown VM for subsequent Windows VM cloning. The shutdown VM's name is the one we use in 
machineset's providerSpec.template. The following steps need to be executed from the bastion instance or any instance 
that has access to the vSphere environent.

## Installing Packer

In case of our vSphere environment, we need to install Packer 1.6.6 on the bastion host. The installation steps are:

- Download [Packer](https://www.packer.io/downloads)
    - `curl -o packer.zip https://releases.hashicorp.com/packer/1.6.6/packer_1.6.6_linux_amd64.zip`
- Unzip using zip utility on the host
    - `gunzip -S .zip packer.zip`
- `chmod +x packer`
- Update $PATH to include the Packer binary

## Prerequsite files
- Please ensure `scripts` directory is present in the location where you are running Packer from on the bastion and 
has the following files:

- autounattend.xml
- install-vm-tools.cmd
- install-openssh.ps1
- install-firewall-rules.ps1 
- authorized_keys
- install-docker.ps1

The `scripts/authorized_keys` file must be edited to contain a public key. The private key associated with the public 
key is what will be used by WMCO to configure VMs created from Windows VM. After deploying WMCO, this private key will 
be provided by the user in the form of a Secret.

The `scripts/autounattend.xml` file must be edited to change the value of `WindowsPassword` to a user provided password.

The provided [autounattend.xml](scripts/autounattend.xml)
- Installs VMWare tools
- Runs `install-openssh.ps1` script which installs and configures OpenSSH Server
- Runs `install-firewall-rules.ps1` script which configures the firewall rules
- Runs `install-docker.ps1` script which installs Docker

This autounattend script is different from the [autounattend script](../unattend.xml) as this script does Windows OS
installation as well.

References:
- [Sample autounattend](https://github.com/guillermo-musumeci/packer-vsphere-iso-windows/blob/master/win2019.base/win2019.base.json)
- [Packer unattended windows installs](https://www.packer.io/guides/automatic-operating-system-installs/autounattend_windows)

### Sample Packer build file

Packer needs a build file which specifies the how the VM template should be built. A sample build file is shown below.
```
{
   "builders":[
      {
         "CPUs":"{{user `vm-cpu-num`}}",
         "RAM":"{{user `vm-mem-size`}}",
         "RAM_reserve_all":true,
         "cluster":"{{user `vsphere-cluster`}}",
         "communicator":"ssh",
         "convert_to_template":"true",
         "datacenter":"{{user `vsphere-datacenter`}}",
         "datastore":"{{user `vsphere-datastore`}}",
         "disk_controller_type":"lsilogic-sas",
         "firmware":"bios",
         "floppy_files":[
            "scripts/autounattend.xml",
            "scripts/install-vm-tools.cmd",
            "scripts/install-openssh.ps1",
            "scripts/install-firewall-rules.ps1",
            "scripts/authorized_keys",
            "scripts/install-docker.ps1"
         ],
         "folder":"{{user `vsphere-folder`}}",
         "guest_os_type":"windows9Server64Guest",
         "insecure_connection":"true",
         "iso_paths":[
            "{{user `os_iso_path`}}",
            "<tools_iso_path>"
         ],
         "network_adapters":[
            {
               "network":"{{user `vsphere-network`}}",
               "network_card":"vmxnet3"
            }
         ],
         "password":"{{user `vsphere-password`}}",
         "storage":[
            {
               "disk_size":"{{user `vm-disk-size`}}",
               "disk_thin_provisioned":true
            }
         ],
         "type":"vsphere-iso",
         "username":"{{user `vsphere-user`}}",
         "vcenter_server":"{{user `vsphere-server`}}",
         "vm_name":"{{user `vm-name`}}",
         "ssh_password":"{{user `winadmin-password`}}",
         "ssh_username":"Administrator",
         "ssh_timeout":"50m"
      }
   ],
   "provisioners":[
      {
	"pause_before": "120s",
	"inline":["ipconfig"],
        "type":"powershell",
	"elevated_user": "Administrator",
        "elevated_password": "{{user `winadmin-password`}}",
	"max_retries":"10"
      }
   ],
   "sensitive-variables":[
      "vsphere_password",
      "winadmin_password"
   ],
   "variables":{
      "os_iso_path":"<Windows_os_iso_path>",
      "vm-cpu-num":"2",
      "vm-disk-size":"128000",
      "vm-mem-size":"4096",
      "vm-name":"<template_to_be_created>",
      "vsphere-cluster":"Cluster-1",
      "vsphere-datacenter":"<datacenter_name>",
      "vsphere-datastore":"<datastore_name>",
      "vsphere-folder":"<folder_where_template_gets_built>",
      "vsphere-network":"network_segment_name",
      "vsphere-password":"<password>",
      "vsphere-server":"<server_name>",
      "vsphere-user":"<user_name>",
      "winadmin-password":"<Windows_os_password>"
   }
}
```

### Variables
- `<tools_iso_path>` - Path where VMWare tools iso is available in vSphere datacenter
- `<Windows_os_iso_path>` - Path where Windows ISO was dowloaded to in vSphere datacenter
- `<template_to_be_created>` - Name of the Windows vSphere template that will be created by successful Packer run in 
 			       vSphere datacenter
- `<datacenter_name>` - Name of the vSphere datacenter
- `<datastore_name>` - Name of the vSphere datastore
- `<folder_where_template_gets_built>` - Path where Windows template gets built by successful Packer run in 
					 vSphere datacenter
- `<password>` - vSphere environment password
- `<server_name>` - vSphere environment URL
- `<user_name>` - vSphere username to login
- `<Windows_os_password>` - Password for the Windows template

## What actually happens during build

Packer mounts the Windows iso and starts the VM. 
- All the files in `floppy_files` section of your build file will be copied to the floppy disk of the mounted iso 
 which is represented as `a:\` drive in the Windows VM
- [Autounattend.xml](scripts/autounattend.xml) is a special file in Windows which gets automatically executed once the
VM starts. You can specify all the commands that needs to executed on first boot.

## Building with Packer

Packer relies on a [build file](build.json) for VM image creation. 

To build:

`packer build build.json`

To forcefully rebuild the template:

`packer build -force build.json`

To enable logging:

`PACKER_LOG=1 packer build -force build.json`

## Post Packer build

Once the Packer build completes successfully, the template must be converted to a VM.

![Convert to VM](images/VMConversion.png)

The machineset's providerSpec.template should be populated with the name of this VM.

