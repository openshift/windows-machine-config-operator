package nodeutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindNode(t *testing.T) {
	ipNode := core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "ip-node",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Address: "127.0.0.1", Type: core.NodeInternalIP},
			},
		},
	}
	dnsNode := core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "dns-node",
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Address: "localhost", Type: core.NodeInternalDNS},
			},
		},
	}

	testCases := []struct {
		name        string
		address     string
		nodeList    *core.NodeList
		expectedOut *core.Node
	}{
		{
			name:    "empty node list",
			address: "localhost",
			nodeList: &core.NodeList{
				Items: []core.Node{},
			},
			expectedOut: nil,
		},
		{
			name:    "not found",
			address: "random.networkaddress",
			nodeList: &core.NodeList{
				Items: []core.Node{ipNode, dnsNode},
			},
			expectedOut: nil,
		},
		{
			name:    "simple happy path",
			address: "127.0.0.1",
			nodeList: &core.NodeList{
				Items: []core.Node{ipNode},
			},
			expectedOut: &ipNode,
		},
		{
			name:    "multiple node happy path",
			address: "localhost",
			nodeList: &core.NodeList{
				Items: []core.Node{ipNode, dnsNode},
			},
			expectedOut: &dnsNode,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			node := FindByAddress(test.address, test.nodeList)
			assert.Equal(t, test.expectedOut, node)
		})
	}

}
