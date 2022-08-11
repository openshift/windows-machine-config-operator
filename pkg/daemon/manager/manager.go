//go:build windows

package manager

import (
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
)

type Manager interface {
	// CreateService creates a Windows service with the given configuration parameters
	CreateService(string, string, mgr.Config, ...string) (winsvc.Service, error)
	// ListServices enumerates all the Windows services that exist on an instance
	ListServices() ([]string, error)
	// OpenService gets the Windows service of the given name if it exists, by which it can be queried or controlled
	OpenService(string) (winsvc.Service, error)
	// CloseService closes access to the handle of the given service
	CloseService(string) error
	// DeleteService marks a Windows service of the given name for deletion, or an error if it does not exist
	DeleteService(string) error
}

// manager is defined as a way for us to redefine the function signatures of mgr.Mgr, so that they can fulfill
// the Mgr interface. When used directly, functions like mgr.Mgr's CreateService() returns a *mgr.Service type. This
// causes issues fitting it to the Mgr interface, even though *mgr.Service implements the Service interface. By
// using the manager wrapper functions, the underlying mgr.Mgr methods can be called, and then the *mgr.Service
// return values can be cast to the Service interface.
type manager mgr.Mgr

func (m *manager) CreateService(name, exepath string, config mgr.Config, args ...string) (winsvc.Service, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	service, err := underlyingMgr.CreateService(name, exepath, config, args...)
	return winsvc.Service(service), err
}

func (m *manager) ListServices() ([]string, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.ListServices()
}

func (m *manager) OpenService(name string) (winsvc.Service, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.OpenService(name)
}

func (m *manager) CloseService(name string) error {
	underlyingMgr := (*mgr.Mgr)(m)
	winSvc, err := underlyingMgr.OpenService(name)
	if err != nil {
		return err
	}
	return winSvc.Close()
}

func (m *manager) DeleteService(name string) error {
	underlyingMgr := (*mgr.Mgr)(m)
	winSvc, err := underlyingMgr.OpenService(name)
	if err != nil {
		return err
	}
	return winSvc.Delete()
}

func New() (Manager, error) {
	newMgr, err := mgr.Connect()
	if err != nil {
		return nil, err
	}

	return (*manager)(newMgr), nil
}
