package payload

import (
	"crypto/sha256"
	"fmt"
	"io/ioutil"

	"github.com/pkg/errors"
)

// Payload files
const (
	// payloadDirectory is the directory in the operator image where are all the binaries live
	payloadDirectory = "/payload/"
	// WmcbPath contains the path of the Windows Machine Config Bootstrapper binary. The container image should already
	// have this binary mounted
	WmcbPath = payloadDirectory + "wmcb.exe"
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
	// IgnoreWgetPowerShellPath contains the path of the powershell script which allows wget to ignore certs. The
	// container image should already have this mounted
	IgnoreWgetPowerShellPath = payloadDirectory + "/powershell/wget-ignore-cert.ps1"
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
	// CNIConfigTemplatePath is the path for CNI config template
	CNIConfigTemplatePath = payloadDirectory + cniDirectory + "cni-conf-template.json"
	// HybridOverlayName is the name of the hybrid overlay executable
	HybridOverlayName = "hybrid-overlay-node.exe"
	// HybridOverlayPath contains the path of the hybrid overlay binary. The container image should already have this
	// binary mounted
	HybridOverlayPath = payloadDirectory + HybridOverlayName
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
		return nil, errors.Wrap(err, "could not get contents of file")
	}
	return &FileInfo{
		Path:   path,
		SHA256: fmt.Sprintf("%x", sha256.Sum256(contents)),
	}, nil
}
