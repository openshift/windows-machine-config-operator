//go:build windows

package winsvc

import (
	"fmt"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type Mgr interface {
	CreateService(string, string, mgr.Config, ...string) (Service, error)
	ListServices() ([]string, error)
	OpenService(string) (Service, error)
}

// realManager is defined as a way for us to redefine the function signatures of mgr.Mgr, so that they can fulfill
// the Mgr interface. When used directly, functions like mgr.Mgr's CreateService() returns a *mgr.Service type. This
// causes issues fitting it to the Mgr interface, even though *mgr.Service implements the Service interface. By
// using the realManager wrapper functions, the underlying mgr.Mgr methods can be called, and then the *mgr.Service
// return values can be cast to the Service interface.
type realManager mgr.Mgr

func (m *realManager) CreateService(name, exepath string, config mgr.Config, args ...string) (Service, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	service, err := underlyingMgr.CreateService(name, exepath, config, args...)
	return Service(service), err
}
func (m *realManager) ListServices() ([]string, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.ListServices()
}
func (m *realManager) OpenService(name string) (Service, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.OpenService(name)
}

// fakeServiceList mocks out the state of all services on a Windows instance
type fakeServiceList struct {
	m    *sync.Mutex
	svcs map[string]Service
}

// write overwrites the given service to the svcs map
func (l *fakeServiceList) write(name string, svc Service) {
	l.m.Lock()
	defer l.m.Unlock()
	l.svcs[name] = svc
}

// read returns the entry with the given name, and a bool indicating if it exists or not
func (l *fakeServiceList) read(name string) (Service, bool) {
	l.m.Lock()
	defer l.m.Unlock()
	service, exists := l.svcs[name]
	return service, exists
}

// listServiceNames returns a slice of all service names
func (l *fakeServiceList) listServiceNames() []string {
	l.m.Lock()
	defer l.m.Unlock()
	var names []string
	for svcName := range l.svcs {
		names = append(names, svcName)
	}
	return names
}

// remove deletes the entry with the given name, throwing an error if it doesn't exist
func (l *fakeServiceList) remove(name string) error {
	l.m.Lock()
	defer l.m.Unlock()
	_, exists := l.svcs[name]
	if !exists {
		return errors.New("service does not exist")
	}
	delete(l.svcs, name)
	return nil
}

func newFakeServiceList() *fakeServiceList {
	return &fakeServiceList{
		m:    &sync.Mutex{},
		svcs: make(map[string]Service),
	}
}

type testMgr struct {
	svcList *fakeServiceList
}

// CreateService installs new service name on the system.
// The service will be executed by running exepath binary.
// Use config c to specify service parameters.
// Any args will be passed as command-line arguments when
// the service is started; these arguments are distinct from
// the arguments passed to Service.Start or via the "Start
// parameters" field in the service's Properties dialog box.
func (t *testMgr) CreateService(name, exepath string, config mgr.Config, args ...string) (Service, error) {
	// Throw an error if the service already exists
	if _, ok := t.svcList.read(name); ok {
		return nil, errors.New("service already exists")
	}
	config.BinaryPathName = exepath
	service := FakeService{
		name:   name,
		config: config,
		status: svc.Status{
			State: svc.Stopped,
		},
		serviceList: t.svcList,
	}
	t.svcList.write(name, &service)
	return &service, nil
}

func (t *testMgr) ListServices() ([]string, error) {
	return t.svcList.listServiceNames(), nil
}

func (t *testMgr) OpenService(name string) (Service, error) {
	service, exists := t.svcList.read(name)
	if !exists {
		return nil, fmt.Errorf("service does not exist")
	}
	return service, nil
}

type Service interface {
	Delete() error
	Close() error
	Start(...string) error
	Config() (mgr.Config, error)
	Control(svc.Cmd) (svc.Status, error)
	Query() (svc.Status, error)
	UpdateConfig(mgr.Config) error
}

type FakeService struct {
	name        string
	config      mgr.Config
	status      svc.Status
	serviceList *fakeServiceList
}

func (f *FakeService) Delete() error {
	return f.serviceList.remove(f.name)
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
		if err := EnsureServiceState(dependencyService, svc.Running); err != nil {
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

func NewMgr() (Mgr, error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, err
	}

	return (*realManager)(manager), nil
}

func NewTestMgr(existingServices map[string]*FakeService) *testMgr {
	testMgr := &testMgr{newFakeServiceList()}
	if existingServices != nil {
		for name, svc := range existingServices {
			svc.serviceList = testMgr.svcList
			testMgr.svcList.svcs[name] = svc
		}
	}
	return testMgr
}

// EnsureServiceState ensures the service is in the given state
func EnsureServiceState(service Service, state svc.State) error {
	status, err := service.Query()
	if err != nil {
		return errors.Wrap(err, "error querying service state")
	}
	if status.State == state {
		return nil
	}
	switch state {
	case svc.Running:
		return service.Start()
	case svc.Stopped:
		_, err = service.Control(svc.Stop)
		return err
	default:
		return errors.New("unexpected state request")
	}
}
