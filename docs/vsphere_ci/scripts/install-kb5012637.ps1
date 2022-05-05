# Powershell script to download and install Windows OS patch KB5012637
#
# USAGE
#    ./install-kb5012637.ps1

# Download the patch file from MSFT
$patchFile = "windows10.0-kb5012637-x64_6a7459b60e226b0ad0d30b34a4be069bee4d2867.msu"
$url = "https://catalog.s.download.windowsupdate.com/c/msdownload/update/software/updt/2022/04/$patchFile"
$dest = "C:\Windows\Temp\$patchFile"
Invoke-WebRequest -Uri $url -OutFile $dest

# Install the patch, bypassing any prompts
cmd.exe /c wusa.exe $dest /quiet /norestart
