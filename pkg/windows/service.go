package windows

import "fmt"

type recoveryActionType string

const serviceRestart = recoveryActionType("restart")

type recoveryAction struct {
	// actionType is the action that will be performed by the Windows service manager after a program crash
	actionType recoveryActionType
	// delay is the amount of time in seconds to wait before performing the specified action
	delay int
}

// service struct contains the service information
type service struct {
	// binaryPath is the path to the binary to be ran as a service
	binaryPath string
	// name is the name of the service
	name string
	// args is the arguments that the binary will be ran with
	args string
	// dependencies is a list of the names of the services that this service is dependent on
	dependencies []string
	// recoveryActions is a list of recovery actions that the service manager will apply in case of program crash
	// these actions will be run in order, until the crash counter is reset
	recoveryActions []recoveryAction
	// recoveryPeriod is the amount of time in seconds with no failures after which the recoveryAction crash counter
	// resets
	recoveryPeriod int
}

// newService initializes and returns a pointer to the service struct. The dependencies, recoveryActions, and
// recoveryPeriod arguments are optional
func newService(binaryPath, name, args string, dependencies []string, recoveryActions []recoveryAction,
	recoveryPeriod int) (*service, error) {
	if binaryPath == "" || name == "" {
		return nil, fmt.Errorf("can't instantiate a service with incomplete service parameters")
	}
	return &service{
		binaryPath:      binaryPath,
		name:            name,
		args:            args,
		dependencies:    dependencies,
		recoveryActions: recoveryActions,
		recoveryPeriod:  recoveryPeriod,
	}, nil
}
