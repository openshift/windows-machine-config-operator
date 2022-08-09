//go:build windows

package fake

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

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
		expectErr    bool
	}{
		{
			name:         "service exists",
			svcName:      "svc-one",
			existingSvcs: map[string]*FakeService{"svc-one": {name: "svc-one", config: mgr.Config{Description: "testsvc"}}},
			expectErr:    false,
		},
		{
			name:         "delete non-existant service",
			svcName:      "svc-one",
			existingSvcs: map[string]*FakeService{},
			expectErr:    true,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			testMgr := NewTestMgr(test.existingSvcs)
			err := testMgr.DeleteService(test.svcName)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			// Check that the service is no longer present in the service list
			list, err := testMgr.ListServices()
			require.NoError(t, err)
			assert.NotContains(t, list, test.svcName)
		})
	}
}
