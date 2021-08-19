# Powershell script to configure VM Tools
#
# USAGE
#    ./configure-vm-tools.ps1

# download configuration template
Invoke-WebRequest -O tools.conf https://raw.githubusercontent.com/vmware/open-vm-tools/master/open-vm-tools/tools.conf
# exclude network interface
(Get-Content -Path tools.conf) -replace '#exclude-nics=','exclude-nics=' | Set-Content -Force -Path tools.conf
# target location
$toolsConfFilePath="$env:ProgramData\VMware\VMware Tools\tools.conf"
# set configuration
New-Item -ItemType File -Path $toolsConfFilePath  -Force
Move-Item -Path tools.conf  -Destination $toolsConfFilePath -Force
