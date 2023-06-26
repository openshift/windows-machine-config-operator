//go:build windows

package controller

import (
	"context"
	"fmt"
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
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/fake"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/manager"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
)

var wmcoNamespace = "openshift-windows-machine-config-operator"

type fakePSCmdRunner struct {
	results map[string]string
}

func (f *fakePSCmdRunner) Run(cmd string) (string, error) {
	result, present := f.results[cmd]
	if !present {
		return "", fmt.Errorf("bad command")
	}
	return result, nil
}

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
			c, err := NewServiceController(context.TODO(), test.nodeName, wmcoNamespace, Options{
				Client: clientfake.NewClientBuilder().WithObjects(&core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name:        "node",
						Annotations: test.nodeAnnotations,
						Labels:      test.nodeLabels,
					},
				}).Build(),
				Mgr:       fake.NewTestMgr(nil),
				cmdRunner: &fakePSCmdRunner{},
			})
			require.NoError(t, err)
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

func TestResolvePowershellVariables(t *testing.T) {
	testIO := []struct {
		name      string
		service   servicescm.Service
		expected  map[string]string
		expectErr bool
	}{
		{
			name:      "No Powershell variables to replace",
			service:   servicescm.Service{},
			expected:  map[string]string{},
			expectErr: false,
		},
		{
			name: "Resolve variable with unknown path",
			service: servicescm.Service{
				PowershellPreScripts: []servicescm.PowershellPreScript{{
					VariableName: "CMD_REPLACE",
					Path:         "invalid-script.ps1",
				}},
			},
			expectErr: true,
		},
		{
			name: "Resolve variable with known path",
			service: servicescm.Service{
				PowershellPreScripts: []servicescm.PowershellPreScript{{
					VariableName: "CMD_REPLACE",
					Path:         "c:\\k\\script.ps1",
				}},
			},
			expected:  map[string]string{"CMD_REPLACE": "127.0.0.1"},
			expectErr: false,
		},
		{
			name: "Empty variable name",
			service: servicescm.Service{
				PowershellPreScripts: []servicescm.PowershellPreScript{{
					VariableName: "",
					Path:         "c:\\k\\script.ps1",
				}},
			},
			expected:  map[string]string{},
			expectErr: false,
		},
		{
			name: "Multiple variable to resolve",
			service: servicescm.Service{
				PowershellPreScripts: []servicescm.PowershellPreScript{
					{
						VariableName: "CMD_REPLACE1",
						Path:         "c:\\k\\script.ps1",
					},
					{
						VariableName: "CMD_REPLACE2",
						Path:         "c:\\k\\test.ps1",
					},
				},
			},
			expected:  map[string]string{"CMD_REPLACE1": "127.0.0.1", "CMD_REPLACE2": "test-output"},
			expectErr: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			c, err := NewServiceController(context.TODO(), "", wmcoNamespace, Options{
				Client: clientfake.NewClientBuilder().Build(),
				Mgr:    fake.NewTestMgr(nil),
				cmdRunner: &fakePSCmdRunner{
					map[string]string{
						"c:\\k\\script.ps1": "127.0.0.1",
						"c:\\k\\test.ps1":   "test-output",
					},
				},
			})
			require.NoError(t, err)
			actual, err := c.resolvePowershellVariables(test.service)
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
		service               *fake.FakeService
		expectedService       servicescm.Service
		expectedServiceConfig mgr.Config
		expectErr             bool
	}{
		{
			name: "Service stub updated",
			service: fake.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:                   "fakeservice",
				Command:                "fakeservice",
				NodeVariablesInCommand: nil,
				PowershellPreScripts:   nil,
				Dependencies:           nil,
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
			service: fake.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "bad",
					Description:    "bad",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:                   "fakeservice",
				Command:                "fakeservice",
				NodeVariablesInCommand: nil,
				PowershellPreScripts:   nil,
				Dependencies:           nil,
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
			service: fake.NewFakeService(
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
				PowershellPreScripts: nil,
				Dependencies:         nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice --node-name=node -v",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
		{
			name: "Service command powershell variable substitution",
			service: fake.NewFakeService(
				"fakeservice",
				mgr.Config{
					BinaryPathName: "bad",
					Description:    "bad",
				},
				svc.Status{
					State: svc.Running,
				}),
			expectedService: servicescm.Service{
				Name:                   "fakeservice",
				Command:                "fakeservice --ip_example=CMD_REPLACE -v",
				NodeVariablesInCommand: nil,
				PowershellPreScripts: []servicescm.PowershellPreScript{{
					VariableName: "CMD_REPLACE",
					Path:         "c:\\k\\script.ps1",
				}},
				Dependencies: nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice --ip_example=127.0.0.1 -v",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
		{
			name: "Service command node and powershell variable substitution",
			service: fake.NewFakeService(
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
				Command: "fakeservice --node-name=NAME_REPLACE --ip_example=CMD_REPLACE -v",
				NodeVariablesInCommand: []servicescm.NodeCmdArg{{
					Name:               "NAME_REPLACE",
					NodeObjectJsonPath: "{.metadata.name}",
				}},
				PowershellPreScripts: []servicescm.PowershellPreScript{{
					VariableName: "CMD_REPLACE",
					Path:         "c:\\k\\script.ps1",
				}},
				Dependencies: nil,
			},
			expectedServiceConfig: mgr.Config{
				BinaryPathName: "fakeservice --node-name=node --ip_example=127.0.0.1 -v",
				Dependencies:   nil,
				Description:    "OpenShift managed fakeservice",
			},
			expectErr: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			c, err := NewServiceController(context.TODO(), "node", wmcoNamespace, Options{
				Client: clientfake.NewClientBuilder().WithObjects(&core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "node",
					},
				}).Build(),
				Mgr: fake.NewTestMgr(map[string]*fake.FakeService{"fakeservice": test.service}),
				cmdRunner: &fakePSCmdRunner{
					map[string]string{
						"c:\\k\\script.ps1": "127.0.0.1",
					},
				},
			})
			require.NoError(t, err)
			err = c.reconcileService(test.service, test.expectedService)
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

func TestBootstrap(t *testing.T) {
	testIO := []struct {
		name                         string
		configMapServices            []servicescm.Service
		expectedServicesNameCmdPairs map[string]string
	}{
		{
			name:                         "No services",
			configMapServices:            []servicescm.Service{},
			expectedServicesNameCmdPairs: map[string]string{},
		},
		{
			name: "Single bootstrap service",
			configMapServices: []servicescm.Service{
				{
					Name:         "test1",
					Command:      "test1 --var=TEST",
					Dependencies: nil,
					Bootstrap:    true,
					Priority:     0,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 --var=TEST"},
		},
		{
			name: "No bootstrap services",
			configMapServices: []servicescm.Service{
				{
					Name:         "test1",
					Command:      "test1 arg1",
					Dependencies: nil,
					Bootstrap:    false,
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
			expectedServicesNameCmdPairs: map[string]string{},
		},
		{
			name: "Multiple mixed services",
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
					Command:      "test2",
					Dependencies: nil,
					Bootstrap:    true,
					Priority:     1,
				},
				{
					Name:         "test3",
					Command:      "test3 arg1 arg2",
					Dependencies: nil,
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 arg1", "test2": "test2"},
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			desiredVersion := "testversion"
			cm, err := servicescm.Generate(servicescm.NamePrefix+desiredVersion,
				wmcoNamespace, &servicescm.Data{Services: test.configMapServices,
					Files: []servicescm.FileInfo{}})
			require.NoError(t, err)
			clusterObjs := []client.Object{cm}

			winSvcMgr := fake.NewTestMgr(make(map[string]*fake.FakeService))
			sc, err := NewServiceController(context.TODO(), "", wmcoNamespace, Options{
				Client:    clientfake.NewClientBuilder().WithObjects(clusterObjs...).Build(),
				Mgr:       winSvcMgr,
				cmdRunner: &fakePSCmdRunner{},
			})
			require.NoError(t, err)

			err = sc.Bootstrap(desiredVersion)
			assert.NoError(t, err)

			createdServices, err := getAllFakeServices(winSvcMgr)
			require.NoError(t, err)

			testServicesCreatedAsExpected(t, createdServices, test.expectedServicesNameCmdPairs)
		})
	}
}

func TestReconcile(t *testing.T) {
	testIO := []struct {
		name                         string
		existingServices             map[string]*fake.FakeService
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
					Bootstrap:    false,
					Priority:     0,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 --node-name=node"},
			expectErr:                    false,
		},
		{
			name: "Single service that needs to be updated",
			existingServices: map[string]*fake.FakeService{"test1": fake.NewFakeService("test1",
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
			name: "Single service to be updated, with a running dependency",
			existingServices: map[string]*fake.FakeService{
				"test1": fake.NewFakeService("test1",
					mgr.Config{BinaryPathName: "badvalue"}, svc.Status{State: svc.Running}),
				"test2": fake.NewFakeService("test2",
					mgr.Config{BinaryPathName: "test2 arg1 arg2", Dependencies: []string{"test1"}},
					svc.Status{State: svc.Running}),
			},
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
					Dependencies: []string{"test1"},
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 arg1", "test2": "test2 arg1 arg2"},
			expectErr:                    false,
		},
		{
			name: "Single service to be updated, with a dependency chain",
			existingServices: map[string]*fake.FakeService{
				"test1": fake.NewFakeService("test1",
					mgr.Config{BinaryPathName: "badvalue"}, svc.Status{State: svc.Running}),
				"test2": fake.NewFakeService("test2",
					mgr.Config{BinaryPathName: "test2 arg1 arg2", Dependencies: []string{"test1"}},
					svc.Status{State: svc.Running}),
				"test3": fake.NewFakeService("test3",
					mgr.Config{BinaryPathName: "test3 arg1 arg2", Dependencies: []string{"test2"}},
					svc.Status{State: svc.Running}),
			},
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
					Dependencies: []string{"test1"},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "test3",
					Command:      "test3 arg1 arg2",
					Dependencies: []string{"test2"},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedServicesNameCmdPairs: map[string]string{"test1": "test1 arg1", "test2": "test2 arg1 arg2",
				"test3": "test3 arg1 arg2"},
			expectErr: false,
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
			// This ConfigMap's name must match with the given Node object's desired-version annotation
			cm, err := servicescm.Generate(servicescm.NamePrefix+desiredVersion,
				wmcoNamespace, &servicescm.Data{Services: test.configMapServices,
					Files: []servicescm.FileInfo{}, EnvironmentVars: nil})
			require.NoError(t, err)
			clusterObjs := []client.Object{
				// This is the node object that will be used in these test cases
				&core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "node",
						Annotations: map[string]string{
							metadata.DesiredVersionAnnotation: desiredVersion,
						},
					},
					Status: core.NodeStatus{
						Conditions: []core.NodeCondition{
							{
								Type:   core.NodeReady,
								Status: core.ConditionTrue,
							},
						},
					},
				},
				cm,
			}

			winSvcMgr := fake.NewTestMgr(test.existingServices)
			c, err := NewServiceController(context.TODO(), "node", wmcoNamespace, Options{
				Client:    clientfake.NewClientBuilder().WithObjects(clusterObjs...).Build(),
				Mgr:       winSvcMgr,
				cmdRunner: &fakePSCmdRunner{},
			})
			_, err = c.Reconcile(context.TODO(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "node"}})
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			createdServices, err := getAllFakeServices(winSvcMgr)
			require.NoError(t, err)

			testServicesCreatedAsExpected(t, createdServices, test.expectedServicesNameCmdPairs)
		})
	}
}

// testServicesCreatedAsExpected tests that the created services are running and configured as expected
func testServicesCreatedAsExpected(t *testing.T, createdServices map[string]fake.FakeService,
	expectedServicesNameCmdPairs map[string]string) {
	createdServiceNameCmdPairs := make(map[string]string)
	// Ensure each created service is running
	for name, createdService := range createdServices {
		serviceStatus, _ := createdService.Query()
		assert.Equal(t, svc.Running, serviceStatus.State)

		config, err := createdService.Config()
		require.NoError(t, err)
		createdServiceNameCmdPairs[name] = config.BinaryPathName
	}
	// Also ensure the correct number of expected services are created, all configured as intended
	assert.Equal(t, expectedServicesNameCmdPairs, createdServiceNameCmdPairs)
}

// getAllFakeServices accepts a mocked Windows service manager, and returns a map of copies of all existing Windows
// services
func getAllFakeServices(svcMgr manager.Manager) (map[string]fake.FakeService, error) {
	svcs, err := svcMgr.GetServices()
	if err != nil {
		return nil, err
	}
	fakeServices := make(map[string]fake.FakeService)
	for winServiceName := range svcs {
		winService, err := svcMgr.OpenService(winServiceName)
		if err != nil {
			return nil, err
		}
		fakeService, ok := winService.(*fake.FakeService)
		if !ok {
			return nil, fmt.Errorf("this function should only be ran against a fake service manager")
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

func TestSlicesEquivalent(t *testing.T) {
	testIO := []struct {
		name     string
		slice1   []string
		slice2   []string
		expected bool
	}{
		{
			name:     "empty slices",
			slice1:   []string{},
			slice2:   []string{},
			expected: true,
		},
		{
			name:     "nil slices",
			slice1:   nil,
			slice2:   nil,
			expected: true,
		},
		{
			name:     "one empty and one nil slice",
			slice1:   []string{},
			slice2:   nil,
			expected: true,
		},
		{
			name:     "same contents",
			slice1:   []string{"foo"},
			slice2:   []string{"foo"},
			expected: true,
		},
		{
			name:     "different contents",
			slice1:   []string{"foo"},
			slice2:   nil,
			expected: false,
		},
	}
	for _, test := range testIO {
		t.Run(test.name, func(t *testing.T) {
			actual := slicesEquivalent(test.slice1, test.slice2)
			assert.Equal(t, test.expected, actual)
		})
	}

}
