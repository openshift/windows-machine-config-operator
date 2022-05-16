//go:build windows

package winsvc

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// These test functions have been created to define the expected behavior of the structs mocking the Windows
// service API. The expected behavior should match up with the behavior seen when using the Windows API, and
// if differences are seen between the two, the mock implementations should be modified to correct the difference.

func TestCreateService(t *testing.T) {
	testIO := []struct {
		name         string
		svcName      string
		svcExepath   string
		svcConfig    mgr.Config
		existingSvcs map[string]*FakeService
		expectErr    bool
	}{
		{
			name:    "new service with no other services",
			svcName: "svc-one",
			svcConfig: mgr.Config{
				Description: "testsvc",
			},
			svcExepath:   "testpath",
			existingSvcs: nil,
			expectErr:    false,
		},
		{
			name:    "new service with different existing service",
			svcName: "svc-one",
			svcConfig: mgr.Config{
				Description: "testsvc",
			},
			svcExepath:   "testpath",
			existingSvcs: map[string]*FakeService{"svc-two": {}},
			expectErr:    false,
		},
		{
			name:    "existing service",
			svcName: "svc-one",
			svcConfig: mgr.Config{
				Description: "testsvc",
			},
			svcExepath:   "testpath",
			existingSvcs: map[string]*FakeService{"svc-one": {}},
			expectErr:    true,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			testMgr := NewTestMgr(test.existingSvcs)
			_, err := testMgr.CreateService(test.svcName, test.svcExepath, test.svcConfig)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			// ensure all existing keys are preserved
			for svcName := range test.existingSvcs {
				assert.Contains(t, testMgr.svcList.svcs, svcName)
			}
			// ensure new service has been added to list with correct values
			require.Contains(t, testMgr.svcList.svcs, test.svcName)
			newSvcInterface := testMgr.svcList.svcs[test.svcName]
			newSvc, ok := newSvcInterface.(*FakeService)
			require.True(t, ok, "cannot cast service to correct type")
			assert.Equal(t, test.svcName, newSvc.name)
			assert.Equal(t, test.svcExepath, newSvc.config.BinaryPathName)
			assert.Equal(t, test.svcConfig.Description, newSvc.config.Description)
			assert.Equal(t, svc.Stopped, newSvc.status.State)
		})
	}
}

func TestListServices(t *testing.T) {
	testIO := []struct {
		name         string
		existingSvcs map[string]*FakeService
		expected     []string
	}{
		{
			name:         "no services",
			existingSvcs: map[string]*FakeService{},
			expected:     []string{},
		},
		{
			name:         "some services",
			existingSvcs: map[string]*FakeService{"svc-one": {}, "svc-two": {}},
			expected:     []string{"svc-one", "svc-two"},
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			testMgr := NewTestMgr(test.existingSvcs)
			list, err := testMgr.ListServices()
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expected, list)
		})
	}
}

func TestOpenService(t *testing.T) {
	testIO := []struct {
		name         string
		svcName      string
		existingSvcs map[string]*FakeService
		expected     *FakeService
		expectErr    bool
	}{
		{
			name:         "existing service",
			svcName:      "svc-one",
			existingSvcs: map[string]*FakeService{"svc-one": {config: mgr.Config{Description: "testsvc"}}},
			expectErr:    false,
		},
		{
			name:         "nonexistent service",
			svcName:      "svc-two",
			existingSvcs: map[string]*FakeService{"svc-one": {config: mgr.Config{Description: "testsvc"}}},
			expectErr:    true,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			testMgr := NewTestMgr(test.existingSvcs)
			s, err := testMgr.OpenService(test.svcName)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.existingSvcs[test.svcName], s)
		})
	}
}

func TestDeleteService(t *testing.T) {
	testIO := []struct {
		name         string
		svcName      string
		existingSvcs map[string]*FakeService
	}{
		{
			name:         "service exists",
			svcName:      "svc-one",
			existingSvcs: map[string]*FakeService{"svc-one": {name: "svc-one", config: mgr.Config{Description: "testsvc"}}},
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			testMgr := NewTestMgr(test.existingSvcs)
			// First check that the service exists in the service list
			list, err := testMgr.ListServices()
			require.NoError(t, err)
			require.Contains(t, list, test.svcName)
			// Open the service to get a service handle, and then delete the service
			s, err := testMgr.OpenService(test.svcName)
			require.NoError(t, err)
			require.NoError(t, s.Delete())
			// Check that the service is no longer present in the service list
			list, err = testMgr.ListServices()
			require.NoError(t, err)
			assert.NotContains(t, list, test.svcName)
		})
	}
}

func TestStartService(t *testing.T) {
	svcName := "testsvc"
	testIO := []struct {
		name          string
		startingState map[string]*FakeService
		expectedState map[string]Service
		expectErr     bool
	}{
		{
			name:          "service not running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectedState: map[string]Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Running}}},
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Running}}},
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
		expectedState map[string]Service
		expectErr     bool
	}{
		{
			name:          "service not running",
			startingState: map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: svc.Stopped}}},
			expectedState: map[string]Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Stopped}}},
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{
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
			expectedState: map[string]Service{svcName: &FakeService{name: svcName, status: svc.Status{State: svc.Stopped}}},
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
func serviceListsEqual(m1, m2 map[string]Service) (bool, error) {
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

func TestEnsureService(t *testing.T) {
	testIO := []struct {
		name          string
		signal        svc.Cmd
		startingState svc.State
		expectedState svc.State
		expectErr     bool
	}{
		{
			name:          "stop a stopped service",
			startingState: svc.Stopped,
			expectedState: svc.Stopped,
			expectErr:     false,
		},
		{
			name:          "stop a running service",
			startingState: svc.Running,
			expectedState: svc.Stopped,
			expectErr:     false,
		},
		{
			name:          "start a stopped service",
			startingState: svc.Stopped,
			expectedState: svc.Running,
			expectErr:     false,
		},
		{
			name:          "stop a running service",
			startingState: svc.Running,
			expectedState: svc.Running,
			expectErr:     false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			svcName := "testsvc"
			svcs := map[string]*FakeService{svcName: {name: svcName, status: svc.Status{State: test.startingState}}}
			manager := NewTestMgr(svcs)
			service, err := manager.OpenService(svcName)
			require.NoError(t, err)
			err = EnsureServiceState(service, test.expectedState)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			newStatus, err := service.Query()
			require.NoError(t, err)
			assert.Equal(t, test.expectedState, newStatus.State)

		})
	}
}
