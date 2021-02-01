Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
$firewallRuleName = "ContainerLogsPort"
$containerLogsPort = "10250"
New-NetFirewallRule -DisplayName $firewallRuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $containerLogsPort -EdgeTraversalPolicy Allow
Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force
Install-Module -Force OpenSSHUtils
Set-Service -Name ssh-agent -StartupType 'Automatic'
Set-Service -Name sshd -StartupType 'Automatic'
Start-Service ssh-agent
Start-Service sshd
$pubKeyConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
$pubKeyConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
$passwordConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
$passwordConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
$authFileConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys','#AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys'
$authFileConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
$pubKeyLocationConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'Match Group administrators','#Match Group administrators'
$pubKeyLocationConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
Restart-Service sshd
New-item -Path $env:USERPROFILE -Name .ssh -ItemType Directory -force
cp a:\authorized_keys C:\Users\Administrator\.ssh\authorized_keys
