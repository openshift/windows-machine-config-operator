package wellknownlocations

const (
	// CloudCredentialsPath contains the path to the file where cloud credentials are stored. This would've been mounted
	// as a secret by user
	CloudCredentialsPath = "/etc/cloud/credentials"
	// PrivateKeyPath contains the path to the private key which is used in decrypting password in case of AWS
	// cloud provider. This would have been mounted as a secret by user
	// TODO: Jira story for validation: https://issues.redhat.com/browse/WINC-316
	PrivateKeyPath = "/etc/private-key/private-key.pem"
	// WmcbPath contains the path of the Windows Machine Config Bootstrapper binary. The container image should already
	// have this binary mounted
	WmcbPath = "/payload/wmcb.exe"
	// KubeletPath contains the path of the kubelet binary. The container image should already have this binary mounted
	KubeletPath = "/payload/kube-node/kubelet.exe"
	// IgnoreWgetPowerShellPath contains the path of the powershell script which allows wget to ignore certs. The
	// container image should already have this mounted
	IgnoreWgetPowerShellPath = "/payload/powershell/wget-ignore-cert.ps1"
)
