package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsEndpointsValid(t *testing.T) {
	tests := []struct {
		name      string
		nodes     *v1.NodeList
		endpoints *v1.Endpoints
		want      bool
	}{
		{
			name: "EndpointAddresses match nodes count and name",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							TargetRef: &v1.ObjectReference{
								Kind: "Node",
								Name: "the-node-name",
							},
						},
					}},
				},
			},
			want: true,
		},
		{
			name: "EndpointAddresses match two nodes",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name-one"},
					},
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name-two"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{
						Addresses: []v1.EndpointAddress{
							{
								TargetRef: &v1.ObjectReference{
									Kind: "Node",
									Name: "the-node-name-two",
								},
							},
							{
								TargetRef: &v1.ObjectReference{
									Kind: "Node",
									Name: "the-node-name-one",
								},
							},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "No Endpoint Subsets",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{},
			},
			want: false,
		},
		{
			name: "No nodes",
			nodes: &v1.NodeList{
				Items: []v1.Node{},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							TargetRef: &v1.ObjectReference{
								Kind: "Node",
								Name: "the-node-name",
							},
						},
					}},
				},
			},
			want: false,
		},
		{
			name: "EndpointAddress does not match node name",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							TargetRef: &v1.ObjectReference{
								Kind: "Node",
								Name: "wrong-node-name",
							},
						},
					}},
				},
			},
			want: false,
		},
		{
			name: "EndpointAddress without targetRef",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							IP: "1.2.3.4",
						},
					}},
				},
			},
			want: false,
		},
		{
			name: "EndpointAddress with targetRef without name",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							TargetRef: &v1.ObjectReference{
								Kind: "Node",
							},
						},
					}},
				},
			},
			want: false,
		},
		{
			name: "EndpointAddress with targetRef invalid kind",
			nodes: &v1.NodeList{
				Items: []v1.Node{
					{
						ObjectMeta: meta.ObjectMeta{Name: "the-node-name"},
					},
				},
			},
			endpoints: &v1.Endpoints{
				Subsets: []v1.EndpointSubset{
					{Addresses: []v1.EndpointAddress{
						{
							TargetRef: &v1.ObjectReference{
								Kind: "AnotherKind",
							},
						},
					}},
				},
			},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, isEndpointsValid(test.nodes, test.endpoints))
		})
	}
}
