# Reference: https://learn.microsoft.com/en-us/troubleshoot/windows-server/networking/configure-ipv6-in-windows
New-ItemProperty -Path HKLM:\SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters\ -Name DisabledComponents -PropertyType DWord -Value 0xFF -Force
