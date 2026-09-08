package e2e

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

func reconfigurationTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	t.Run("Re-add removed instance", tc.testReAddInstance)
	t.Run("Change private key", testPrivateKeyChange)
}

// findNodeByConfigMapAddress finds a node where any address matches the ConfigMap address
func (tc *testContext) findNodeByConfigMapAddress(t *testing.T, configMapAddr string) *core.Node {
	nodes := gc.allNodes()
	for i := range nodes {
		for _, nodeAddr := range nodes[i].Status.Addresses {
			if nodeAddr.Address == configMapAddr {
				return &nodes[i]
			}
		}
	}
	return nil
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

	// Ensure we have fresh node data by waiting for expected nodes
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, true, true)
	require.NoError(t, err, "error refreshing BYOH nodes before test")

	// Capture the host key annotation before removal to verify TOFU persistence
	// Find the node by matching addresses using controllers.GetAddress logic
	var originalHostKey string
	node := tc.findNodeByConfigMapAddress(t, addr)
	require.NotNil(t, node, "could not find node with address %s before removal", addr)
	originalHostKey = node.Annotations[windows.SSHHostKeyAnnotation]
	require.NotEmpty(t, originalHostKey, "node should have ssh-host-key annotation before removal")

	// remove the entry that was found and then update the ConfigMap
	delete(windowsInstances.Data, addr)

	patchData := []*patch.JSONPatch{patch.NewJSONPatch("remove", "/data", windowsInstances.Data)}
	// convert patch data to bytes
	patchDataBytes, err := json.Marshal(patchData)
	require.NoError(t, err, "error getting patch data in bytes")

	windowsInstances, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(),
		wiparser.InstanceConfigMap, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	require.NoError(t, err, "error patching windows-instances ConfigMap data with remove operation")

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
	require.NoError(t, err, "error waiting for the Windows node to be re-added")
	tc.testNodesBecomeReadyAndSchedulable(t)

	// Verify the host key persists across the re-add (same VM should have same key - TOFU)
	readdedNode := tc.findNodeByConfigMapAddress(t, addr)
	require.NotNil(t, readdedNode, "could not find re-added node with address %s", addr)
	readdedHostKey := readdedNode.Annotations[windows.SSHHostKeyAnnotation]
	require.NotEmpty(t, readdedHostKey, "re-added node should have ssh-host-key annotation")
	require.Equal(t, originalHostKey, readdedHostKey,
		"host key should persist across re-add for the same instance")
}
