package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

func deletionTestSuite(t *testing.T) {
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t) })
}

// clearWindowsInstanceConfigMap removes all entries in the windows-instances ConfigMap
func (tc *testContext) clearWindowsInstanceConfigMap() error {
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Get(context.TODO(), wiparser.InstanceConfigMap,
		meta.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error retrieving windows-instances ConfigMap")
	}
	cm.Data = map[string]string{}
	_, err = tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Update(context.TODO(), cm, meta.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "error clearing windows-instances ConfigMap data")
	}
	return nil
}

// testBYOHRemoval tests that nodes are properly removed and deconfigured after removing ConfigMap entries
func (tc *testContext) testBYOHRemoval(t *testing.T) {
	// Get list of BYOH nodes before the node objects are deleted
	byohNodes, err := tc.listFullyConfiguredWindowsNodes(true)
	require.NoError(t, err, "error getting BYOH node list")

	// Remove all entries from the windows-instances ConfigMap, causing all node objects to be deleted
	require.NoError(t, tc.clearWindowsInstanceConfigMap(),
		"error removing windows-instances ConfigMap entries")
	err = tc.waitForWindowsNodes(0, true, false, true)
	require.NoError(t, err, "Removing ConfigMap entries did not cause Windows node deletion")

	// For each node that was deleted, check that all the expected services have been removed
	for _, node := range byohNodes {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")
			svcs, err := tc.getWinServices(addr)
			require.NoError(t, err, "error getting service map")
			for _, svcName := range windows.RequiredServices {
				t.Run(svcName, func(t *testing.T) {
					require.NotContains(t, svcs, svcName, "service present")
				})
			}
			dirsCleanedUp, err := tc.checkDirsDoNotExist(addr)
			require.NoError(t, err, "error determining if created directories exist")
			assert.True(t, dirsCleanedUp, "directories not removed")
		})
	}
}

// checkDirsDoNotExist returns true if the required directories do not exist on the Windows instance with the given
// address
func (tc *testContext) checkDirsDoNotExist(address string) (bool, error) {
	command := "powershell.exe -NonInteractive -ExecutionPolicy Bypass -Command \""
	for _, dir := range windows.RequiredDirectories {
		command += fmt.Sprintf("if ((Test-Path %s) -eq $true) { Write-Output %s exists}", dir, dir)
	}
	command += "exit 0\""
	out, err := tc.runSSHJob("check-win-dirs", command, address)
	if err != nil {
		return false, errors.Wrapf(err, "error confirming directories do not exist %s", out)
	}
	return !strings.Contains(out, "exists"), nil
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// Get all the Machines created by the e2e tests
	e2eMachineSets, err := testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).List(context.TODO(),
		meta.ListOptions{LabelSelector: clusterinfo.MachineE2ELabel + "=true"})
	require.NoError(t, err, "error listing MachineSets")
	var windowsMachineSetWithLabel *mapi.MachineSet
	for _, machineSet := range e2eMachineSets.Items {
		if machineSet.Spec.Selector.MatchLabels[clusterinfo.MachineOSIDLabel] == "Windows" {
			windowsMachineSetWithLabel = &machineSet
			break
		}
	}

	require.NotNil(t, windowsMachineSetWithLabel, "could not find MachineSet with Windows label")

	// Scale the Windows MachineSet to 0
	expectedNodeCount := int32(0)
	windowsMachineSetWithLabel.Spec.Replicas = &expectedNodeCount
	_, err = testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Update(context.TODO(),
		windowsMachineSetWithLabel, meta.UpdateOptions{})
	require.NoError(t, err, "error updating Windows MachineSet")

	// we are waiting 10 minutes for all windows machines to get deleted.
	err = testCtx.waitForWindowsNodes(expectedNodeCount, true, false, false)
	require.NoError(t, err, "Windows node deletion failed")

	t.Run("BYOH node removal", testCtx.testBYOHRemoval)

	// Cleanup all the MachineSets created by us.
	for _, machineSet := range e2eMachineSets.Items {
		assert.NoError(t, testCtx.deleteMachineSet(&machineSet), "error deleting MachineSet")
	}
	// Phase is ignored during deletion, in this case we are just waiting for Machines to be deleted.
	_, err = testCtx.waitForWindowsMachines(int(expectedNodeCount), "", true)
	require.NoError(t, err, "Machine controller Windows machine deletion failed")

	// TODO: Currently on vSphere it is impossible to delete a Machine after its node has been deleted.
	//       This special casing should be removed as part of https://issues.redhat.com/browse/WINC-635
	if testCtx.GetType() != config.VSpherePlatformType {
		_, err = testCtx.waitForWindowsMachines(int(expectedNodeCount), "", false)
		require.NoError(t, err, "ConfigMap controller Windows machine deletion failed")
	}

	// Test if prometheus configuration is updated to have no node entries in the endpoints object
	t.Run("Prometheus configuration", testPrometheus)

	// Cleanup windows-instances ConfigMap
	testCtx.deleteWindowsInstanceConfigMap()

	// Cleanup secrets created by us.
	err = testCtx.client.K8s.CoreV1().Secrets("openshift-machine-api").Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
	require.NoError(t, err, "could not delete userData secret")

	err = testCtx.client.K8s.CoreV1().Secrets("openshift-windows-machine-config-operator").Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete privateKey secret")

	// Cleanup wmco-test namespace created by us.
	err = testCtx.deleteNamespace(testCtx.workloadNamespace)
	require.NoError(t, err, "could not delete test namespace")
}
