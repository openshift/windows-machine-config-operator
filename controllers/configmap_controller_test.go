package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instances"
)

func TestParseHosts(t *testing.T) {
	r := ConfigMapReconciler{}

	testCases := []struct {
		name        string
		input       map[string]string
		expectedOut []*instances.InstanceInfo
		expectedErr bool
	}{
		{
			name:        "invalid username",
			input:       map[string]string{"localhost": "notusername=core"},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "invalid DNS address",
			input:       map[string]string{"notlocalhost": "username=core"},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "invalid username and DNS",
			input:       map[string]string{"invalid": "invalid"},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "valid ipv6 address",
			input:       map[string]string{"::1": "username=core"},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "valid dns address",
			input:       map[string]string{"localhost": "username=core"},
			expectedOut: []*instances.InstanceInfo{{Address: "localhost", Username: "core"}},
			expectedErr: false,
		},
		{
			name:        "valid ip address",
			input:       map[string]string{"127.0.0.1": "username=core"},
			expectedOut: []*instances.InstanceInfo{{Address: "127.0.0.1", Username: "core"}},
			expectedErr: false,
		},
		{
			name:        "valid dns and ip addresses",
			input:       map[string]string{"localhost": "username=core", "127.0.0.1": "username=Admin"},
			expectedOut: []*instances.InstanceInfo{{Address: "localhost", Username: "core"}, {Address: "127.0.0.1", Username: "Admin"}},
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := r.parseHosts(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}

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
					Name:      InstanceConfigMap,
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
					Name:      InstanceConfigMap,
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
