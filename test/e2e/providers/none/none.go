package none

import (
	"github.com/pkg/errors"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

// Provider is a provider struct for testing platform=none
type Provider struct {
	oc *clusterinfo.OpenShift
}

// New returns a Provider implementation for platform=none
func New(clientset *clusterinfo.OpenShift) (*Provider, error) {
	return &Provider{
		oc: clientset,
	}, nil
}

// GenerateMachineSet is not supported for platform=none and throws an exception
func (p *Provider) GenerateMachineSet(withWindowsLabel bool, replicas int32) (*mapi.MachineSet, bool, error) {
	// false boolean return false indicated that platform=none does not test WindowsServer2022
	return nil, false, errors.New("MachineSet generation not supported for platform=none")
}

// GetType returns the platform type for platform=none
func (p *Provider) GetType() config.PlatformType {
	return config.NonePlatformType
}
