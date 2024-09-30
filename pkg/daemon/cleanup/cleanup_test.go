//go:build windows

package cleanup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/fake"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

func TestRemoveServices(t *testing.T) {
	testIO := []struct {
		name                    string
		existingServices        map[string]*fake.FakeService
		configMapServices       []servicescm.Service
		expectedServices        map[string]struct{}
		removeAllTaggedServices bool
	}{
		{
			name:                    "No services",
			existingServices:        map[string]*fake.FakeService{},
			configMapServices:       []servicescm.Service{},
			expectedServices:        map[string]struct{}{},
			removeAllTaggedServices: false,
		},
		{
			name:                    "Upgrade scenario with no services",
			existingServices:        map[string]*fake.FakeService{},
			configMapServices:       []servicescm.Service{},
			expectedServices:        map[string]struct{}{},
			removeAllTaggedServices: false,
		},
		{
			name:             "ConfigMap managed service doesn't exist on node",
			existingServices: map[string]*fake.FakeService{},
			configMapServices: []servicescm.Service{
				{Name: "test1", Dependencies: nil, Priority: 0},
			},
			expectedServices:        map[string]struct{}{},
			removeAllTaggedServices: false,
		},
		{
			name: "Single service",
			existingServices: map[string]*fake.FakeService{
				"test1": newTestService("test1", []string{}, true),
			},
			configMapServices: []servicescm.Service{
				{Name: "test1", Dependencies: nil, Priority: 0},
			},
			expectedServices:        map[string]struct{}{},
			removeAllTaggedServices: false,
		},
		{
			name: "Multiple services",
			existingServices: map[string]*fake.FakeService{
				"test1": newTestService("test1", []string{}, true),
				"test2": newTestService("test2", []string{}, true),
				"test3": newTestService("test3", []string{}, false),
			},
			configMapServices: []servicescm.Service{
				{Name: "test1", Dependencies: nil, Priority: 0},
				{Name: "test2", Dependencies: nil, Priority: 1},
			},
			expectedServices:        map[string]struct{}{"test3": {}},
			removeAllTaggedServices: false,
		},
		{
			name: "Multiple services with dependencies",
			existingServices: map[string]*fake.FakeService{
				"test1": newTestService("test1", []string{}, true),
				"test2": newTestService("test2", []string{}, false),
				"test3": newTestService("test3", []string{}, true),
				"test4": newTestService("test4", []string{"test3"}, true),
				"test5": newTestService("test5", []string{"test1", "test3"}, true),
			},
			configMapServices: []servicescm.Service{
				{Name: "test1", Dependencies: nil, Priority: 0},
				{Name: "test3", Dependencies: nil, Priority: 0},
				{Name: "test4", Dependencies: []string{"test3"}, Priority: 1},
				{Name: "test5", Dependencies: []string{"test1", "test3"}, Priority: 2},
			},
			expectedServices:        map[string]struct{}{"test2": {}},
			removeAllTaggedServices: false,
		},
		{
			name: "best effort cleanup stops leftover service",
			existingServices: map[string]*fake.FakeService{
				"test1":    newTestService("test1", []string{}, true),
				"test2":    newTestService("test2", []string{}, false),
				"test3":    newTestService("test3", []string{}, true),
				"test4":    newTestService("test4", []string{"test3"}, true),
				"test5":    newTestService("test5", []string{"test1", "test3"}, true),
				"leftover": newTestService("leftover", []string{}, true),
			},
			configMapServices: []servicescm.Service{
				{Name: "test1", Dependencies: nil, Priority: 0},
				{Name: "test3", Dependencies: nil, Priority: 0},
				{Name: "test4", Dependencies: []string{"test3"}, Priority: 1},
				{Name: "test5", Dependencies: []string{"test1", "test3"}, Priority: 2},
			},
			expectedServices:        map[string]struct{}{"test2": {}},
			removeAllTaggedServices: true,
		},
	}

	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			winSvcMgr := fake.NewTestMgr(test.existingServices)
			err := removeServices(winSvcMgr, test.configMapServices, test.removeAllTaggedServices)
			require.NoError(t, err)
			allServices, err := winSvcMgr.GetServices()
			require.NoError(t, err)

			assert.Equal(t, test.expectedServices, allServices)
		})
	}
}

func TestMergeServices(t *testing.T) {
	testIO := []struct {
		name     string
		list1    []servicescm.Service
		list2    []servicescm.Service
		expected []servicescm.Service
	}{
		{
			name:     "both empty",
			list1:    []servicescm.Service{},
			list2:    []servicescm.Service{},
			expected: []servicescm.Service{},
		},
		{
			name:     "one empty",
			list1:    []servicescm.Service{{Name: "service1"}, {Name: "service2"}},
			list2:    []servicescm.Service{},
			expected: []servicescm.Service{{Name: "service1"}, {Name: "service2"}},
		},
		{
			name:  "different services",
			list1: []servicescm.Service{{Name: "service1"}, {Name: "service2", Dependencies: []string{"service1"}}},
			list2: []servicescm.Service{{Name: "service3"}, {Name: "service4", Dependencies: []string{"service3"}}},
			expected: []servicescm.Service{{Name: "service1"}, {Name: "service2", Dependencies: []string{"service1"}},
				{Name: "service3"}, {Name: "service4", Dependencies: []string{"service3"}}},
		},
		{
			name:     "overlapping services",
			list1:    []servicescm.Service{{Name: "service1"}, {Name: "service2"}},
			list2:    []servicescm.Service{{Name: "service2", Dependencies: []string{"service1"}}, {Name: "service3"}},
			expected: []servicescm.Service{{Name: "service1"}, {Name: "service2"}, {Name: "service3"}}},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			out := mergeServices(test.list1, test.list2)
			assert.ElementsMatch(t, out, test.expected)
		})
	}
}

func TestMerge(t *testing.T) {
	testIO := []struct {
		name     string
		list1    []string
		list2    []string
		expected []string
	}{
		{
			name:     "both empty",
			list1:    []string{},
			list2:    []string{},
			expected: []string{},
		},
		{
			name:     "one empty",
			list1:    []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
			list2:    []string{},
			expected: []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
		},
		{
			name:     "different lists",
			list1:    []string{"HTTP_PROXY"},
			list2:    []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
			expected: []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
		},
		{
			name:     "overlapping lists",
			list1:    []string{"HTTP_PROXY", "HTTPS_PROXY"},
			list2:    []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
			expected: []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"},
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			out := merge(test.list1, test.list2)
			assert.ElementsMatch(t, out, test.expected)
		})
	}
}

func newTestService(name string, dependencies []string, managed bool) *fake.FakeService {
	description := ""
	if managed {
		description = windows.ManagedTag + name
	}
	return fake.NewFakeService(
		name,
		mgr.Config{
			Description:  description,
			Dependencies: dependencies,
		},
		svc.Status{State: svc.Running},
	)
}
