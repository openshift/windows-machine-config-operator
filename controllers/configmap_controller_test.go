package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
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
			name: "valid instances ConfigMap",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      wiparser.InstanceConfigMap,
					Namespace: watchNamespace,
				},
			},
			isValidConfigMap: true,
		},
		{
			name: "valid services ConfigMap",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      servicescm.Name,
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
			name: "invalid instances ConfigMap namespace",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      wiparser.InstanceConfigMap,
					Namespace: "invalid",
				},
			},
			isValidConfigMap: false,
		},
		{
			name: "invalid services ConfigMap namespace",
			configMapObj: &core.ConfigMap{
				ObjectMeta: meta.ObjectMeta{
					Name:      servicescm.Name,
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
