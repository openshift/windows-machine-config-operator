package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

func TestIsValidConfigMap(t *testing.T) {
	watchNamespace := "test"
	r := ConfigMapReconciler{instanceReconciler: instanceReconciler{watchNamespace: watchNamespace}}

	var tests = []struct {
		name             string
		configMapObj     client.Object
		isValidConfigMap bool
	}{
		{
			name: "valid ConfigMap",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      wiparser.InstanceConfigMap,
					Namespace: watchNamespace,
				},
			},
			isValidConfigMap: true,
		},
		{
			name:             "empty ConfigMap",
			configMapObj:     &core.ConfigMap{},
			isValidConfigMap: false,
		},
		{
			name: "invalid ConfigMap",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      "invalid",
					Namespace: "invalid",
				},
			},
			isValidConfigMap: false,
		},
		{
			name: "invalid namespace",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      wiparser.InstanceConfigMap,
					Namespace: "invalid",
				},
			},
			isValidConfigMap: false,
		},
		{
			name: "invalid name",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      "invalid",
					Namespace: watchNamespace,
				},
			},
			isValidConfigMap: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isValidConfigMap := r.isValidConfigMap(test.configMapObj)
			require.Equal(t, test.isValidConfigMap, isValidConfigMap)
		})
	}
}

func TestHasAssociatedInstance(t *testing.T) {
	type args struct {
		nodeAddresses []core.NodeAddress
		instances     []*instance.Info
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "ignore external address with no reverse lookup record and use internal IP",
			args: args{
				nodeAddresses: []core.NodeAddress{
					{Type: core.NodeExternalIP, Address: "11.22.33.44"},
					{Type: core.NodeInternalIP, Address: "127.0.0.1"},
				},
				instances: []*instance.Info{{Address: "localhost"}},
			},
			want: true,
		},
		{
			name: "valid internal IP",
			args: args{
				nodeAddresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "1.2.3.4"}},
				instances:     []*instance.Info{{IPv4Address: "1.2.3.4"}},
			},
			want: true,
		},
		{
			name: "valid internal DNS",
			args: args{
				nodeAddresses: []core.NodeAddress{{Type: core.NodeInternalDNS, Address: "internal.local"}},
				instances:     []*instance.Info{{Address: "internal.local"}},
			},
			want: true,
		},
		{
			name: "internal IP with invalid DNS lookup",
			args: args{
				nodeAddresses: []core.NodeAddress{{Type: core.NodeInternalIP, Address: "1.2.3.4"}},
				instances:     []*instance.Info{{Address: "invalid.dns"}},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasAssociatedInstance(tt.args.nodeAddresses, tt.args.instances)
			require.Equalf(t, tt.want, got, "hasAssociatedInstance(%s, %s)", tt.args.nodeAddresses, tt.args.instances)
		})
	}
}
