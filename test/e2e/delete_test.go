package e2e

import (
	"context"
	"strings"
	"testing"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

func deletionTestSuite(t *testing.T) {
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t) })
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// Get all the Machines in the
	machineSetList, err := testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).List(context.TODO(), meta.ListOptions{})
	require.NoError(t, err, "error listing MachineSets")
	var windowsMachineSetWithLabel string
	var e2eMachineSets []*mapi.MachineSet
	for _, machineSet := range machineSetList.Items {
		if strings.Contains(machineSet.Name, "e2e-windows-machineset-") {
			e2eMachineSets = append(e2eMachineSets, &machineSet)
		}
		if machineSet.Spec.Selector.MatchLabels["machine.openshift.io/os-id"] == "Windows" {
			windowsMachineSetWithLabel = machineSet.Name
		}
	}
	windowsMachineSet, err := testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		windowsMachineSetWithLabel, meta.GetOptions{})
	require.NoError(t, err, "error getting Windows MachineSet")
	// Reset the number of nodes to be deleted to 0
	gc.numberOfNodes = 0
	// Delete the Windows VM that got created.
	windowsMachineSet.Spec.Replicas = &gc.numberOfNodes
	_, err = testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Update(context.TODO(), windowsMachineSet,
		meta.UpdateOptions{})
	require.NoError(t, err, "error updating Windows MachineSet")
	// we are waiting 10 minutes for all windows machines to get deleted.
	err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, true, false)
	require.NoError(t, err, "Windows node deletion failed")

	// Cleanup all the MachineSets created by us.
	for _, machineSet := range e2eMachineSets {
		assert.NoError(t, testCtx.deleteMachineSet(machineSet), "error deleting MachineSet")
	}
	// Phase is ignored during deletion, in this case we are just waiting for Machines to be deleted.
	require.NoError(t, testCtx.waitForWindowsMachines(int(gc.numberOfNodes), ""), "Windows machine deletion failed")

	// Test if prometheus configuration is updated to have no node entries in the endpoints object
	testPrometheus(t)

	// Cleanup secrets created by us.
	err = testCtx.client.K8s.CoreV1().Secrets("openshift-machine-api").Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
	require.NoError(t, err, "could not delete userData secret")

	err = testCtx.client.K8s.CoreV1().Secrets("openshift-windows-machine-config-operator").Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete privateKey secret")

	// Cleanup wmco-test namespace created by us.
	err = testCtx.deleteNamespace(testCtx.workloadNamespace)
	require.NoError(t, err, "could not delete test namespace")
}
