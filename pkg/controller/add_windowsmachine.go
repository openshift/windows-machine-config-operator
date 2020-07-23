package controller

import (
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, windowsmachine.Add)
}
