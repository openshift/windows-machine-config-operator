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
    "cniVersion":"0.2.0",
    "name":"OVNKubernetesHNSNetwork",
    "type":"win-overlay",
    "apiVersion": 2,
    "capabilities":{
        "portMappings": true,
        "dns":true
    },
    "ipam":{
        "type":"host-local",
        "subnet":"ovn_host_subnet"
    },
    "policies":[
    {
        "name": "EndpointPolicy",
        "value": {
            "type": "OutBoundNAT",
            "settings": {
                "exceptionList": [
                "10.0.0.1/32"
                ],
                "destinationPrefix": "",
                "needEncap": false
            }
        }
    },
    {
        "name": "EndpointPolicy",
        "value": {
            "type": "SDNRoute",
            "settings": {
                "exceptionList": [],
                "destinationPrefix": "10.0.0.1/32",
                "needEncap": true
            }
        }
    },
    {
        "name": "EndpointPolicy",
        "value": {
            "type": "ProviderAddress",
            "settings": {
                "providerAddress": "provider_address"
            }
        }
    }
    ]
}
'@

# from https://github.com/kubernetes-sigs/sig-windows-tools/blob/fbe00b42e2a5cca06bc182e1b6ee579bd65ed1b5/hostprocess/flannel/kube-proxy/start.ps1
function GetSourceVip($NetworkName)
{
    mkdir -force c:\k\cni\sourcevip | Out-Null
    $sourceVipJson = [io.Path]::Combine("c:\k\cni", "sourcevip",  "sourceVip.json")
    $sourceVipRequest = [io.Path]::Combine("c:\k\cni", "sourcevip", "sourceVipRequest.json")

    if (Test-Path $sourceVipJson) {
        $sourceVipJSONData = Get-Content $sourceVipJson | ConvertFrom-Json
        $vip = $sourceVipJSONData.ip4.ip.Split("/")[0]
        return $vip
    }

    $hnsNetwork = Get-HnsNetwork | ? Name -EQ $NetworkName.ToLower()
    $subnet = $hnsNetwork.Subnets[0].AddressPrefix

    $ipamConfig = @"
    {"cniVersion": "0.2.0", "name": "$NetworkName", "ipam":{"type":"host-local","ranges":[[{"subnet":"$subnet"}]],"dataDir":"/var/lib/cni/networks"}}
"@

    $ipamConfig | Out-File $sourceVipRequest

    $env:CNI_COMMAND="ADD"
    $env:CNI_CONTAINERID="dummy"
    $env:CNI_NETNS="dummy"
    $env:CNI_IFNAME="dummy"
    $env:CNI_PATH="c:\k\cni"

    # reserve an ip address for source VIP, a requirement for kubeproxy in overlay mode
    Get-Content $sourceVipRequest | c:\k\cni\host-local.exe | Out-File $sourceVipJson

    Remove-Item env:CNI_COMMAND
    Remove-Item env:CNI_CONTAINERID
    Remove-Item env:CNI_NETNS
    Remove-Item env:CNI_IFNAME
    Remove-Item env:CNI_PATH

    $sourceVipJSONData = Get-Content $sourceVipJson | ConvertFrom-Json
    $vip = $sourceVipJSONData.ip4.ip.Split("/")[0]
    return $vip
}

# Generate CNI Config
$hns_network=Get-HnsNetwork  | where { $_.Name -eq 'OVNKubernetesHNSNetwork'}
$subnet=$hns_network.Subnets.AddressPrefix
$cni_template=$cni_template.Replace("ovn_host_subnet",$subnet)
$provider_address=$hns_network.ManagementIP
$cni_template=$cni_template.Replace("provider_address",$provider_address)

# Compare CNI config with existing file, and replace if necessary
$existing_config=""
if(Test-Path -Path c:\k\cni\config\cni.conf) {
` + "    $existing_config=((Get-Content -Path \"c:\\k\\cni\\config\\cni.conf\" -Raw) -Replace \"`r\",\"\")" + `
}
if($existing_config -ne $cni_template){
    Set-Content -Path "c:\k\cni\config\cni.conf" -Value $cni_template -NoNewline
}

# Return source VIP for HNS network
(GetSourceVip("OVNKubernetesHNSNetwork"))
`
	actual, err := generateNetworkConfigScript("10.0.0.1/32",
		"OVNKubernetesHNSNetwork", "c:\\k\\hns.psm1", "c:\\k\\cni", "c:\\k\\cni\\config\\cni.conf")
	require.NoError(t, err)
	assert.Equal(t, string(expectedOut), actual)
}
