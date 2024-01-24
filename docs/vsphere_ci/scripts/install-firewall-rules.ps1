# Powershell script to configure Firewall Rules in Windows Server 1809 and later.
#
# USAGE
#    ./install-firewall-rules.ps1 

# Allow incoming connections for container logs and metrics
New-NetFirewallRule -DisplayName "ContainerLogsPort" -LocalPort 10250 -Enabled True -Direction Inbound -Protocol TCP -Action Allow -EdgeTraversalPolicy Allow
New-NetFirewallRule -DisplayName "WindowsExporter" -LocalPort 9182 -Enabled True -Direction Inbound -Protocol TCP -Action Allow -EdgeTraversalPolicy Allow

# success
exit 0
