# Powershell script to configure Firewall Rules in Windows Server 1809 and later.
#
# USAGE
#    ./install-firewall-rules.ps1 

# create firewall rule to allow Container Logs on port 10250
New-NetFirewallRule -DisplayName "ContainerLogsPort" -LocalPort 10250 -Enabled True -Direction Inbound -Protocol TCP -Action Allow -EdgeTraversalPolicy Allow

# success
exit 0
