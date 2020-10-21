# Quick Start MS Windows Server Datacenter Core with Containers 
This guide assumes the user has a current version of MS Windows Server 2019
Datacenter (core) with containers ready to prepare as a worker node for
OpenShift.

## Enable ssh service
  - Run in admin powershell
```sh
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
Add-WindowsCapability -Online -Name OpenSSH.Client~~~~0.0.1.0

Start-Service sshd
Set-Service -Name sshd -StartupType Automatic
```
## Configure administrators rsa key authentication
  - Run in admin powershell
    
  1. Write ssh public key to file
```sh
$sshPubKey = 'ssh-rsa AAAAB3NzaC...TRUNCATED...AQAAAAB23V6s/L/yU= admin@bastion' 
$sshPubKeyFile = 'administrators_authorized_keys'
$sshDataDir = 'C:\ProgramData\ssh'
New-Item -Path $sshDataDir -Name $sshPubKeyFile -ItemType "file" -Value $sshPubKey  
```

  2. Set permissions on public key file
```sh
$acl = Get-Acl C:\ProgramData\ssh\administrators_authorized_keys
$acl.SetAccessRuleProtection($true, $false)
$administratorsRule = New-Object system.security.accesscontrol.filesystemaccessrule("Administrators","FullControl","Allow")
$systemRule = New-Object system.security.accesscontrol.filesystemaccessrule("SYSTEM","FullControl","Allow")
$acl.SetAccessRule($administratorsRule)
$acl.SetAccessRule($systemRule)
$acl | Set-Acl
```
