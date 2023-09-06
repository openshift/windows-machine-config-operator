package e2e

import (
	"context"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

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
	tc, err := NewTestContext()
	require.NoError(t, err)
	t.Run("Deletion", tc.testWindowsNodeDeletion)
}

// clearWindowsInstanceConfigMap removes all entries in the windows-instances ConfigMap
func (tc *testContext) clearWindowsInstanceConfigMap() error {
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), wiparser.InstanceConfigMap,
		meta.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving windows-instances ConfigMap: %w", err)
	}
	cm.Data = map[string]string{}
	_, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Update(context.TODO(), cm, meta.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error clearing windows-instances ConfigMap data: %w", err)
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
	err = tc.waitForWindowsNodeRemoval(true)
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

			envVarsRemoved, err := tc.checkEnvVarsRemoved(addr)
			require.NoError(t, err, "error determining if ENV vars are removed")
			assert.True(t, envVarsRemoved, "ENV vars not removed")
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
		return false, fmt.Errorf("error confirming directories do not exist %s: %w", out, err)
	}
	return !strings.Contains(out, "exists"), nil
}

// checkNetworksRemoved returns true if the HNS networks created by hybrid-overlay and the
// HNS endpoint created by the operator do not exist on the Windows instance with the given address
func (tc *testContext) checkNetworksRemoved(address string) (bool, error) {
	command := "Get-HnsNetwork; Get-HnsEndpoint"
	out, err := tc.runPowerShellSSHJob("check-hns-networks", command, address)
	if err != nil {
		return false, fmt.Errorf("error confirming networks are removed %s: %w", out, err)
	}
	return !(strings.Contains(out, windows.BaseOVNKubeOverlayNetwork) ||
		strings.Contains(out, windows.OVNKubeOverlayNetwork) ||
		strings.Contains(out, "VIPEndpoint")), nil
}

// checkEnvVarsRemoved returns true if the system and service level ENV vars do not exist on the Windows instance
func (tc *testContext) checkEnvVarsRemoved(address string) (bool, error) {
	watchedEnvVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	for _, proxyVar := range watchedEnvVars {
		systemEnvVars, err := tc.getSystemEnvVar(address, proxyVar)
		if err != nil {
			return false, fmt.Errorf("error retrieving system level ENV vars: %w", err)
		}
		if _, exists := systemEnvVars[proxyVar]; exists {
			return false, nil
		}
		for _, svcName := range windows.RequiredServices {
			svcEnvVars, err := tc.getProxyEnvVarsFromService(address, svcName)
			if err != nil {
				return false, fmt.Errorf("error retrieving service level ENV vars: %w", err)
			}
			if _, exists := svcEnvVars[proxyVar]; exists {
				return false, nil
			}
		}
	}
	return true, nil
}

// waitForWindowsNodeRemoval returns when there are zero Windows nodes of the given type, machine or byoh, in the cluster
func (tc *testContext) waitForWindowsNodeRemoval(isBYOH bool) error {
	labelSelector := core.LabelOSStable + "=windows"
	if isBYOH {
		// BYOH label is set to true
		labelSelector = fmt.Sprintf("%s,%s=true", labelSelector, controllers.BYOHLabel)
	} else {
		// BYOH label is not set
		labelSelector = fmt.Sprintf("%s,!%s", labelSelector, controllers.BYOHLabel)
	}
	return wait.PollImmediate(retryInterval, 10*time.Minute, func() (done bool, err error) {
		nodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(),
			meta.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, nil
		}
		return len(nodes.Items) == 0, nil
	})
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func (tc *testContext) testWindowsNodeDeletion(t *testing.T) {
	tc.cleanupDeployments()
	// Deploy a DaemonSet and wait until its pods have been made ready on each Windows node. DaemonSet pods cannot be
	// drained from a Node.
	// Doing this ensures that WMCO is able to handle containers being present on the instance when deconfiguring.
	ds, err := tc.deployNOOPDaemonSet()
	require.NoError(t, err, "error creating DaemonSet")
	defer tc.client.K8s.AppsV1().DaemonSets(ds.GetNamespace()).Delete(context.TODO(), ds.GetName(),
		meta.DeleteOptions{})
	err = tc.waitUntilDaemonsetScaled(ds.GetName(), int(gc.numberOfMachineNodes+gc.numberOfBYOHNodes))
	require.NoError(t, err, "error waiting for DaemonSet pods to become ready")

	// set expected node count to zero, since all Windows Nodes should be deleted in the test
	expectedNodeCount := int32(0)
	// Get all the Machines created by the e2e tests
	// For platform=none, the resulting slice is empty
	e2eMachineSets, err := tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).List(context.TODO(),
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
		_, err = tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Update(context.TODO(),
			machineControllerMachineSet, meta.UpdateOptions{})
		require.NoError(t, err, "error updating Windows MachineSet")

		// we are waiting 10 minutes for all windows machines to get deleted.
		err = tc.waitForWindowsNodeRemoval(false)
		require.NoError(t, err, "Windows Machine Node deletion failed")
	}
	t.Run("BYOH node removal", tc.testBYOHRemoval)

	// Cleanup all the MachineSets created by us.
	for _, machineSet := range e2eMachineSets.Items {
		assert.NoError(t, tc.deleteMachineSet(&machineSet), "error deleting MachineSet")
	}
	// Phase is ignored during deletion, in this case we are just waiting for Machines to be deleted.
	_, err = tc.waitForWindowsMachines(int(expectedNodeCount), "", false)
	require.NoError(t, err, "Machine controller Windows machine deletion failed")
	_, err = tc.waitForWindowsMachines(int(expectedNodeCount), "", true)
	require.NoError(t, err, "ConfigMap controller Windows machine deletion failed")

	// Test if prometheus configuration is updated to have no node entries in the endpoints object
	t.Run("Prometheus configuration", tc.testPrometheus)

	// Cleanup windows-instances ConfigMap
	tc.deleteWindowsInstanceConfigMap()

	// Cleanup secrets created by us.
	err = tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Delete(context.TODO(),
		clusterinfo.UserDataSecretName, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete userData secret")

	err = tc.client.K8s.CoreV1().Secrets(wmcoNamespace).Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete privateKey secret")

	// Cleanup wmco-test namespace created by us.
	err = tc.deleteNamespace(tc.workloadNamespace)
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
					OS:          &core.PodOS{Name: core.Windows},
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
		return nil, fmt.Errorf("error creating daemonset %v: %w", ds, err)
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
			return fmt.Errorf("could not get daemonset %s: %w", name, err)
		}
		if int(ds.Status.NumberAvailable) == desiredReplicas {
			return nil
		}
		time.Sleep(retryInterval)
	}
	return fmt.Errorf("timed out waiting for daemonset %s to scale, current status: %+v", name, ds.Status)
}

// cleanupWorkloads attempts to delete all deployments that exist within the testContext workload namespace
func (tc *testContext) cleanupDeployments() {
	deployments, err := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace).List(context.TODO(), meta.ListOptions{})
	if err != nil {
		log.Printf("error getting deployment list: %s", err)
		return
	}
	for _, deployment := range deployments.Items {
		log.Printf("deleting deployment %s", deployment.GetName())
		err := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace).Delete(context.TODO(), deployment.GetName(),
			meta.DeleteOptions{})
		if err != nil {
			log.Printf("error deleting deployment %s: %s", deployment.GetName(), err)
		}
	}
}
