package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
)

// newTestSecretReconciler returns a SecretReconciler backed by a fake client pre-seeded with initObjs.
func newTestSecretReconciler(initObjs ...client.Object) *SecretReconciler {
	scheme := runtime.NewScheme()
	_ = core.AddToScheme(scheme)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjs...).Build()
	return &SecretReconciler{
		instanceReconciler: instanceReconciler{
			client: fc,
			log:    ctrl.Log.WithName("test"),
		},
	}
}

// newUserDataSecret creates a minimal windows-user-data Secret in the given namespace.
func newUserDataSecret(namespace, userData string) *core.Secret {
	return &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      secrets.UserDataSecret,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"userData": []byte(userData),
		},
	}
}

// TestIsUserDataSecret verifies that isUserDataSecret matches secrets in both the Machine API and
// Cluster API namespaces, but not in unrelated namespaces.
func TestIsUserDataSecret(t *testing.T) {
	tests := []struct {
		name     string
		ns       string
		expected bool
	}{
		{
			name:     "Machine API namespace matches",
			ns:       cluster.MachineAPINamespace,
			expected: true,
		},
		{
			name:     "Cluster API namespace matches",
			ns:       cluster.ClusterAPINamespace,
			expected: true,
		},
		{
			name:     "Unrelated namespace does not match",
			ns:       "kube-system",
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newUserDataSecret(tt.ns, "data")
			assert.Equal(t, tt.expected, isUserDataSecret(s))
		})
	}
}

// TestIsClusterAPIEnabled verifies that isClusterAPIEnabled returns true only when the
// openshift-cluster-api namespace exists.
func TestIsClusterAPIEnabled(t *testing.T) {
	ctx := context.Background()

	t.Run("Namespace absent returns false", func(t *testing.T) {
		r := newTestSecretReconciler()
		enabled, err := r.isClusterAPIEnabled(ctx)
		require.NoError(t, err)
		assert.False(t, enabled)
	})

	t.Run("Namespace present returns true", func(t *testing.T) {
		ns := &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: cluster.ClusterAPINamespace}}
		r := newTestSecretReconciler(ns)
		enabled, err := r.isClusterAPIEnabled(ctx)
		require.NoError(t, err)
		assert.True(t, enabled)
	})
}

// TestEnsureCAPIUserDataSecret verifies the create / no-op / update behaviour of ensureCAPIUserDataSecret.
func TestEnsureCAPIUserDataSecret(t *testing.T) {
	ctx := context.Background()
	const content = "<powershell>ssh-rsa AAAA...</powershell>"

	t.Run("Creates secret when absent in CAPI namespace", func(t *testing.T) {
		r := newTestSecretReconciler()
		src := newUserDataSecret(cluster.MachineAPINamespace, content)

		require.NoError(t, r.ensureCAPIUserDataSecret(ctx, src))

		got := &core.Secret{}
		require.NoError(t, r.client.Get(ctx,
			client.ObjectKey{Name: secrets.UserDataSecret, Namespace: cluster.ClusterAPINamespace}, got))
		assert.Equal(t, content, string(got.Data["userData"]))
		assert.Equal(t, cluster.ClusterAPINamespace, got.Namespace)
	})

	t.Run("No-op when CAPI secret already matches", func(t *testing.T) {
		existing := newUserDataSecret(cluster.ClusterAPINamespace, content)
		existing.ResourceVersion = "42"
		r := newTestSecretReconciler(existing)
		src := newUserDataSecret(cluster.MachineAPINamespace, content)

		require.NoError(t, r.ensureCAPIUserDataSecret(ctx, src))

		got := &core.Secret{}
		require.NoError(t, r.client.Get(ctx,
			client.ObjectKey{Name: secrets.UserDataSecret, Namespace: cluster.ClusterAPINamespace}, got))
		// ResourceVersion must be unchanged (no update was issued).
		assert.Equal(t, "42", got.ResourceVersion)
	})

	t.Run("Updates CAPI secret when content differs", func(t *testing.T) {
		existing := newUserDataSecret(cluster.ClusterAPINamespace, "old-data")
		existing.ResourceVersion = "1"
		r := newTestSecretReconciler(existing)
		src := newUserDataSecret(cluster.MachineAPINamespace, content)

		require.NoError(t, r.ensureCAPIUserDataSecret(ctx, src))

		got := &core.Secret{}
		require.NoError(t, r.client.Get(ctx,
			client.ObjectKey{Name: secrets.UserDataSecret, Namespace: cluster.ClusterAPINamespace}, got))
		assert.Equal(t, content, string(got.Data["userData"]))
	})
}
