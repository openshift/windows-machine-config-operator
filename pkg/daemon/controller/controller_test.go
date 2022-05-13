//go:build windows

package controller

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			c := NewServiceController(fakeClient, winsvc.NewTestMgr(nil), test.nodeName)
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
			c := NewServiceController(fakeClient, winSvcMgr, "node")
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

type fakeAddress struct {
	addr string
}

func (a *fakeAddress) String() string {
	return a.addr
}
func (a *fakeAddress) Network() string {
	return "fake"

}
func TestCurrentNode(t *testing.T) {
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
			actual, err := currentNode(test.nodes, test.addrs)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expected, actual)
		})
	}
}
