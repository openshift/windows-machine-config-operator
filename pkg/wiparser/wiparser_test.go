package wiparser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
)

func TestParse(t *testing.T) {

	testCases := []struct {
		name        string
		input       map[string]string
		nodeList    *core.NodeList
		expectedOut []*instance.Info
		expectedErr bool
	}{
		{
			name:        "invalid username",
			input:       map[string]string{"localhost": "notusername=core"},
			nodeList:    &core.NodeList{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "invalid DNS address",
			input:       map[string]string{"notlocalhost": "username=core"},
			nodeList:    &core.NodeList{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "invalid username and DNS",
			input:       map[string]string{"invalid": "invalid"},
			nodeList:    &core.NodeList{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "valid ipv6 address",
			input:       map[string]string{"::1": "username=core"},
			nodeList:    &core.NodeList{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "valid dns address",
			input:       map[string]string{"localhost": "username=core"},
			nodeList:    &core.NodeList{},
			expectedOut: []*instance.Info{{Address: "localhost", IPv4Address: "127.0.0.1", Username: "core"}},
			expectedErr: false,
		},
		{
			name:        "valid ip address",
			input:       map[string]string{"127.0.0.1": "username=core"},
			nodeList:    &core.NodeList{},
			expectedOut: []*instance.Info{{Address: "127.0.0.1", IPv4Address: "127.0.0.1", Username: "core"}},
			expectedErr: false,
		},
		{
			name:     "valid dns and ip addresses with no nodes",
			input:    map[string]string{"localhost": "username=core", "127.0.0.1": "username=Admin"},
			nodeList: &core.NodeList{},
			expectedOut: []*instance.Info{
				{Address: "localhost", IPv4Address: "127.0.0.1", Username: "core"},
				{Address: "127.0.0.1", IPv4Address: "127.0.0.1", Username: "Admin"},
			},
			expectedErr: false,
		},
		{
			name:  "valid dns and ip addresses with unassociated nodes",
			input: map[string]string{"localhost": "username=core", "127.0.0.1": "username=Admin"},
			nodeList: &core.NodeList{
				Items: []core.Node{
					{
						ObjectMeta: meta.ObjectMeta{
							Name: "wrong-node",
						},
						Status: core.NodeStatus{
							Addresses: []core.NodeAddress{
								{Address: "111.1.1.1", Type: core.NodeInternalIP},
							},
						},
					},
				},
			},
			expectedOut: []*instance.Info{
				{Address: "127.0.0.1", IPv4Address: "127.0.0.1", Username: "Admin", Node: nil},
				{Address: "localhost", IPv4Address: "127.0.0.1", Username: "core", Node: nil},
			},
			expectedErr: false,
		},
		{
			name:  "valid dns and ip addresses with associated nodes",
			input: map[string]string{"localhost": "username=core", "127.0.0.2": "username=Admin"},
			nodeList: &core.NodeList{
				Items: []core.Node{
					{
						ObjectMeta: meta.ObjectMeta{
							Name: "ip-node",
						},
						Status: core.NodeStatus{
							Addresses: []core.NodeAddress{
								{Address: "127.0.0.2", Type: core.NodeInternalIP},
							},
						},
					},
					{
						ObjectMeta: meta.ObjectMeta{
							Name: "dns-node",
						},
						Status: core.NodeStatus{
							Addresses: []core.NodeAddress{
								{Address: "127.0.0.1", Type: core.NodeInternalIP},
							},
						},
					},
				},
			},
			expectedOut: []*instance.Info{
				{Address: "127.0.0.2", IPv4Address: "127.0.0.2", Username: "Admin",
					Node: &core.Node{ObjectMeta: meta.ObjectMeta{Name: "ip-node"},
						Status: core.NodeStatus{Addresses: []core.NodeAddress{{Address: "127.0.0.2",
							Type: core.NodeInternalIP}},
						}}},
				{Address: "localhost", IPv4Address: "127.0.0.1", Username: "core",
					Node: &core.Node{ObjectMeta: meta.ObjectMeta{Name: "dns-node"},
						Status: core.NodeStatus{Addresses: []core.NodeAddress{{Address: "127.0.0.1",
							Type: core.NodeInternalIP}},
						}}},
			},
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := Parse(test.input, test.nodeList)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}

func TestGetNodeUsername(t *testing.T) {
	testNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "test-node",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Address: "111.1.1.1", Type: core.NodeInternalIP},
			},
		},
	}

	testCases := []struct {
		name        string
		data        map[string]string
		node        *core.Node
		expectedOut string
		expectedErr bool
	}{
		{
			name:        "invalid node",
			data:        map[string]string{"localhost": "username=core"},
			node:        nil,
			expectedOut: "",
			expectedErr: true,
		},
		{
			name:        "empty map data",
			data:        map[string]string{},
			node:        testNode,
			expectedOut: "",
			expectedErr: true,
		},
		{
			name:        "bad map data",
			data:        map[string]string{"localhost": "core"},
			node:        testNode,
			expectedOut: "",
			expectedErr: true,
		},
		{
			name:        "bad map data left hand side",
			data:        map[string]string{"localhost": "notusername=core"},
			node:        testNode,
			expectedOut: "",
			expectedErr: true,
		},
		{
			name:        "node not in map data",
			data:        map[string]string{"localhost": "username=core"},
			node:        testNode,
			expectedOut: "",
			expectedErr: true,
		},
		{
			name:        "one entry in map data",
			data:        map[string]string{"111.1.1.1": "username=core"},
			node:        testNode,
			expectedOut: "core",
			expectedErr: false,
		},
		{
			name:        "multiple entries in map data",
			data:        map[string]string{"localhost": "username=core", "111.1.1.1": "username=Admin"},
			node:        testNode,
			expectedOut: "Admin",
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := GetNodeUsername(test.data, test.node)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedOut, out)
		})
	}
}
