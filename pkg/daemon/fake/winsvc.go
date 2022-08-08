//go:build windows

package fake

import (
	"github.com/pkg/errors"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
)

type FakeService struct {
	name        string
	config      mgr.Config
	status      svc.Status
	serviceList *fakeServiceList
}

func (f *FakeService) Close() error {
	return nil
}

func (f *FakeService) Start(_ ...string) error {
	if f.status.State == svc.Running {
		return errors.New("service already running")
	}
	// each of the service's dependencies must be started before the service is started
	for _, dependency := range f.config.Dependencies {
		dependencyService, present := f.serviceList.read(dependency)
		if !present {
			return errors.New("dependent service doesnt exist")
		}
		// Windows will attempt to start the service only if it is not already running
		if err := winsvc.EnsureServiceState(dependencyService, svc.Running); err != nil {
			return err
		}
	}
	f.status.State = svc.Running
	return nil
}

func (f *FakeService) Config() (mgr.Config, error) {
	return f.config, nil
}

func (f *FakeService) Control(cmd svc.Cmd) (svc.Status, error) {
	switch cmd {
	case svc.Stop:
		if f.status.State == svc.Stopped {
			return svc.Status{}, errors.New("service already stopped")
		}
		// Windows has a hard time stopping services that other services are dependent on. To most safely model this
		// functionality it is better to make it so that our mock manager is completely unable to stop services in that
		// scenario.
		existingServices := f.serviceList.listServiceNames()
		for _, serviceName := range existingServices {
			service, present := f.serviceList.read(serviceName)
			if !present {
				return svc.Status{}, errors.New("unable to open service " + serviceName)
			}
			config, err := service.Config()
			if err != nil {
				return svc.Status{}, errors.Wrapf(err, "error getting %s service config", serviceName)
			}
			for _, dependency := range config.Dependencies {
				// Found a service that has this one as a dependency, ensure it is not running
				if dependency == f.name {
					status, err := service.Query()
					if err != nil {
						return svc.Status{}, errors.Wrapf(err, "error querying %s service status", serviceName)
					}
					if status.State != svc.Stopped {
						return svc.Status{}, errors.New("cannot stop service as other service " + serviceName +
							" is dependent on it")
					}
				}
			}
		}
		f.status.State = svc.Stopped
	}
	return f.status, nil
}

func (f *FakeService) Query() (svc.Status, error) {
	return f.status, nil
}

func (f *FakeService) UpdateConfig(config mgr.Config) error {
	f.config = config
	return nil
}

func NewFakeService(name string, config mgr.Config, status svc.Status) *FakeService {
	return &FakeService{
		name:   name,
		config: config,
		status: status,
	}
}
