//go:build windows

package fake

import (
	"errors"
	"reflect"
	"testing"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// These test functions have been created to define the expected behavior of the structs mocking the Windows
// service API. The expected behavior should match up with the behavior seen when using the Windows API, and
// if differences are seen between the two, the mock implementations should be modified to correct the difference.

func TestStartService(t *testing.T) {
	svcName := "testsvc"
	testIO := []struct {
		name          string
		startingState map[string]*FakeService
		expectedState map[string]winsvc.Service
		expectErr     bool
	}{
		{
			name:          "service not running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectedState: map[string]winsvc.Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Running}}},
			expectErr:     false,
		},
		{
			name: "service not running and dependency running",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Running},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Running}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Running},
				},
			},
			expectErr: false,
		},
		{
			name: "service and dependency not running",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Stopped},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Running}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Running},
				},
			},
			expectErr: false,
		},
		{
			name: "service and dependency not running, dependency not startable",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"doesnt-exist"}},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2"}},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"doesnt-exist"}},
				},
			},
			expectErr: true,
		},
		{
			name: "service and dependency not running, service not startable",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2", "doesnt-exist"}},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Stopped},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{"service2", "doesnt-exist"}},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Running},
				},
			},
			expectErr: true,
		},
		{
			name:          "service running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Running}}},
			expectedState: map[string]winsvc.Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Running}}},
			expectErr:     true,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			manager := NewTestMgr(test.startingState)
			service, err := manager.OpenService(svcName)
			require.NoError(t, err)
			err = service.Start()
			if test.expectErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			listsEqual, err := serviceListsEqual(test.expectedState, manager.svcList.svcs)
			require.NoError(t, err)
			assert.True(t, listsEqual)
		})
	}
}

func TestStopService(t *testing.T) {
	svcName := "testsvc"
	testIO := []struct {
		name          string
		startingState map[string]*FakeService
		expectedState map[string]winsvc.Service
		expectErr     bool
	}{
		{
			name:          "service not running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectedState: map[string]winsvc.Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectErr:     true,
		},
		{
			name: "service running and dependent service not running",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Running},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{svcName}},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Stopped},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Stopped}, config: mgr.Config{Dependencies: []string{svcName}},
				},
			},
			expectErr: false,
		},
		{
			name: "service and dependent service running",
			startingState: map[string]*FakeService{
				svcName: {
					name: svcName, status: svc.Status{State: svc.Running},
				},
				"service2": {
					name: svcName, status: svc.Status{State: svc.Running}, config: mgr.Config{Dependencies: []string{svcName}},
				},
			},
			expectedState: map[string]winsvc.Service{
				svcName: &FakeService{
					name: svcName, status: svc.Status{State: svc.Running},
				},
				"service2": &FakeService{
					name: svcName, status: svc.Status{State: svc.Running}, config: mgr.Config{Dependencies: []string{svcName}},
				},
			},
			expectErr: true,
		},
		{
			name:          "service running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Running}}},
			expectedState: map[string]winsvc.Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectErr:     false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			manager := NewTestMgr(test.startingState)
			service, err := manager.OpenService(svcName)
			require.NoError(t, err)
			_, err = service.Control(svc.Stop)
			if test.expectErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			listsEqual, err := serviceListsEqual(test.expectedState, manager.svcList.svcs)
			require.NoError(t, err)
			assert.True(t, listsEqual)
		})
	}
}

// serviceListsEqual returns true if the two maps are equivalent
func serviceListsEqual(m1, m2 map[string]winsvc.Service) (bool, error) {
	if len(m1) != len(m2) {
		return false, nil
	}
	for m1ServiceName, m1Service := range m1 {
		m1FakeService, ok := m1Service.(*FakeService)
		if !ok {
			return false, errors.New("service not castable to *FakeService")
		}
		m2Service, present := m2[m1ServiceName]
		if !present {
			return false, nil
		}
		m2FakeService, ok := m2Service.(*FakeService)
		if !ok {
			return false, errors.New("service not castable to *FakeService")
		}
		// Differences in the serviceList pointers will cause DeepEqual() to return false, so make a copy of each
		// service and set the serviceList pointer to nil
		m1FakeServiceCopy := *m1FakeService
		m2FakeServiceCopy := *m2FakeService
		m1FakeServiceCopy.serviceList = nil
		m2FakeServiceCopy.serviceList = nil
		if !reflect.DeepEqual(m1FakeServiceCopy, m2FakeServiceCopy) {
			return false, nil
		}
	}
	return true, nil
}
