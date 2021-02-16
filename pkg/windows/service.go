package windows

import "github.com/pkg/errors"

// service struct contains the service information
type service struct {
	// binaryPath is the path to the binary to be ran as a service
	binaryPath string
	// name is the name of the service
	name string
	// args is the arguments that the binary will be ran with
	args string
}

// newService initializes and returns a pointer to the service struct
func newService(binaryPath, name, args string) (*service, error) {
	if binaryPath == "" || name == "" {
		return nil, errors.Errorf("can't instantiate a service with incomplete service parameters")
	}
	return &service{
		binaryPath: binaryPath,
		name:       name,
		args:       args,
	}, nil
}
