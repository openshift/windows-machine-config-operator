package windows

// service represents a Windows service
type service interface {
	Name() string
	BinaryPath() string
	Args() string
}

// kubeProxyService implements the service interface and is specific to the kube-proxy service
type kubeProxyService struct {
	// binaryPath is the path to the binary to be ran as a service
	binaryPath string
	// name is the name of the service
	name string
	// args is the arguments that the binary will be ran with
	args string
}

// newKubeProxyService returns a service interface with a kubeProxyService implementation
func newKubeProxyService(nodeName, hostSubnet, sourceVIP string) (service, error) {
	return &kubeProxyService{
		binaryPath: kubeProxyPath,
		name:       kubeProxyServiceName,
		args: "--windows-service --v=4 --proxy-mode=kernelspace --feature-gates=WinOverlay=true " +
			"--hostname-override=" + nodeName + " --kubeconfig=c:\\k\\kubeconfig " +
			"--cluster-cidr=" + hostSubnet + " --log-dir=" + kubeProxyLogDir + " --logtostderr=false " +
			"--network-name=OVNKubernetesHybridOverlayNetwork --source-vip=" + sourceVIP +
			" --enable-dsr=false",
	}, nil
}

// Name returns the name of the service
func (s *kubeProxyService) Name() string {
	return s.name
}

// Args returns the arguments that the service will run with
func (s *kubeProxyService) Args() string {
	return s.args
}

// BinaryPath returns the path of the binary that service the service will run
func (s *kubeProxyService) BinaryPath() string {
	return s.binaryPath
}
