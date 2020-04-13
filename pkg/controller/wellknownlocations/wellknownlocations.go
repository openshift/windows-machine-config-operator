package wellknownlocations

const (
	// CloudCredentialsPath contains the path to the file where cloud credentials are stored. This would've been mounted
	// as a secret by user
	CloudCredentialsPath = "/etc/cloud/credentials"
	// PrivateKeyPath contains the path to the private key which is used in decrypting password in case of AWS
	// cloud provider. This would have been mounted as a secret by user
	// TODO: Jira story for validation: https://issues.redhat.com/browse/WINC-316
	PrivateKeyPath = "/etc/private-key/private-key.pem"
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
	// FlannelCNIPluginPath is the path of the flannel CNI plugin binary. The container image should already have this
	// binary mounted
	FlannelCNIPluginPath = payloadDirectory + "/cni-plugins/flannel.exe"
	// HostLocalCNIPluginPath is the path of the host-local CNI plugin binary. The container image should already have
	// this binary mounted
	HostLocalCNIPlugin = payloadDirectory + "/cni-plugins/host-local.exe"
	// WinBridgeCNIPluginPath is the path of the win-bridge CNI plugin binary. The container image should already have
	// this binary mounted
	WinBridgeCNIPlugin = payloadDirectory + "/cni-plugins/win-bridge.exe"
	// WinOverlayCNIPluginPath is the path of the win-overlay CNI Plugin binary. The container image should already have
	// this binary mounted
	WinOverlayCNIPlugin = payloadDirectory + "/cni-plugins/win-overlay.exe"
	// hybridOverlayName is the name of the hybrid overlay executable
	HybridOverlayName = "hybrid-overlay-node.exe"
	// HybridOverlayPath contains the path of the hybrid overlay binary. The container image should already have this
	// binary mounted
	HybridOverlayPath = payloadDirectory + HybridOverlayName
)
