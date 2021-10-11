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

	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

func reconfigurationTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run("Reconfigure instance", reconfigurationTest)
	if testCtx.CloudProvider.GetType() == config.VSpherePlatformType {
		t.Run("Re-add removed instance", testReAddInstance)
	}
	// testPrivateKeyChange must be the last test run in the reconfiguration suite. This is because we do not currently
	// wait for nodes to fully come back up after changing the private key back to the valid key. Only the deletion test
	// suite should run after this. Any other tests may result in flakes.
	// This limitation will be removed with https://issues.redhat.com/browse/WINC-655
	t.Run("Change private key", testPrivateKeyChange)
}

// reconfigurationTest tests that the correct behavior occurs when a previously configured instance is configured
// again. In practice, this exact scenario should not happen, however it simulates a similar scenario where an instance
// was almost completely configured, an error occurred, and the instance is requeued. This is a scenario that should be
// expected to be ran into often enough, for reasons such as network instability. For that reason this test is warranted.
func reconfigurationTest(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	machineNodes, err := testCtx.listFullyConfiguredWindowsNodes(false)
	require.NoError(t, err)
	byohNodes, err := testCtx.listFullyConfiguredWindowsNodes(true)
	require.NoError(t, err)

	// Remove the version annotation of one of each type of node
	patchData, err := metadata.GenerateRemovePatch([]string{}, []string{metadata.VersionAnnotation})
	require.NoError(t, err)
	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), machineNodes[0].Name, types.JSONPatchType,
		patchData, metav1.PatchOptions{})
	require.NoError(t, err)
	// Ensure operator communicates to OLM that upgrade is not safe when processing Machine nodes
	err = testCtx.validateUpgradeableCondition(metav1.ConditionFalse)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")

	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), byohNodes[0].Name, types.JSONPatchType,
		patchData, metav1.PatchOptions{})
	require.NoError(t, err)

	// The Windows nodes should eventually be returned to the state we expect them to be in
	err = testCtx.waitForWindowsNodes(gc.numberOfMachineNodes, false, true, false)
	assert.NoError(t, err, "error waiting for Windows Machine nodes to be reconfigured")

	err = testCtx.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	assert.NoError(t, err, "error waiting for Windows BYOH nodes to be reconfigured")

	err = testCtx.validateUpgradeableCondition(metav1.ConditionTrue)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")
}

// testReAddInstance tests the case where a Windows BYOH instance was removed from the cluster, and then re-added.
func testReAddInstance(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH testing disabled")
	}

	tc, err := NewTestContext()
	require.NoError(t, err)

	windowsInstances, err := tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Get(context.TODO(), wiparser.InstanceConfigMap,
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

	windowsInstances, err = tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Patch(context.TODO(),
		wiparser.InstanceConfigMap, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	require.NoError(t, err, "error patching windows-instances ConfigMap data with remove operation")
	// Ensure operator communicates to OLM that upgrade is not safe when processing BYOH nodes
	err = tc.validateUpgradeableCondition(metav1.ConditionFalse)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")

	// wait for the node to be removed
	err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes-1, false, true, true)
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

	windowsInstances, err = tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Patch(context.TODO(),
		wiparser.InstanceConfigMap, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	require.NoError(t, err, "error patching windows-instances ConfigMap data with add operation")

	// wait for the node to be successfully re-added
	err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	assert.NoError(t, err, "error waiting for the Windows node to be re-added")

	err = tc.validateUpgradeableCondition(metav1.ConditionTrue)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")
}
