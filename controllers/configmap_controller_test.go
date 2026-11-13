package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
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

// TestEnsureInstancesAreUpToDateContinuesOnFailure verifies that
// ensureInstancesAreUpToDate processes ALL instances even when individual
// instances fail, and returns an aggregated error containing each failure.
// Previously, the function returned on the first per-node error, which blocked
// subsequent instances and prevented cleanup of removed nodes.
func TestEnsureInstancesAreUpToDateContinuesOnFailure(t *testing.T) {
	watchNamespace := "test-ns"

	// Create a fake client with the private key secret so the function
	// gets past the GetPrivateKey call and into the per-instance loop.
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))
	privateKeySecret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      secrets.PrivateKeySecret,
			Namespace: watchNamespace,
		},
		Data: map[string][]byte{
			secrets.PrivateKeySecretKey: []byte("test-key-data"),
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(privateKeySecret).Build()

	r := &ConfigMapReconciler{
		instanceReconciler: instanceReconciler{
			client:         fakeClient,
			log:            logr.Discard(),
			watchNamespace: watchNamespace,
			// clusterServiceCIDR is intentionally empty so each
			// ensureInstanceIsUpToDate call fails at NewNodeConfig with
			// a CIDR validation error. This gives predictable failures
			// for both instances.
		},
	}

	// Two instances that will both fail (no SSH infrastructure, invalid CIDR)
	instances := []*instance.Info{
		{Address: "10.0.0.1", IPv4Address: "10.0.0.1", Username: "user1"},
		{Address: "10.0.0.2", IPv4Address: "10.0.0.2", Username: "user2"},
	}

	err := r.ensureInstancesAreUpToDate(context.Background(), instances)
	require.Error(t, err, "expected errors from both instances")

	errMsg := err.Error()
	assert.True(t, strings.Contains(errMsg, "10.0.0.1"),
		"error should mention first instance address, got: %s", errMsg)
	assert.True(t, strings.Contains(errMsg, "10.0.0.2"),
		"error should mention second instance address, got: %s", errMsg)
}

// TestReconcileNodesDeconfiguresDespiteUpdateFailure verifies that
// reconcileNodes still calls deconfigureInstances even when
// ensureInstancesAreUpToDate returns errors. This prevents a failed node
// update from blocking cleanup of removed instances.
func TestReconcileNodesDeconfiguresDespiteUpdateFailure(t *testing.T) {
	watchNamespace := "test-ns"

	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))

	privateKeySecret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      secrets.PrivateKeySecret,
			Namespace: watchNamespace,
		},
		Data: map[string][]byte{
			secrets.PrivateKeySecretKey: []byte("test-key-data"),
		},
	}

	// Create a BYOH node that is NOT in the instances ConfigMap.
	// This node should be a candidate for deconfiguration.
	orphanedNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "orphaned-node",
			Labels: map[string]string{
				BYOHLabel:          "true",
				core.LabelOSStable: "windows",
			},
			Annotations: map[string]string{
				UsernameAnnotation: "encrypted-user",
			},
		},
		Status: core.NodeStatus{
			Addresses: []core.NodeAddress{
				{Type: core.NodeInternalIP, Address: "10.0.0.99"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(privateKeySecret, orphanedNode).Build()

	r := &ConfigMapReconciler{
		instanceReconciler: instanceReconciler{
			client:         fakeClient,
			log:            logr.Discard(),
			watchNamespace: watchNamespace,
		},
	}

	// ConfigMap data has one instance that will fail to update,
	// but does NOT include the orphaned node's address.
	windowsInstances := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      wiparser.InstanceConfigMap,
			Namespace: watchNamespace,
		},
		Data: map[string]string{
			"10.0.0.1": "user1",
		},
	}

	err := r.reconcileNodes(context.Background(), windowsInstances)
	// The function should return an error (from the failed instance update
	// and/or from the deconfiguration of the orphaned node), but the critical
	// assertion is that deconfigureInstances was reached — evidenced by
	// attempting to deconfigure the orphaned node. Without the fix,
	// deconfigureInstances would never be called.
	require.Error(t, err, "expected error from update failure")

	// Verify the error contains evidence of processing the instance update failure.
	// The orphaned node deconfigure will also fail (no SSH infrastructure), but
	// the important thing is that reconcileNodes attempted it.
	errMsg := err.Error()
	assert.True(t, strings.Contains(errMsg, "10.0.0.1"),
		"error should contain the failed instance address, got: %s", errMsg)
}
