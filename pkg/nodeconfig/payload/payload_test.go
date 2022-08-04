package payload

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateNetworkConfigScript(t *testing.T) {
	expectedOut := `# This script ensures the contents of the CNI config file is correct, and returns the HNS endpoint IP
$ErrorActionPreference = "Stop"
Import-Module -DisableNameChecking c:\k\hns.psm1

$cni_template=@'
{
    "CniVersion":"0.2.0",
    "Name":"OVNKubernetesHNSNetwork",
    "Type":"win-overlay",
    "apiVersion": 2,
    "Capabilities":{
        "portMappings": true,
        "Dns":true
    },
    "Ipam":{
        "Type":"host-local",
        "Subnet":"ovn_host_subnet"
    },
    "Policies":[
    {
        "Name": "EndpointPolicy",
        "Value": {
            "Type": "OutBoundNAT",
            "Settings": {
                "ExceptionList": [
                "10.0.0.1/32"
                ],
                "DestinationPrefix": "",
                "NeedEncap": false
            }
        }
    },
    {
        "Name": "EndpointPolicy",
        "Value": {
            "Type": "SDNROUTE",
            "Settings": {
                "ExceptionList": [],
                "DestinationPrefix": "10.0.0.1/32",
                "NeedEncap": true
            }
        }
    },
    {
        "Name": "EndpointPolicy",
        "Value": {
            "Type": "ProviderAddress",
            "Settings": {
                "ProviderAddress": "provider_address"
            }
        }
    }
    ]
}
'@

# Generate CNI Config
$hns_network=Get-HnsNetwork  | where { $_.Name -eq 'OVNKubernetesHNSNetwork'}
$subnet=$hns_network.Subnets.AddressPrefix
$cni_template=$cni_template.Replace("ovn_host_subnet",$subnet)
$provider_address=$hns_network.ManagementIP
$cni_template=$cni_template.Replace("provider_address",$provider_address)

# Compare CNI config with existing file, and replace if necessary
$existing_config=""
if(Test-Path -Path c:\k\cni.conf) {
    $existing_config= Get-Content -Path "c:\k\cni.conf"
}
if($existing_config -ne $cni_template){
    Set-Content -Path "c:\k\cni.conf" -Value $cni_template -NoNewline
}

# Create HNS endpoint if it doesn't exist
$endpoint = Invoke-HNSRequest GET endpoints | where { $_.Name -eq 'VIPEndpoint'}
if( $endpoint -eq $null) {
    $endpoint = New-HnsEndpoint -NetworkId $hns_network.ID -Name "VIPEndpoint"
    Attach-HNSHostEndpoint -EndpointID $endpoint.ID -CompartmentID 1
}

# Return HNS endpoint IP
(Get-NetIPConfiguration -AllCompartments -All -Detailed | where { $_.NetAdapter.LinkLayerAddress -eq $endpoint.MacAddress }).IPV4Address.IPAddress.Trim()
`
	actual, err := generateNetworkConfigScript("10.0.0.1/32",
		"OVNKubernetesHNSNetwork", "c:\\k\\hns.psm1", "c:\\k\\cni.conf")
	require.NoError(t, err)
	assert.Equal(t, string(expectedOut), actual)
}
