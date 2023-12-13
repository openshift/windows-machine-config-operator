package payload

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"io/ioutil"
	"strings"
)

// Payload files
const (
	// payloadDirectory is the directory in the operator image where are all the binaries live
	payloadDirectory = "/payload/"
	// WICDPath is the path to the Windows Instance Config Daemon exe
	WICDPath = payloadDirectory + "windows-instance-config-daemon.exe"
	// KubeletPath contains the path of the kubelet binary. The container image should already have this binary mounted
	KubeletPath = payloadDirectory + "/kube-node/kubelet.exe"
	// KubeProxyPath contains the path of the kube-proxy binary. The container image should already have this binary
	// mounted
	KubeProxyPath = payloadDirectory + "/kube-node/kube-proxy.exe"
	// ContainerdPath contains the path of the containerd binary. The container image should already have this binary
	// mounted
	ContainerdPath = payloadDirectory + "/containerd/containerd.exe"
	//HcsshimPath contains the path of the hcsshim binary. The container image should already have this binary mounted
	HcsshimPath = payloadDirectory + "/containerd/containerd-shim-runhcs-v1.exe"
	// ContainerdConfPath contains the path of the containerd config file.
	ContainerdConfPath = payloadDirectory + "/containerd/containerd_conf.toml"
	// GcpGetHostnameScriptName is the name of the PowerShell script that resolves the hostname for GCP instances
	GcpGetHostnameScriptName = "gcp-get-hostname.ps1"
	// GcpGetValidHostnameScriptPath is the path of the PowerShell script that resolves the hostname for GCP instances
	GcpGetValidHostnameScriptPath = payloadDirectory + "/powershell/" + GcpGetHostnameScriptName
	// WinDefenderExclusionScriptName is the name of the PowerShell script that creates an exclusion for containerd if
	// the Windows Defender Antivirus is active
	WinDefenderExclusionScriptName = "windows-defender-exclusion.ps1"
	// WinDefenderExclusionScriptPath is the path of the PowerShell script that creates an exclusion for containerd if
	// the Windows Defender Antivirus is active
	WinDefenderExclusionScriptPath = payloadDirectory + "/powershell/" + WinDefenderExclusionScriptName
	// HNSPSModule is the path to the powershell module which defines various functions for dealing with Windows HNS
	// networks
	HNSPSModule = payloadDirectory + "/powershell/hns.psm1"
	// cniDirectory is the directory for storing the CNI plugins and the CNI config template
	cniDirectory = "/cni/"
	// HostLocalCNIPlugin is the path of the host-local CNI plugin binary. The container image should already have
	// this binary mounted
	HostLocalCNIPlugin = payloadDirectory + cniDirectory + "host-local.exe"
	// WinBridgeCNIPlugin is the path of the win-bridge CNI plugin binary. The container image should already have
	// this binary mounted
	WinBridgeCNIPlugin = payloadDirectory + cniDirectory + "win-bridge.exe"
	// WinOverlayCNIPlugin is the path of the win-overlay CNI Plugin binary. The container image should already have
	// this binary mounted
	WinOverlayCNIPlugin = payloadDirectory + cniDirectory + "win-overlay.exe"
	// NetworkConfigurationScript is the path for generated Network configuration Script
	NetworkConfigurationScript = payloadDirectory + "/generated/network-conf.ps1"
	// HybridOverlayName is the name of the hybrid overlay executable
	HybridOverlayName = "hybrid-overlay-node.exe"
	// HybridOverlayPath contains the path of the hybrid overlay binary. The container image should already have this
	// binary mounted
	HybridOverlayPath = payloadDirectory + HybridOverlayName
	// CSIProxyPath contains the path of the csi-proxy executable. This should be mounted in the container image.
	CSIProxyPath = payloadDirectory + "csi-proxy/csi-proxy.exe"
	// WindowsExporterName is the name of the Windows metrics exporter executable
	WindowsExporterName = "windows_exporter.exe"
	// WindowsExporterPath contains the path of the windows_exporter binary. The container image should already have
	// this binary mounted
	WindowsExporterPath = payloadDirectory + WindowsExporterName
	// AzureCloudNodeManager is the name of the cloud node manager for Azure platform
	AzureCloudNodeManager = "azure-cloud-node-manager.exe"
	// AzureCloudNodeManagerPath contains the path of the azure cloud node manager binary. The container image should
	// already have this binary mounted
	AzureCloudNodeManagerPath = payloadDirectory + AzureCloudNodeManager
	// TODO: This script is doing both CNI configuration and HNS endpoint creation, two things that aren't necessarily
	//       related. Correct that in: https://issues.redhat.com/browse/WINC-882
	// networkConfTemplate is the template used to generate the network configuration script
	networkConfTemplate = `# This script ensures the contents of the CNI config file is correct, and returns the HNS endpoint IP
$ErrorActionPreference = "Stop"
Import-Module -DisableNameChecking HNS_MODULE_PATH

$cni_template=@'
{
    "cniVersion":"0.2.0",
    "name":"HNS_NETWORK",
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
                "SERVICE_NETWORK_CIDR"
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
                "destinationPrefix": "SERVICE_NETWORK_CIDR",
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

# Generate CNI Config
$hns_network=Get-HnsNetwork  | where { $_.Name -eq 'HNS_NETWORK'}
$subnet=$hns_network.Subnets.AddressPrefix
$cni_template=$cni_template.Replace("ovn_host_subnet",$subnet)
$provider_address=$hns_network.ManagementIP
$cni_template=$cni_template.Replace("provider_address",$provider_address)

# Compare CNI config with existing file, and replace if necessary
$existing_config=""
if(Test-Path -Path CNI_CONFIG_PATH) {
` + "    $existing_config=((Get-Content -Path \"CNI_CONFIG_PATH\" -Raw) -Replace \"`r\",\"\")" + `
}
if($existing_config -ne $cni_template){
    Set-Content -Path "CNI_CONFIG_PATH" -Value $cni_template -NoNewline
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
)

// FileInfo contains information about a file
type FileInfo struct {
	Path   string
	SHA256 string
}

// NewFileInfo returns a pointer to a FileInfo object created from the specified file
func NewFileInfo(path string) (*FileInfo, error) {
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not get contents of file: %w", err)
	}
	return &FileInfo{
		Path:   path,
		SHA256: fmt.Sprintf("%x", sha256.Sum256(contents)),
	}, nil
}

// PopulateNetworkConfScript creates the .ps1 file responsible for CNI configuration
func PopulateNetworkConfScript(clusterCIDR, hnsNetworkName, hnsPSModulePath, cniConfigPath string) error {
	scriptContents, err := generateNetworkConfigScript(clusterCIDR, hnsNetworkName,
		hnsPSModulePath, cniConfigPath)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(NetworkConfigurationScript, []byte(scriptContents), fs.ModePerm)
}

// generateNetworkConfigScript generates the contents of the .ps1 file responsible for CNI configuration
func generateNetworkConfigScript(clusterCIDR, hnsNetworkName, hnsPSModulePath,
	cniConfigPath string) (string, error) {
	networkConfScript := networkConfTemplate
	for key, val := range map[string]string{
		"HNS_NETWORK":          hnsNetworkName,
		"SERVICE_NETWORK_CIDR": clusterCIDR,
		"HNS_MODULE_PATH":      hnsPSModulePath,
		"CNI_CONFIG_PATH":      cniConfigPath,
	} {
		networkConfScript = strings.ReplaceAll(networkConfScript, key, val)
	}
	return networkConfScript, nil
}
