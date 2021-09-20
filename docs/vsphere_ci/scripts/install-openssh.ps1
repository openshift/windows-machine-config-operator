# Powershell script to configure OpenSSH Server in Windows Server 1809 and later.
#
# USAGE
#    ./install-openssh.ps1
#    ./install-openssh.ps1 <path_to_key_file>
#    ./install-openssh.ps1 -keyfile=<path_to_key_file>
#
# OPTIONS
#    $1     <path_to_key_file>      Path to public key file (Default: authorized_keys)

# define param for key file path
param ($keyfile='authorized_keys')
# validate given keyfile
if (-not(Test-Path -Path $keyfile -PathType Leaf)) {
    # log error and stop
    Write-Error -ErrorAction Stop -Message "Cannot find file: $keyfile"
}

# install OpenSSH server (See: https://docs.microsoft.com/en-us/windows-server/administration/openssh/openssh_install_firstuse)
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
# set service startup type
Set-Service -Name ssh-agent -StartupType 'Automatic'
Set-Service -Name sshd -StartupType 'Automatic'
# start service
Start-Service ssh-agent
Start-Service sshd
# configure key based-authentication
$sshdConfigFilePath = "$env:ProgramData\ssh\sshd_config"
$pubKeyConf = (Get-Content -path $sshdConfigFilePath) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
$pubKeyConf | Set-Content -Path $sshdConfigFilePath
$passwordConf = (Get-Content -path $sshdConfigFilePath) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
$passwordConf | Set-Content -Path $sshdConfigFilePath
# create key file in configuration
$authorizedKeyConf = "$env:ProgramData\ssh\administrators_authorized_keys"
New-Item -Force $authorizedKeyConf
# setup the provided authorized public key
Get-Content $keyfile | Out-File $authorizedKeyConf -Encoding ascii
# configure file acl
$acl = Get-Acl $authorizedKeyConf
# disable inheritance
$acl.SetAccessRuleProtection($true, $false)
# set full control for Administrators
$administratorsRule = New-Object system.security.accesscontrol.filesystemaccessrule("Administrators","FullControl","Allow")
$acl.SetAccessRule($administratorsRule)
# set full control for SYSTEM
$systemRule = New-Object system.security.accesscontrol.filesystemaccessrule("SYSTEM","FullControl","Allow")
$acl.SetAccessRule($systemRule)
# apply file acl
$acl | Set-Acl
# restart service
Restart-Service sshd
# success
exit 0
