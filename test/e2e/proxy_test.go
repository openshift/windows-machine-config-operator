package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

// proxyTestSuite contains the validation cases for cluster-wide proxy.
// All subtests are skipped if a proxy is not enabled in the test environment.
func proxyTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	clusterProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		require.NoError(t, err)
	}
	if clusterProxy.Status.HTTPProxy == "" && clusterProxy.Status.HTTPSProxy == "" {
		t.Skip("cluster-wide proxy is not enabled in this environment")
	}

	t.Run("Trusted CA ConfigMap validation", testTrustedCAConfigMap)
}

// testTrustedCAConfigMap tests multiple aspects of expected functionality for the trusted-ca ConfigMap
// 1. It exists on operator startup 2. It is re-created when deleted 3. It is patched if invalid contents are detected.
// The ConfigMap data is managed by CNO so no need to do content validation testing.
func testTrustedCAConfigMap(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	// Ensure the trusted-ca ConfigMap exists in the cluster as expected
	t.Run("Trusted CA ConfigMap metadata", func(t *testing.T) {
		trustedCA, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			controllers.ProxyCertsConfigMap, meta.GetOptions{})
		require.NoErrorf(t, err, "error ensuring ConfigMap %s exists", controllers.ProxyCertsConfigMap)
		assert.True(t, trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true")
	})

	t.Run("Trusted CA ConfigMap re-creation", func(t *testing.T) {
		err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(),
			controllers.ProxyCertsConfigMap, meta.DeleteOptions{})
		require.NoError(t, err)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoErrorf(t, err, "error ensuring ConfigMap %s is re-created when deleted", controllers.ProxyCertsConfigMap)
	})

	t.Run("Invalid trusted CA ConfigMap patching", func(t *testing.T) {
		// Intentionally remove the required label and wait for WMCO to reconcile and re-apply it
		var labelPatch = []*patch.JSONPatch{
			patch.NewJSONPatch("remove", "/metadata/labels", map[string]string{controllers.InjectionRequestLabel: "true"}),
		}
		patchData, err := json.Marshal(labelPatch)
		require.NoError(t, err)

		_, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(), controllers.ProxyCertsConfigMap,
			types.JSONPatchType, patchData, meta.PatchOptions{})
		require.NoErrorf(t, err, "unable to patch %s", controllers.ProxyCertsConfigMap)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoError(t, err, "error testing handling of invalid ConfigMap")
	})
}

// waitForValidTrustedCAConfigMap returns a reference to the ConfigMap that matches the given name.
// If a ConfigMap with valid contents is not found within the time limit, an error is returned.
func (tc *testContext) waitForValidTrustedCAConfigMap() error {
	trustedCA := &core.ConfigMap{}
	err := wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		var err error
		trustedCA, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			controllers.ProxyCertsConfigMap, meta.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Retry if the Get() results in a IsNotFound error
				return false, nil
			}
			return false, fmt.Errorf("error retrieving ConfigMap %s: %w", controllers.ProxyCertsConfigMap, err)
		}
		// Here, we've retreived a ConfigMap but still need to ensure it is valid.
		// If it's not valid, retry in hopes that WMCO will replace it with a valid one as expected.
		return trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true", nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for ConfigMap %s/%s: %w", wmcoNamespace, controllers.ProxyCertsConfigMap, err)
	}
	return nil
}
