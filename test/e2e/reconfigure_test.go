package e2e

import (
	"context"
	"encoding/json"
	"testing"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

func reconfigurationTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	if tc.CloudProvider.GetType() == config.VSpherePlatformType {
		t.Run("Re-add removed instance", tc.testReAddInstance)
	}
	t.Run("Change private key", testPrivateKeyChange)
}

// testReAddInstance tests the case where a Windows BYOH instance was removed from the cluster, and then re-added.
func (tc *testContext) testReAddInstance(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH testing disabled")
	}

	windowsInstances, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), wiparser.InstanceConfigMap,
		metav1.GetOptions{})
	require.NoError(t, err, "error retrieving windows-instances ConfigMap")
	require.NotEmpty(t, windowsInstances.Data, "no instances to remove")

	// Read a single entry from the ConfigMap data
	var addr, data string
	for addr, data = range windowsInstances.Data {
		break
	}

	// remove the entry that was found and then update the ConfigMap
	delete(windowsInstances.Data, addr)

	patchData := []*patch.JSONPatch{patch.NewJSONPatch("remove", "/data", windowsInstances.Data)}
	// convert patch data to bytes
	patchDataBytes, err := json.Marshal(patchData)
	require.NoError(t, err, "error getting patch data in bytes")

	windowsInstances, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(),
		wiparser.InstanceConfigMap, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	require.NoError(t, err, "error patching windows-instances ConfigMap data with remove operation")
	// Ensure operator communicates to OLM that upgrade is not safe when processing BYOH nodes
	err = tc.validateUpgradeableCondition(metav1.ConditionFalse)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")

	// wait for the node to be removed
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes-1, true, true)
	require.NoError(t, err, "error waiting for the removal of a node")

	// update the ConfigMap again, re-adding the instance
	if windowsInstances.Data == nil {
		windowsInstances.Data = make(map[string]string)
	}
	windowsInstances.Data[addr] = data

	patchData = []*patch.JSONPatch{patch.NewJSONPatch("add", "/data", windowsInstances.Data)}
	// convert patch data to bytes
	patchDataBytes, err = json.Marshal(patchData)
	require.NoError(t, err, "error getting patch data in bytes")

	windowsInstances, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(),
		wiparser.InstanceConfigMap, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	require.NoError(t, err, "error patching windows-instances ConfigMap data with add operation")

	// wait for the node to be successfully re-added
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, true, true)
	assert.NoError(t, err, "error waiting for the Windows node to be re-added")

	err = tc.validateUpgradeableCondition(metav1.ConditionTrue)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")
}
