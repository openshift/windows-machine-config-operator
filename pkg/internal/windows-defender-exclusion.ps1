# This script creates an exclusion for the given file if the Windows Defender antivirus is running on the instance.
# No action taken otherwise. 
# If getting the antivirus process or creating the exclusion fails unexpectedly, return the error.
# Returns nothing otherwise.

# USAGE
#    windows-defender-exclusion.ps1 [OPTIONS] <file_path>
# OPTIONS
#    -BinPath                 path to the file that should be excluded
# EXAMPLES
#    windows-defender-exclusion.ps1 -BinPath "C:\k\containerd\containerd.exe"


param(
    [Parameter(Mandatory=$true)] [String] $BinPath
)

# Check if the process associated with Windows Defender exists. Reference:
# https://learn.microsoft.com/en-us/microsoft-365/security/defender-endpoint/microsoft-defender-antivirus-compatibility?view=o365-worldwide#use-windows-powershell-to-confirm-that-microsoft-defender-antivirus-is-running
try {
    # The winDefenderProcess variable is used to suppress the command's stdout as the value is not relevant; only the
    # error is relevant, and errors are not suppressed.
    $winDefenderProcess = Get-Process -Name MsMpEng -ErrorAction Stop
    # No error means the process exists, so create exclusion
    Add-MpPreference -ExclusionProcess $BinPath 
}
catch [Microsoft.PowerShell.Commands.ProcessCommandException] {
    # Process does not exist, do nothing
}
