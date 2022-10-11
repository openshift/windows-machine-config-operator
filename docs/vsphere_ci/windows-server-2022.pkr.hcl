
variable "os-iso-path" {
  type    = string
  default = "[WorkloadDatastore] windows-iso-images/winsrv2022_sep_2021_x64_dvd.iso"
}

variable "vm-elevated-password" {
  type      = string
  default   = "WindowsPassword"
  sensitive = true
}

variable "vm-elevated-user" {
  type    = string
  default = "Administrator"
}

variable "vm-template-folder" {
  type    = string
  default = "windows-golden-images"
}

variable "vm-template-name" {
  type    = string
  default = "windows-server-2022-template-test"
}

variable "vmtools-iso-path" {
  type    = string
  default = "[WorkloadDatastore] windows-iso-images/vmtools-v11360-windows.iso"
}

variable "vsphere-cluster" {
  type    = string
  default = "Cluster-1"
}

variable "vsphere-datacenter" {
  type    = string
  default = "SDDC-Datacenter"
}

variable "vsphere-datastore" {
  type    = string
  default = "WorkloadDatastore"
}

variable "vsphere-password" {
  type      = string
  default   = "vcenter_password"
  sensitive = true
}

variable "vsphere-server" {
  type    = string
  default = "vcenter.example.com"
}

variable "vsphere-user" {
  type    = string
  default = "vcenter_user"
}

source "vsphere-iso" "windows-server-2022" {
  CPUs                 = "4"
  RAM                  = "16384"
  RAM_reserve_all      = true
  cluster              = "${var.vsphere-cluster}"
  communicator         = "ssh"
  convert_to_template  = "true"
  datacenter           = "${var.vsphere-datacenter}"
  datastore            = "${var.vsphere-datastore}"
  disk_controller_type = ["lsilogic-sas"]
  firmware             = "bios"
  floppy_files         = ["answer-files/autounattend.xml", "answer-files/unattend.xml", "scripts/authorized_keys", "scripts/install-vm-tools.cmd", "scripts/configure-vm-tools.ps1", "scripts/install-openssh.ps1", "scripts/rename-computer.ps1"]
  folder               = "${var.vm-template-folder}"
  guest_os_type        = "windows9Server64Guest"
  insecure_connection  = "true"
  iso_paths            = ["${var.os-iso-path}", "${var.vmtools-iso-path}"]
  network_adapters {
    network      = "dev-segment"
    network_card = "vmxnet3"
  }
  password         = "${var.vsphere-password}"
  shutdown_command = "c:\\windows\\system32\\sysprep\\sysprep.exe /generalize /oobe /shutdown /unattend:a:\\unattend.xml"
  ssh_password     = "${var.vm-elevated-password}"
  ssh_timeout      = "10m"
  ssh_username     = "${var.vm-elevated-user}"
  storage {
    disk_size             = "61440"
    disk_thin_provisioned = true
  }
  username       = "${var.vsphere-user}"
  vcenter_server = "${var.vsphere-server}"
  vm_name        = "${var.vm-template-name}"
  vm_version     = 15
}

build {
  sources = ["source.vsphere-iso.windows-server-2022"]

  provisioner "file" {
    destination = "C:/"
    source      = "scripts/rename-computer.ps1"
  }

  provisioner "powershell" {
    elevated_password = "${var.vm-elevated-password}"
    elevated_user     = "${var.vm-elevated-user}"
    script            = "scripts/install-firewall-rules.ps1"
  }

  provisioner "powershell" {
    elevated_password = "${var.vm-elevated-password}"
    elevated_user     = "${var.vm-elevated-user}"
    script            = "scripts/install-docker.ps1"
  }

  provisioner "powershell" {
    elevated_password = "${var.vm-elevated-password}"
    elevated_user     = "${var.vm-elevated-user}"
    script            = "scripts/install-updates.ps1"
  }

  # Restart to apply the updates
  provisioner "windows-restart" {
    restart_timeout = "1h"
  }

  # We have to run updates again to ensure all updates are installed
  provisioner "powershell" {
    elevated_password = "${var.vm-elevated-password}"
    elevated_user     = "${var.vm-elevated-user}"
    script            = "scripts/install-updates.ps1"
  }

  # Restart again to apply the updates
  provisioner "windows-restart" {
    restart_timeout = "1h"
  }

  # Pause to allow Windows to caalese and executy a dummy command
  provisioner "powershell" {
    elevated_password = "${var.vm-elevated-password}"
    elevated_user     = "${var.vm-elevated-user}"
    inline            = ["dir c:\\"]
    pause_before      = "2m0s"
  }

}
