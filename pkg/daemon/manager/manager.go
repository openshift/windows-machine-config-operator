//go:build windows

package manager

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

type Manager interface {
	// CreateService creates a Windows service with the given configuration parameters
	CreateService(string, string, mgr.Config, ...string) (winsvc.Service, error)
	// GetServices returns a map of all the Windows services that exist on an instance.
	// The keys are service names and values are empty structs, used as 0 byte placeholders.
	GetServices() (map[string]struct{}, error)
	// OpenService gets the Windows service of the given name if it exists, by which it can be queried or controlled
	OpenService(string) (winsvc.Service, error)
	// DeleteService marks a Windows service of the given name for deletion. No-op if the service already doesn't exist
	DeleteService(string) error
	// EnsureServiceState ensures the service is in the given state
	EnsureServiceState(winsvc.Service, svc.State) error
	// Disconnect closes connection to the service manager
	Disconnect() error
}

// enumServiceStatus implements the ENUM_SERVICE_STATUS type as defined in the Windows API
type enumServiceStatus struct {
	ServiceName   *uint16
	DisplayName   *uint16
	ServiceStatus windows.SERVICE_STATUS
}

// enumDependentServicesW is a handle to the EnumDependentServicesW syscall
// https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-enumdependentservicesw
// This is global to prevent having to load the dll into memory and search for the API call every time it is used
var enumDependentServicesW = windows.NewLazySystemDLL("Advapi32.dll").NewProc("EnumDependentServicesW")

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

func (m *manager) GetServices() (map[string]struct{}, error) {
	// The most reliable way to determine if a service exists or not is to do a 'list' API call. It is possible to
	// remove this call, and parse the error messages of a service 'open' API call, but I find that relying on human
	// readable errors could cause issues when providing compatibility across different versions of Windows.
	manager := (*mgr.Mgr)(m)
	svcList, err := manager.ListServices()
	if err != nil {
		return nil, err
	}
	svcs := make(map[string]struct{})
	for _, service := range svcList {
		svcs[service] = struct{}{}
	}
	return svcs, nil
}

func (m *manager) OpenService(name string) (winsvc.Service, error) {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.OpenService(name)
}

func (m *manager) DeleteService(name string) error {
	manager := (*mgr.Mgr)(m)
	service, err := manager.OpenService(name)
	if err != nil {
		// Nothing to do if it already does not exist
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return fmt.Errorf("failed to open service %q: %w", name, err)
	}
	defer service.Close()
	// Ensure service is stopped before deleting
	if err = m.EnsureServiceState(service, svc.Stopped); err != nil {
		return fmt.Errorf("failed to stop service %q: %w", name, err)
	}
	if err = service.Delete(); err != nil {
		return fmt.Errorf("failed to delete service %q: %w", name, err)
	}
	return nil
}

func (m *manager) EnsureServiceState(service winsvc.Service, state svc.State) error {
	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("error querying service state: %w", err)
	}
	if status.State == state {
		return nil
	}
	switch state {
	case svc.Running:
		err = service.Start()
		if err != nil {
			return err
		}
	case svc.Stopped:
		// Before we can stop this service, we need to make sure all services dependent on this service are stopped
		// The service must be cast to the actual type so we can get its handle
		winSvc, ok := service.(*mgr.Service)
		if !ok {
			return fmt.Errorf("service is not correct type")
		}
		dependentServices, err := m.listDependentServices(winSvc.Handle)
		if err != nil {
			return fmt.Errorf("error finding dependent services: %w", err)
		}
		for _, dependentServiceName := range dependentServices {
			dependentSvc, err := m.OpenService(dependentServiceName)
			if err != nil {
				return fmt.Errorf("error opening dependent service %s: %w", dependentServiceName, err)
			}
			defer dependentSvc.Close()
			err = m.EnsureServiceState(dependentSvc, svc.Stopped)
			if err != nil {
				return fmt.Errorf("unable to stop dependent service %s: %w", dependentServiceName, err)
			}
		}
		if err := m.stopServiceAndProcess(winSvc); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unexpected state request: %d", state)
	}
	// Wait for the state change to actually take place
	return winsvc.WaitForState(service, state)
}

// Stop the service, and wait for the process associated with the service to stop
func (m *manager) stopServiceAndProcess(winSvc *mgr.Service) error {
	status, err := winSvc.Query()
	if err != nil {
		return fmt.Errorf("error querying service: %w", err)
	}
	var pHandle windows.Handle
	// A value of 0 indicates that no process is running
	if status.ProcessId != 0 {
		pHandle, err = windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false,
			status.ProcessId)
		if err != nil {
			return fmt.Errorf("unable to open service's associated process: %w", err)
		}
		defer windows.CloseHandle(pHandle)
	}
	_, err = winSvc.Control(svc.Stop)
	if err != nil {
		return err
	}
	if status.ProcessId != 0 {
		if err = waitForProcessToStop(pHandle); err != nil {
			// Terminate the process if it does not exit on its own
			if err = windows.TerminateProcess(pHandle, uint32(1)); err != nil {
				return fmt.Errorf("error terminating stalled process: %w", err)
			}
		}
	}
	return nil
}

// listDependentServices returns a list of names of all services dependent on the given service
func (m *manager) listDependentServices(serviceHandle windows.Handle) ([]string, error) {
	// Borrowing the main steps done here from the golang windows/mgr library's ListServices() function, as the
	// EnumServicesStatusEx syscall has a very similar way of being called.
	// https://cs.opensource.google/go/x/sys/+/refs/tags/v0.1.0:windows/svc/mgr/mgr.go;l=176
	var serviceBuffer []byte
	var bytesNeeded, returnedServiceCount uint32

	// The documentation for this syscall says it should be ran at least twice. First to determine the size of the
	// buffer it will return, and then to actually capture the data with an allocated buffer. As the count of dependent
	// services can change in between calls, it may need to be ran more than twice.
	for {
		var p *byte
		if len(serviceBuffer) > 0 {
			p = &serviceBuffer[0]
		}
		// Returned error from `Call` will always be non-nil
		success, _, err := enumDependentServicesSyscall(serviceHandle, windows.SERVICE_STATE_ALL, p,
			uint32(len(serviceBuffer)), &bytesNeeded, &returnedServiceCount)
		if success != 0 {
			// a non-zero return value indicates the syscall completed successfully, and serviceBuffer has been filled
			// with the requested data.
			break
		}
		if err != windows.ERROR_MORE_DATA {
			return nil, fmt.Errorf("received unexpected error from enumDependentServicesSyscall: %w", err)
		}
		if bytesNeeded <= uint32(len(serviceBuffer)) {
			return nil, err
		}
		serviceBuffer = make([]byte, bytesNeeded)
	}
	// If no services are dependent on this service, return successfully
	if returnedServiceCount == 0 {
		return nil, nil
	}
	// create a slice based on the buffer that was returned to us, so that we can iterate through it
	var services []enumServiceStatus
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&services))
	hdr.Data = uintptr(unsafe.Pointer(&serviceBuffer[0]))
	hdr.Len = int(returnedServiceCount)
	hdr.Cap = int(returnedServiceCount)

	var dependencies []string
	for _, s := range services {
		dependencies = append(dependencies, windows.UTF16PtrToString(s.ServiceName))
	}
	return dependencies, nil
}

func (m *manager) Disconnect() error {
	underlyingMgr := (*mgr.Mgr)(m)
	return underlyingMgr.Disconnect()
}

func New() (Manager, error) {
	newMgr, err := mgr.Connect()
	if err != nil {
		return nil, err
	}

	return (*manager)(newMgr), nil
}

// enumDependentServicesSyscall is a wrapper around enumDependentServicesW.Call with the correct argument casting
// Refer to the API documentation for an explanation of the arguments:
// https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-enumdependentservicesw
func enumDependentServicesSyscall(hService windows.Handle, dwServiceState uint32, lpServices *byte, cbBufSize uint32,
	pcbBytesNeeded *uint32, lpServicesReturned *uint32) (uintptr, uintptr, error) {
	return enumDependentServicesW.Call(uintptr(hService), uintptr(dwServiceState), uintptr(unsafe.Pointer(lpServices)),
		uintptr(cbBufSize), uintptr(unsafe.Pointer(pcbBytesNeeded)), uintptr(unsafe.Pointer(lpServicesReturned)))
}

// waitForProcessToStop waits until the process has exited
func waitForProcessToStop(process windows.Handle) error {
	return wait.PollImmediate(retry.WindowsAPIInterval, retry.ResourceChangeTimeout, func() (done bool, err error) {
		var exitCode uint32
		if err := windows.GetExitCodeProcess(process, &exitCode); err != nil {
			// unexpected error, most likely related to permissions
			return false, fmt.Errorf("error getting process exit code: %w", err)
		}
		// STATUS_PENDING indicates the process has not exited, keep retrying.
		if exitCode == uint32(windows.STATUS_PENDING) {
			return false, nil
		}
		return true, nil
	})
}
