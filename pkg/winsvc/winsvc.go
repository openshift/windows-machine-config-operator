//go:build windows

package winsvc

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type Mgr interface {
	CreateService(string, string, mgr.Config, ...string) (Service, error)
	ListServices() ([]string, error)
	OpenService(string) (Service, error)
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
func (t testMgr) CreateService(name, exepath string, config mgr.Config, args ...string) (Service, error) {
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

func (t testMgr) ListServices() ([]string, error) {
	return t.svcList.listServiceNames(), nil
}

func (t testMgr) OpenService(name string) (Service, error) {
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

func (f FakeService) Delete() error {
	return f.serviceList.remove(f.name)
}

func (f FakeService) Close() error {
	return nil
}

func (f FakeService) Start(_ ...string) error {
	f.status.State = svc.Running
	return nil
}

func (f FakeService) Config() (mgr.Config, error) {
	return f.config, nil
}

func (f FakeService) Control(cmd svc.Cmd) (svc.Status, error) {
	switch cmd {
	case svc.Stop:
		f.status.State = svc.Stopped
	}
	return f.status, nil
}

func (f FakeService) Query() (svc.Status, error) {
	return f.status, nil
}

func (f FakeService) UpdateConfig(config mgr.Config) error {
	f.config = config
	return nil
}

func NewMgr() (*mgr.Mgr, error) {
	return mgr.Connect()
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
