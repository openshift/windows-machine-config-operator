package windows

// service implements the service interface and is specific to the kube-proxy service
type service struct {
	// binaryPath is the path to the binary to be ran as a service
	binaryPath string
	// name is the name of the service
	name string
	// args is the arguments that the binary will be ran with
	args string
}

func newService(binaryPath, name, args string) (*service, error) {
	return &service{
		binaryPath: binaryPath,
		name:       name,
		args:       args,
	}, nil
}

// Name returns the name of the service
func (s *service) Name() string {
	return s.name
}

// Args returns the arguments that the service will run with
func (s *service) Args() string {
	return s.args
}

// BinaryPath returns the path of the binary that service the service will run
func (s *service) BinaryPath() string {
	return s.binaryPath
}
