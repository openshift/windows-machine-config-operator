package wellknownlocations

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
	// IgnoreWgetPowerShellPath contains the path of the powershell script which allows wget to ignore certs. The
	// container image should already have this mounted
	IgnoreWgetPowerShellPath = payloadDirectory + "/powershell/wget-ignore-cert.ps1"
	// HNSPSModule is the path to the powershell module which defines various functions for dealing with Windows HNS
	// networks
	HNSPSModule = payloadDirectory + "/powershell/hns.psm1"
	// cniDirectory is the directory for storing the CNI plugins and the CNI config template
	cniDirectory = "/cni/"
	// FlannelCNIPluginPath is the path of the flannel CNI plugin binary. The container image should already have this
	// binary mounted
	FlannelCNIPluginPath = payloadDirectory + cniDirectory + "flannel.exe"
	// HostLocalCNIPluginPath is the path of the host-local CNI plugin binary. The container image should already have
	// this binary mounted
	HostLocalCNIPlugin = payloadDirectory + cniDirectory + "host-local.exe"
	// WinBridgeCNIPluginPath is the path of the win-bridge CNI plugin binary. The container image should already have
	// this binary mounted
	WinBridgeCNIPlugin = payloadDirectory + cniDirectory + "win-bridge.exe"
	// WinOverlayCNIPluginPath is the path of the win-overlay CNI Plugin binary. The container image should already have
	// this binary mounted
	WinOverlayCNIPlugin = payloadDirectory + cniDirectory + "win-overlay.exe"
	// CNIConfigTemplatePath is the path for CNI config template
	CNIConfigTemplatePath = payloadDirectory + cniDirectory + "cni-conf-template.json"
	// hybridOverlayName is the name of the hybrid overlay executable
	HybridOverlayName = "hybrid-overlay-node.exe"
	// HybridOverlayPath contains the path of the hybrid overlay binary. The container image should already have this
	// binary mounted
	HybridOverlayPath = payloadDirectory + HybridOverlayName
)
