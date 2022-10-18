# This script returns a mutated hostname if the length is longer than 63 characters for GCP. The hostname will be
# the lesser of 63 characters after the first dot in the FQDN.
#
# To get the original hostname for a GCP compute instance, the instance metadata service is invoked using its
# DNS address (metadata.google.internal) as the prefered method over the IPv4 address or other alternatives like
# the GoogleCloud Tools for PowerShell library (https://cloud.google.com/tools/powershell/docs/quickstart) since
# they may not be available, specially in the BYOH use case.
#
# See instance metadata documentaion for GCP https://cloud.google.com/compute/docs/metadata/default-metadata-values

# MAX_LENGTH is the maximum number of character allowed for the instance's hostname in GCP
$MAX_LENGTH = 63

# get hostname from the instance metadata service
$hostname=(Invoke-RestMethod -Headers @{'Metadata-Flavor'='Google'} `
            -Uri "http://metadata.google.internal/computeMetadata/v1/instance/hostname")

# check hostname length
if ($hostname.Length -le $MAX_LENGTH) {
    # up to 63 characters is good, nothing to do!
    return $hostname
}

# find the index of first dot in the FQDN
$firstDotIndex=$hostname.IndexOf(".")
if (($firstDotIndex -gt 0) -and ($firstDotIndex -le $MAX_LENGTH) ) {
    # and return first part of the FQDN
    return $hostname.Substring(0, $firstDotIndex)
}

# otherwise, return the first 63 characters of the hostname
return $hostname.Substring(0, $MAX_LENGTH)
