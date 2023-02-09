package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

// remotePowerShellCmdPrefix holds the PowerShell prefix that needs to be prefixed  for every remote PowerShell
// command executed on the remote Windows VM
const remotePowerShellCmdPrefix = "powershell.exe -NonInteractive -ExecutionPolicy Bypass -Command "

func deletionTestSuite(t *testing.T) {
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t) })
}

// clearWindowsInstanceConfigMap removes all entries in the windows-instances ConfigMap
func (tc *testContext) clearWindowsInstanceConfigMap() error {
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), wiparser.InstanceConfigMap,
		meta.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error retrieving windows-instances ConfigMap")
	}
	cm.Data = map[string]string{}
	_, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Update(context.TODO(), cm, meta.UpdateOptions{})
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

			networksRemoved, err := tc.checkNetworksRemoved(addr)
			require.NoError(t, err, "error determining if HNS networks are removed")
			assert.True(t, networksRemoved, "HNS networks not removed")
		})
	}
}

// checkDirsDoNotExist returns true if the required directories do not exist on the Windows instance with the given
// address
func (tc *testContext) checkDirsDoNotExist(address string) (bool, error) {
	command := ""

	for _, dir := range windows.RequiredDirectories {
		command += fmt.Sprintf("if ((Test-Path %s) -eq $true) { Write-Output %s exists}", dir, dir)
	}
	command += "exit 0"
	out, err := tc.runPowerShellSSHJob("check-win-dirs", command, address)
	if err != nil {
		return false, errors.Wrapf(err, "error confirming directories do not exist %s", out)
	}
	return !strings.Contains(out, "exists"), nil
}

// checkNetworksRemoved returns true if the HNS networks created by hybrid-overlay and the
// HNS endpoint created by the operator do not exist on the Windows instance with the given address
func (tc *testContext) checkNetworksRemoved(address string) (bool, error) {
	command := "Get-HnsNetwork; Get-HnsEndpoint"
	out, err := tc.runPowerShellSSHJob("check-hns-networks", command, address)
	if err != nil {
		return false, errors.Wrapf(err, "error confirming networks are removed %s", out)
	}
	return !(strings.Contains(out, windows.BaseOVNKubeOverlayNetwork) ||
		strings.Contains(out, windows.OVNKubeOverlayNetwork) ||
		strings.Contains(out, "VIPEndpoint")), nil
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// Deploy a DaemonSet and wait until its pods have been made ready on each Windows node. DaemonSet pods cannot be
	// drained from a Node.
	// Doing this ensures that WMCO is able to handle containers being present on the instance when deconfiguring.
	ds, err := testCtx.deployNOOPDaemonSet()
	require.NoError(t, err, "error creating DaemonSet")
	defer testCtx.client.K8s.AppsV1().DaemonSets(ds.GetNamespace()).Delete(context.TODO(), ds.GetName(),
		meta.DeleteOptions{})
	err = testCtx.waitUntilDaemonsetScaled(ds.GetName(), int(gc.numberOfMachineNodes+gc.numberOfBYOHNodes))
	require.NoError(t, err, "error waiting for DaemonSet pods to become ready")

	// set expected node count to zero, since all Windows Nodes should be deleted in the test
	expectedNodeCount := int32(0)
	// Get all the Machines created by the e2e tests
	// For platform=none, the resulting slice is empty
	e2eMachineSets, err := testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).List(context.TODO(),
		meta.ListOptions{LabelSelector: clusterinfo.MachineE2ELabel + "=true"})
	require.NoError(t, err, "error listing MachineSets")
	var machineControllerMachineSet *mapi.MachineSet
	for _, machineSet := range e2eMachineSets.Items {
		if machineSet.Spec.Selector.MatchLabels[controllers.IgnoreLabel] != "true" {
			machineControllerMachineSet = &machineSet
			break
		}
	}
	// skip the scale down step if there is no MachineSet for the Windows Machine controller
	if machineControllerMachineSet != nil {
		// Scale the Windows MachineSet to 0
		machineControllerMachineSet.Spec.Replicas = &expectedNodeCount
		_, err = testCtx.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Update(context.TODO(),
			machineControllerMachineSet, meta.UpdateOptions{})
		require.NoError(t, err, "error updating Windows MachineSet")

		// we are waiting 10 minutes for all windows machines to get deleted.
		err = testCtx.waitForWindowsNodes(expectedNodeCount, true, false, false)
		require.NoError(t, err, "Windows node deletion failed")
	}
	t.Run("BYOH node removal", testCtx.testBYOHRemoval)

	// Cleanup all the MachineSets created by us.
	for _, machineSet := range e2eMachineSets.Items {
		assert.NoError(t, testCtx.deleteMachineSet(&machineSet), "error deleting MachineSet")
	}
	// Phase is ignored during deletion, in this case we are just waiting for Machines to be deleted.
	_, err = testCtx.waitForWindowsMachines(int(expectedNodeCount), "", false)
	require.NoError(t, err, "Machine controller Windows machine deletion failed")

	// TODO: Currently on vSphere it is impossible to delete a Machine after its node has been deleted.
	//       This special casing should be removed as part of https://issues.redhat.com/browse/WINC-635
	if testCtx.GetType() != config.VSpherePlatformType {
		_, err = testCtx.waitForWindowsMachines(int(expectedNodeCount), "", true)
		require.NoError(t, err, "ConfigMap controller Windows machine deletion failed")
	}

	// Test if prometheus configuration is updated to have no node entries in the endpoints object
	t.Run("Prometheus configuration", testPrometheus)

	// Cleanup windows-instances ConfigMap
	testCtx.deleteWindowsInstanceConfigMap()

	// Cleanup secrets created by us.
	err = testCtx.client.K8s.CoreV1().Secrets("openshift-machine-api").Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
	require.NoError(t, err, "could not delete userData secret")

	err = testCtx.client.K8s.CoreV1().Secrets(wmcoNamespace).Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete privateKey secret")

	// Cleanup wmco-test namespace created by us.
	err = testCtx.deleteNamespace(testCtx.workloadNamespace)
	require.NoError(t, err, "could not delete test namespace")
}

// DeployNOOPDaemonSet deploys a DaemonSet which will deploy pods in a sleep loop across all Windows nodes
func (tc *testContext) deployNOOPDaemonSet() (*apps.DaemonSet, error) {
	ds := apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name: "noop-ds",
		},
		Spec: apps.DaemonSetSpec{
			Selector: &meta.LabelSelector{
				MatchLabels: map[string]string{"name": "noop-ds"},
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{"name": "noop-ds"},
				},
				Spec: core.PodSpec{
					Tolerations: []core.Toleration{{Key: "os", Value: "Windows", Effect: "NoSchedule"}},
					Containers: []core.Container{{
						Name:    "sleep",
						Image:   tc.getWindowsServerContainerImage(),
						Command: []string{powerShellExe, "-command", "while ($true) {Start-Sleep -Seconds 1}"},
					}},
					NodeSelector: map[string]string{"kubernetes.io/os": "windows"},
				},
			},
		},
	}
	created, err := tc.client.K8s.AppsV1().DaemonSets(tc.workloadNamespace).Create(context.TODO(), &ds,
		meta.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "error creating daemonset %v", ds)
	}
	return created, nil
}

// waitUntilDeploymentScaled will return nil if the daemonset is fully deployed across the Windows nodes
func (tc *testContext) waitUntilDaemonsetScaled(name string, desiredReplicas int) error {
	var ds *apps.DaemonSet
	var err error
	for i := 0; i < retryCount; i++ {
		ds, err = tc.client.K8s.AppsV1().DaemonSets(tc.workloadNamespace).Get(context.TODO(), name, meta.GetOptions{})
		if err != nil {
			return errors.Wrapf(err, "could not get daemonset %s", name)
		}
		if int(ds.Status.NumberAvailable) == desiredReplicas {
			return nil
		}
		time.Sleep(retryInterval)
	}
	return errors.Errorf("timed out waiting for daemonset %s to scale, current status: %+v", name, ds.Status)
}
