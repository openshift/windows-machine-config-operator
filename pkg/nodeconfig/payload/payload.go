package payload

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// sha25sumColumnSeparator is the separator used in the utility sha256sum. This is between the file name and the sha256 sum value.
	sha256sumColumnSeparator = "  "
	// payloadDirectory is the directory in the operator image where are all the binaries live
	payloadDirectory = "/payload/"
	// WICDPath is the path to the Windows Instance Config Daemon exe
	WICDPath = payloadDirectory + "windows-instance-config-daemon.exe.tar.gz"
	// KubeletPath contains the path of the kubelet binary. The container image should already have this binary mounted
	KubeletPath = payloadDirectory + "/kube-node/kubelet.exe.tar.gz"
	// KubeProxyPath contains the path of the kube-proxy binary. The container image should already have this binary
	// mounted
	KubeProxyPath = payloadDirectory + "/kube-node/kube-proxy.exe.tar.gz"
	// KubeLogRunnerPath contains the path of the kube-log-runner binary.
	KubeLogRunnerPath = payloadDirectory + "/kube-node/kube-log-runner.exe.tar.gz"
	// ContainerdPath contains the path of the containerd binary. The container image should already have this binary
	// mounted
	ContainerdPath = payloadDirectory + "/containerd/containerd.exe.tar.gz"
	//HcsshimPath contains the path of the hcsshim binary. The container image should already have this binary mounted
	HcsshimPath = payloadDirectory + "/containerd/containerd-shim-runhcs-v1.exe.tar.gz"
	// ContainerdConfPath contains the path of the containerd config file.
	ContainerdConfPath = payloadDirectory + "/containerd/containerd_conf.toml.tar.gz"
	// GcpGetHostnameScriptName is the name of the PowerShell script that resolves the hostname for GCP instances
	GcpGetHostnameScriptName = "gcp-get-hostname.ps1.tar.gz"
	// GcpGetValidHostnameScriptPath is the path of the PowerShell script that resolves the hostname for GCP instances
	GcpGetValidHostnameScriptPath = payloadDirectory + "/powershell/" + GcpGetHostnameScriptName
	// WinDefenderExclusionScriptName is the name of the PowerShell script that creates an exclusion for containerd if
	// the Windows Defender Antivirus is active
	WinDefenderExclusionScriptName = "windows-defender-exclusion.ps1.tar.gz"
	// WinDefenderExclusionScriptPath is the path of the PowerShell script that creates an exclusion for containerd if
	// the Windows Defender Antivirus is active
	WinDefenderExclusionScriptPath = payloadDirectory + "/powershell/" + WinDefenderExclusionScriptName
	// HNSPSModule is the path to the powershell module which defines various functions for dealing with Windows HNS
	// networks
	HNSPSModule = payloadDirectory + "/powershell/hns.psm1.tar.gz"
	// cniDirectory is the directory for storing the CNI plugins and the CNI config template
	cniDirectory = "/cni/"
	// HostLocalCNIPlugin is the path of the host-local CNI plugin binary. The container image should already have
	// this binary mounted
	HostLocalCNIPlugin = payloadDirectory + cniDirectory + "host-local.exe.tar.gz"
	// WinBridgeCNIPlugin is the path of the win-bridge CNI plugin binary. The container image should already have
	// this binary mounted
	WinBridgeCNIPlugin = payloadDirectory + cniDirectory + "win-bridge.exe.tar.gz"
	// WinOverlayCNIPlugin is the path of the win-overlay CNI Plugin binary. The container image should already have
	// this binary mounted
	WinOverlayCNIPlugin = payloadDirectory + cniDirectory + "win-overlay.exe.tar.gz"
	// NetworkConfigurationScript is the path for generated Network configuration Script
	NetworkConfigurationScript = payloadDirectory + "/generated/network-conf.ps1.tar.gz"
	// HybridOverlayName is the name of the hybrid overlay executable
	HybridOverlayName = "hybrid-overlay-node.exe.tar.gz"
	// HybridOverlayPath contains the path of the hybrid overlay binary. The container image should already have this
	// binary mounted
	HybridOverlayPath = payloadDirectory + HybridOverlayName
	// CSIProxyPath contains the path of the csi-proxy executable. This should be mounted in the container image.
	CSIProxyPath = payloadDirectory + "csi-proxy/csi-proxy.exe.tar.gz"
	// WindowsExporterName is the name of the Windows metrics exporter executable
	WindowsExporterName = "windows_exporter.exe.tar.gz"
	// WindowsExporterDirectory is the directory for storing the windows-exporter binary and the TLS webconfig file
	WindowsExporterDirectory = "windows-exporter/"
	// WindowsExporterPath contains the path of the windows_exporter binary. The container image should already have
	// this binary mounted
	WindowsExporterPath = payloadDirectory + WindowsExporterDirectory + WindowsExporterName
	// TLSConfPath contains the path of the TLS config file
	TLSConfPath = payloadDirectory + WindowsExporterDirectory + "windows-exporter-webconfig.yaml.tar.gz"
	// ECRCredentialProviderPath is the path to ecr-credential-provider.exe
	ECRCredentialProviderPath = payloadDirectory + "ecr-credential-provider.exe.tar.gz"
	// AzureCloudNodeManager is the name of the cloud node manager for Azure platform
	AzureCloudNodeManager = "azure-cloud-node-manager.exe.tar.gz"
	// AzureCloudNodeManagerPath contains the path of the azure cloud node manager binary. The container image should
	// already have this binary mounted
	AzureCloudNodeManagerPath = payloadDirectory + AzureCloudNodeManager
	// TODO: This script is doing both CNI configuration and HNS endpoint creation, two things that aren't necessarily
	//       related. Correct that in: https://issues.redhat.com/browse/WINC-882
	// networkConfTemplate is the template used to generate the network configuration script
	networkConfTemplate = `# This script ensures the contents of the CNI config file is correct, and creates the kube-proxy config file.

param(
    [string]$hostnameOverride,
    [string]$clusterCIDR,
    [string]$kubeConfigPath,
    [string]$kubeProxyConfigPath,
    [string]$verbosity
)
  # this compares the config with the existing config, and replaces if necessary
  function Compare-And-Replace-Config {
    param (
        [string]$ConfigPath,
        [string]$NewConfigContent
    )
    
    # Read existing config content
    $existing_config = ""
    if (Test-Path -Path $ConfigPath) {
        $config_file_content = Get-Content -Path $ConfigPath -Raw
        if ($config_file_content -ne $null) {
` + "        $existing_config=$config_file_content.Replace(\"`r\",\"\")" + `
        }
    }
    
    if ($existing_config -ne $NewConfigContent) {
        Set-Content -Path $ConfigPath -Value $NewConfigContent -NoNewline
    }
  }

# This retries getting the HNS-network until it succeeds
function Retry-GetHnsNetwork {
     $retryCount = 20
     $retryDelaySeconds = 1

     $attempt = 1
 
     while ($attempt -le $retryCount) {
         try {
             $hns_network = Get-HnsNetwork | Where-Object { $_.Name -eq 'HNS_NETWORK' }
             
             # Check if hns_network is null
             if ($null -eq $hns_network) {
                 Write-Host "Attempt $attempt returned null. Retrying in $retryDelaySeconds seconds..."
             } else {
                 Write-Host "Found HNS_NETWORK on attempt $attempt"
                 return $hns_network 
             }
         } catch {
             Write-Host "Attempt $attempt failed: $_"
         }
         Start-Sleep -Seconds $retryDelaySeconds
         $attempt++
     }
     Write-Host "Max retry attempts reached."
     return $null
}

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

$hns_network = Retry-GetHnsNetwork
# If the HNS network is never found, quit.
if ($null -eq $hns_network) {
  throw "cannot find HNS network with name HNS_NETWORK"
}

# Generate CNI Config
$subnet=$hns_network.Subnets.AddressPrefix
$cni_template=$cni_template.Replace("ovn_host_subnet",$subnet)
$provider_address=$hns_network.ManagementIP
$cni_template=$cni_template.Replace("provider_address",$provider_address)

Compare-And-Replace-Config -ConfigPath CNI_CONFIG_PATH -NewConfigContent $cni_template

# Create HNS endpoint if it doesn't exist
$endpoint = Invoke-HNSRequest GET endpoints | where { $_.Name -eq 'VIPEndpoint'}
if( $endpoint -eq $null) {
    $endpoint = New-HnsEndpoint -NetworkId $hns_network.ID -Name "VIPEndpoint"
    Attach-HNSHostEndpoint -EndpointID $endpoint.ID -CompartmentID 1
}
# Get HNS endpoint IP
$sourceVip = (Get-NetIPConfiguration -AllCompartments -All -Detailed | where { $_.NetAdapter.LinkLayerAddress -eq $endpoint.MacAddress }).IPV4Address.IPAddress.Trim()

#Kube Proxy configuration

$kube_proxy_config=@"
kind: KubeProxyConfiguration
apiVersion: kubeproxy.config.k8s.io/v1alpha1
featureGates:
  WinDSR: true
  WinOverlay: true
clientConnection:
  kubeconfig: $kubeConfigPath
  acceptContentTypes: ''
  contentType: ''
  qps: 0
  burst: 0
logging:
  flushFrequency: 0
  verbosity: $verbosity
  options:
    text:
      infoBufferSize: '0'
    json:
      infoBufferSize: '0'
hostnameOverride: $hostnameOverride
bindAddress: ''
healthzBindAddress: ''
metricsBindAddress: ''
bindAddressHardFail: false
enableProfiling: false
showHiddenMetricsForVersion: ''
mode: kernelspace
iptables:
  masqueradeBit: null
  masqueradeAll: false
  localhostNodePorts: null
  syncPeriod: 0s
  minSyncPeriod: 0s
ipvs:
  syncPeriod: 0s
  minSyncPeriod: 0s
  scheduler: ''
  excludeCIDRs: null
  strictARP: false
  tcpTimeout: 0s
  tcpFinTimeout: 0s
  udpTimeout: 0s
nftables:
  masqueradeBit: null
  masqueradeAll: false
  syncPeriod: 0s
  minSyncPeriod: 0s
winkernel:
  networkName: OVNKubernetesHybridOverlayNetwork
  sourceVip: $sourceVip
  enableDSR: true
  rootHnsEndpointName: ''
  forwardHealthCheckVip: false
detectLocalMode: ''
detectLocal:
  bridgeInterface: ''
  interfaceNamePrefix: ''
clusterCIDR: $clusterCIDR
nodePortAddresses: null
oomScoreAdj: null
conntrack:
  maxPerCore: null
  min: null
  tcpEstablishedTimeout: null
  tcpCloseWaitTimeout: null
  tcpBeLiberal: false
  udpTimeout: 0s
  udpStreamTimeout: 0s
configSyncPeriod: 0s
portRange: ''
"@

# Generate kube-proxy config 
Compare-And-Replace-Config -ConfigPath $kubeProxyConfigPath -NewConfigContent $kube_proxy_config
`
)

// This is okay to be global as for each program run, this can only have one valid instance due it representing the
// actual state of the /payload directory.
// This should not be written to once the operator controllers have started, as it is not threadsafe to read+write at the same time.
var shaMap map[string]string

// PopulateSHAMap populates the global shaMap with the SHA256sums from the payload
func PopulateSHAMap() error {
	data, err := os.ReadFile("/payload/sha256sum")
	if err != nil {
		return fmt.Errorf("error reading from /payload/sha256sum: %w", err)
	}
	shaMap, err = parseShaFile(data)
	if err != nil {
		return err
	}
	return nil
}

// parseShaFile returns a mapping of file names to SHA256 sums
// input should be of the format generated by the sha256sum binary: `SHA256SUM  filepath`
func parseShaFile(fileData []byte) (map[string]string, error) {
	m := make(map[string]string)
	for _, line := range strings.Split(string(fileData), "\n") {
		lineContents := strings.TrimSpace(line)
		if len(lineContents) == 0 {
			continue
		}
		columnSplit := strings.SplitN(lineContents, sha256sumColumnSeparator, 2)
		if len(columnSplit) != 2 || len(columnSplit[0]) == 0 || len(columnSplit[1]) == 0 {
			return nil, fmt.Errorf("unexpected sha256sum line format %s", line)
		}
		fileName := filepath.Base(columnSplit[1])
		if _, present := m[fileName]; present {
			return nil, fmt.Errorf("duplicate sha256sum entry for %s", fileName)
		}
		m[fileName] = columnSplit[0]
	}
	return m, nil
}

// CompressedFileInfo contains information about a compressed file
type CompressedFileInfo struct {
	// Path to a compressed (.tar.gz) file
	Path string
	// SHA256 sum of the file when uncompressed
	SHA256 string
}

// NewCompressedFileInfo returns a pointer to a CompressedFileInfo object created from the specified .tar.gz file
func NewCompressedFileInfo(path string) (*CompressedFileInfo, error) {
	uncompressedName := strings.TrimSuffix(filepath.Base(path), ".tar.gz")
	val, present := shaMap[uncompressedName]
	if !present {
		return nil, fmt.Errorf("missing SHA256Sum for %s", uncompressedName)
	}
	return &CompressedFileInfo{
		Path:   path,
		SHA256: val,
	}, nil
}

// PopulateNetworkConfScript creates the .ps1 file responsible for CNI configuration
func PopulateNetworkConfScript(clusterCIDR, hnsNetworkName, hnsPSModulePath, cniConfigPath string) error {
	scriptContents, err := generateNetworkConfigScript(clusterCIDR, hnsNetworkName,
		hnsPSModulePath, cniConfigPath)
	if err != nil {
		return err
	}
	fileName := strings.TrimSuffix(filepath.Base(NetworkConfigurationScript), ".tar.gz")
	shaMap[fileName] = fmt.Sprintf("%x", sha256.Sum256([]byte(scriptContents)))
	compressedFile, err := os.Create(NetworkConfigurationScript)
	if err != nil {
		return fmt.Errorf("failed to create target file: %w", err)
	}
	defer compressedFile.Close()
	return createTarGzFile([]byte(scriptContents), fileName, compressedFile)
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

// Creates a .tar.gz archive from file data
func createTarGzFile(data []byte, fileName string, outWriter io.Writer) error {
	// Chain writers: File -> Gzip -> Tar
	gzipWriter := gzip.NewWriter(outWriter)
	defer gzipWriter.Close()
	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	header := &tar.Header{
		Name:    fileName,
		Size:    int64(len(data)),
		Mode:    0644,
		ModTime: time.Now(),
	}

	if err := tarWriter.WriteHeader(header); err != nil {
		return fmt.Errorf("failed to write tar header: %w", err)
	}
	if _, err := tarWriter.Write(data); err != nil {
		return fmt.Errorf("failed to write data to tar writer: %w", err)
	}
	return nil
}
