package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
)

func reconfigurationTestSuite(t *testing.T) {
	t.Run("Reconfigure instance", reconfigurationTest)
	t.Run("Re-add removed instance", testReAddInstance)
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
	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), byohNodes[0].Name, types.JSONPatchType,
		patchData, metav1.PatchOptions{})
	require.NoError(t, err)

	// The Windows nodes should eventually be returned to the state we expect them to be in
	err = testCtx.waitForWindowsNodes(gc.numberOfMachineNodes, false, true, false)
	assert.NoError(t, err, "error waiting for Windows Machine nodes to be reconfigured")

	err = testCtx.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	assert.NoError(t, err, "error waiting for Windows BYOH nodes to be reconfigured")
}

// testReAddInstance tests the case where a Windows BYOH instance was removed from the cluster, and then re-added.
func testReAddInstance(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH testing disabled")
	}

	tc, err := NewTestContext()
	require.NoError(t, err)

	cm, err := tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Get(context.TODO(), controllers.InstanceConfigMap,
		metav1.GetOptions{})
	require.NoError(t, err, "error retrieving windows-instances ConfigMap")
	require.NotEmpty(t, cm.Data, "no instances to remove")

	// Read a single entry from the data map
	var addr, data string
	for addr, data = range cm.Data {
		break
	}

	// remove the entry that was found and then update the ConfigMap
	delete(cm.Data, addr)
	cm, err = tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
	require.NoError(t, err, "error updating windows-instances ConfigMap data")

	// wait for the node to be removed
	err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes-1, false, true, true)
	require.NoError(t, err, "error waiting for the removal of a node")

	// update the ConfigMap again, re-adding the instance
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[addr] = data
	_, err = tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Update(context.TODO(), cm, metav1.UpdateOptions{})
	require.NoError(t, err, "error updating windows-instances ConfigMap data")

	// wait for the node to be successfully re-added
	err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	assert.NoError(t, err, "error waiting for the node to be re-added")
}
