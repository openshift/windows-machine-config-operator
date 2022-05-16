//go:build windows

package controller

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/winsvc"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
)

func TestResolveNodeVariables(t *testing.T) {
	testIO := []struct {
		name            string
		nodeName        string
		nodeAnnotations map[string]string
		nodeLabels      map[string]string
		service         servicescm.Service
		expected        map[string]string
		expectErr       bool
	}{
		{
			name:            "Node doesn't exist",
			nodeName:        "badnode",
			nodeAnnotations: nil,
			service:         servicescm.Service{},
			expectErr:       true,
		},
		{
			name:            "Desired annotation missing",
			nodeName:        "node",
			nodeAnnotations: map[string]string{"foo": "fah"},
			service: servicescm.Service{
				NodeVariablesInCommand: []servicescm.NodeCmdArg{
					{
						Name:               "replace",
						NodeObjectJsonPath: "{.metadata.annotations.desiredkey}",
					},
				},
			},
			expectErr: true,
		},
		{
			name:            "Desired annotation present",
			nodeName:        "node",
			nodeAnnotations: map[string]string{"desiredkey": "desiredvalue"},
			service: servicescm.Service{
				NodeVariablesInCommand: []servicescm.NodeCmdArg{
					{
						Name:               "replace",
						NodeObjectJsonPath: "{.metadata.annotations.desiredkey}",
					},
				},
			},
			expected:  map[string]string{"replace": "desiredvalue"},
			expectErr: false,
		},
		{
			name:            "Multiple fields found",
			nodeName:        "node",
			nodeAnnotations: map[string]string{"desiredkey": "desiredvalue"},
			nodeLabels:      map[string]string{"label": "labelvalue"},
			service: servicescm.Service{
				NodeVariablesInCommand: []servicescm.NodeCmdArg{
					{
						Name:               "replace",
						NodeObjectJsonPath: "{.metadata.annotations.desiredkey}",
					},
					{
						Name:               "label",
						NodeObjectJsonPath: "{.metadata.labels.label}",
					},
				},
			},
			expected:  map[string]string{"replace": "desiredvalue", "label": "labelvalue"},
			expectErr: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithObjects(&core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:        "node",
					Annotations: test.nodeAnnotations,
					Labels:      test.nodeLabels,
				},
			}).Build()
			c := NewServiceController(context.TODO(), fakeClient, winsvc.NewTestMgr(nil), test.nodeName)
			actual, err := c.resolveNodeVariables(test.service)
			if test.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.EqualValues(t, test.expected, actual)
		})
	}
}

func TestReconcileService(t *testing.T) {
	testIO := []struct {
		name                  string
		service               *winsvc.FakeService
		expectedService       servicescm.Service
		expectedServiceConfig mgr.Config
		expectErr             bool
	}{
		{
			name: "Service stub updated",
			service: winsvc.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:                         "fakeservice",
				Command:                      "fakeservice",
				NodeVariablesInCommand:       nil,
				PowershellVariablesInCommand: nil,
				Dependencies:                 nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
		{
			name: "Service binarypathname and description corrected",
			service: winsvc.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "bad",
					Description:    "bad",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:                         "fakeservice",
				Command:                      "fakeservice",
				NodeVariablesInCommand:       nil,
				PowershellVariablesInCommand: nil,
				Dependencies:                 nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
		{
			name: "Service command node variable substitution",
			service: winsvc.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "bad",
					Description:    "bad",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:    "fakeservice",
				Command: "fakeservice --node-name=NAME_REPLACE -v",
				NodeVariablesInCommand: []servicescm.NodeCmdArg{{
					Name:               "NAME_REPLACE",
					NodeObjectJsonPath: "{.metadata.name}",
				}},
				PowershellVariablesInCommand: nil,
				Dependencies:                 nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice --node-name=node -v",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithObjects(&core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "node",
				},
			}).Build()

			winSvcMgr := winsvc.NewTestMgr(map[string]*winsvc.FakeService{"fakeservice": test.service})
			c := NewServiceController(context.TODO(), fakeClient, winSvcMgr, "node")
			err := c.reconcileService(test.service, test.expectedService)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			actualConfig, err := test.service.Config()
			require.NoError(t, err)
			assert.Equal(t, test.expectedServiceConfig, actualConfig)
			serviceStatus, _ := test.service.Query()
			assert.Equal(t, svc.Running, serviceStatus.State)
		})
	}
}

func TestReconcile(t *testing.T) {
	testIO := []struct {
		name                         string
		existingServices             map[string]*winsvc.FakeService
		configMapServices            []servicescm.Service
		expectedServicesNameCmdPairs map[string]string
		expectErr                    bool
	}{
		{
			name:                         "No services",
			configMapServices:            []servicescm.Service{},
			expectedServicesNameCmdPairs: map[string]string{},
			expectErr:                    false,
		},
		{
			name: "Single service",
			configMapServices: []servicescm.Service{
				{
					Name:    "test1",
					Command: "test1 --node-name=NODENAME",
					NodeVariablesInCommand: []servicescm.NodeCmdArg{{
						Name:               "NODENAME",
						NodeObjectJsonPath: "{.metadata.name}",
					}},
					Dependencies: nil,
					Bootstrap:    true,
					Priority:     0,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 --node-name=node"},
			expectErr:                    false,
		},
		{
			name: "Single service that needs to be updated",
			existingServices: map[string]*winsvc.FakeService{"test1": winsvc.NewFakeService("test1",
				mgr.Config{BinaryPathName: "badvalue"}, svc.Status{State: svc.Running})},
			configMapServices: []servicescm.Service{
				{
					Name:         "test1",
					Command:      "test1 arg1",
					Dependencies: nil,
					Bootstrap:    true,
					Priority:     0,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 arg1"},
			expectErr:                    false,
		},
		{
			name: "Multiple services",
			configMapServices: []servicescm.Service{
				{
					Name:         "test1",
					Command:      "test1 arg1",
					Dependencies: nil,
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test2",
					Command:      "test2 arg1 arg2",
					Dependencies: nil,
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 arg1", "test2": "test2 arg1 arg2"},
			expectErr:                    false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			desiredVersion := "testversion"
			clusterObjs := []client.Object{
				// This is the node object that will be used in these test cases
				&core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name:        "node",
						Annotations: map[string]string{desiredVersionAnnotation: desiredVersion},
					},
				},
			}
			// This ConfigMap's name must match with the given Node object's desired-version annotation
			cm, err := servicescm.GenerateWithData(servicescm.NamePrefix+desiredVersion, "openshift-windows-machine-config-operator", &test.configMapServices, &[]servicescm.FileInfo{})
			require.NoError(t, err)
			clusterObjs = append(clusterObjs, cm)
			fakeClient := fake.NewClientBuilder().WithObjects(clusterObjs...).Build()

			winSvcMgr := winsvc.NewTestMgr(test.existingServices)
			c := NewServiceController(context.TODO(), fakeClient, winSvcMgr, "node")
			_, err = c.Reconcile(context.TODO(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			createdServices, err := getAllFakeServices(winSvcMgr)
			require.NoError(t, err)

			// Specifically testing that the name/command is as expected
			createdServiceNameCmdPairs := make(map[string]string)
			for name, createdService := range createdServices {
				config, err := createdService.Config()
				require.NoError(t, err)
				createdServiceNameCmdPairs[name] = config.BinaryPathName
			}
			assert.Equal(t, test.expectedServicesNameCmdPairs, createdServiceNameCmdPairs)
		})
	}
}

// getAllFakeServices accepts a mocked Windows service manager, and returns a map of copies of all existing Windows
// services
func getAllFakeServices(svcMgr winsvc.Mgr) (map[string]winsvc.FakeService, error) {
	svcs, err := svcMgr.ListServices()
	if err != nil {
		return nil, err
	}
	fakeServices := make(map[string]winsvc.FakeService)
	for _, winServiceName := range svcs {
		winService, err := svcMgr.OpenService(winServiceName)
		if err != nil {
			return nil, err
		}
		fakeService, ok := winService.(*winsvc.FakeService)
		if !ok {
			return nil, errors.New("this function should only be ran against a fake service manager")
		}
		fakeServices[winServiceName] = *fakeService
	}
	return fakeServices, nil
}

type fakeAddress struct {
	addr string
}

func (a *fakeAddress) String() string {
	return a.addr
}
func (a *fakeAddress) Network() string {
	return "fake"

}

func TestFindNodeByAddress(t *testing.T) {
	testIO := []struct {
		name      string
		nodes     *core.NodeList
		addrs     []net.Addr
		expected  *core.Node
		expectErr bool
	}{
		{
			name: "Node doesn't exist",
			nodes: &core.NodeList{Items: []core.Node{
				{
					ObjectMeta: meta.ObjectMeta{Name: "wrong-node"},
					Status: core.NodeStatus{
						Addresses: []core.NodeAddress{
							{Address: "192.168.9.9"},
						},
					}}}},
			addrs:     []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.10.10")}},
			expected:  nil,
			expectErr: true,
		},
		{
			name: "Local ip is a loopback ip",
			nodes: &core.NodeList{Items: []core.Node{
				{
					ObjectMeta: meta.ObjectMeta{Name: "wrong-node"},
					Status: core.NodeStatus{
						Addresses: []core.NodeAddress{
							{Address: "127.0.0.1"},
						},
					}}}},
			addrs:     []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1")}},
			expected:  nil,
			expectErr: true,
		},
		{
			name: "Node ip overlaps with a non-ipnet local address",
			nodes: &core.NodeList{Items: []core.Node{
				{
					ObjectMeta: meta.ObjectMeta{Name: "wrong-node"},
					Status: core.NodeStatus{
						Addresses: []core.NodeAddress{
							{Address: "192.168.10.10"},
						},
					}}}},
			addrs:     []net.Addr{&fakeAddress{addr: "192.168.10.10"}},
			expected:  nil,
			expectErr: true,
		},
		{
			name: "Node found",
			nodes: &core.NodeList{Items: []core.Node{
				{
					ObjectMeta: meta.ObjectMeta{Name: "wrong-node"},
					Status: core.NodeStatus{
						Addresses: []core.NodeAddress{
							{Address: "192.168.9.9"},
						},
					},
				},
				{
					ObjectMeta: meta.ObjectMeta{Name: "right-node"},
					Status: core.NodeStatus{
						Addresses: []core.NodeAddress{
							{Address: "192.168.10.10"},
						},
					},
				},
			}},
			addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.10.10")}},
			expected: &core.Node{
				ObjectMeta: meta.ObjectMeta{Name: "right-node"},
				Status: core.NodeStatus{
					Addresses: []core.NodeAddress{
						{Address: "192.168.10.10"},
					},
				},
			},
			expectErr: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			actual, err := findNodeByAddress(test.nodes, test.addrs)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expected, actual)
		})
	}
}
