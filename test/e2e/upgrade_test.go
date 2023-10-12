package e2e

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/vsphere"
)

const (
	// deploymentRetryInterval is the retry time for WMCO deployment to scale up/down
	deploymentRetryInterval = time.Second * 10
	// deploymentTimeout is the maximum duration to update WMCO deployment
	deploymentTimeout = time.Minute * 1
	// resourceName is the name of a resource in the watched namespace (e.g pod name, deployment name)
	resourceName = "windows-machine-config-operator"
	// windowsWorkloadTesterJob is the name of the job created to test Windows workloads
	windowsWorkloadTesterJob = "windows-workload-tester"
	// outdatedVersion is the 'previous' version in the simulated upgrade that the operator is being upgraded from
	outdatedVersion = "old-version"
)

// upgradeTestSuite tests behaviour of the operator when an upgrade takes place.
func upgradeTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	// test if Windows workloads are running by creating a Job that curls the workloads continuously.
	cleanupWorkloadAndTester, err := tc.deployWindowsWorkloadAndTester()
	require.NoError(t, err, "error deploying Windows workloads")
	defer cleanupWorkloadAndTester()

	// apply configuration steps before running the upgrade tests
	err = tc.configureUpgradeTest()
	require.NoError(t, err, "error configuring upgrade")

	// get current Windows node state
	// TODO: waitForConfiguredWindowsNodes currently loads nodes into global context, so we need this (even though BYOH
	// 		 nodes are not being upgraded/tested here). Remove as part of https://issues.redhat.com/browse/WINC-620
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, true, false)
	require.NoError(t, err, "wrong number of Machine controller nodes found")
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, true, true)
	require.NoError(t, err, "wrong number of ConfigMap controller nodes found")

	t.Run("Operator version upgrade", tc.testUpgradeVersion)
}

// testUpgradeVersion tests the upgrade scenario of the operator. The node version annotation is changed when
// the operator is shut-down. The function tests if the operator on restart deletes the machines and recreates
// them on version annotation mismatch.
func (tc *testContext) testUpgradeVersion(t *testing.T) {
	// Test the node metadata and if the version annotation corresponds to the current operator version
	tc.testNodeMetadata(t)
	// Test if prometheus is reconfigured with ip addresses of newly configured nodes
	tc.testPrometheus(t)

	// Ensure outdated ConfigMap is not retrievable
	t.Run("Outdated services ConfigMap removal", func(t *testing.T) {
		err := tc.waitForServicesConfigMapDeletion(servicescm.NamePrefix + outdatedVersion)
		assert.NoError(t, err, "failed to ensure outdated services ConfigMap is removed after operator upgrade")
	})

	// TODO: Fix matching label for jobs. See https://issues.redhat.com/browse/WINC-673
	// Test if there was any downtime for Windows workloads by checking the failure on the Job pods.
	pods, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).List(context.TODO(),
		metav1.ListOptions{FieldSelector: "status.phase=Failed",
			LabelSelector: "job-name=" + windowsWorkloadTesterJob + "-job"})

	require.NoError(t, err)
	require.Equal(t, 0, len(pods.Items), "Windows workloads inaccessible for significant amount of time during upgrade")

}

// configureUpgradeTest carries out steps required before running tests for upgrade scenario.
// The steps include -
// 1. Scale down the operator to 0.
// 2. Change Windows node version annotation to an invalid value
// 3. Create a services ConfigMap tied to an outdated operator version
// 4. Scale up the operator to 1
func (tc *testContext) configureUpgradeTest() error {
	// Scale down the WMCO deployment to 0
	if err := tc.scaleWMCODeployment(0); err != nil {
		return err
	}

	// tamper version annotation on all nodes
	machineNodes, err := tc.listFullyConfiguredWindowsNodes(false)
	if err != nil {
		return fmt.Errorf("error getting list of fully configured Machine nodes: %w", err)
	}
	byohNodes, err := tc.listFullyConfiguredWindowsNodes(true)
	if err != nil {
		return fmt.Errorf("error getting list of fully configured BYOH nodes: %w", err)
	}

	for _, node := range append(machineNodes, byohNodes...) {
		patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, metadata.VersionAnnotation, outdatedVersion)
		node, err := tc.client.K8s.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType,
			[]byte(patchData), metav1.PatchOptions{})
		if err != nil {
			return err
		}
		log.Printf("Node Annotation changed to %v", node.Annotations[metadata.VersionAnnotation])
	}

	// Create outdated services ConfigMap
	outdatedServicesCM := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      servicescm.NamePrefix + outdatedVersion,
			Namespace: wmcoNamespace,
		},
		Data: map[string]string{"services": "[]", "files": "[]"},
	}
	if _, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Create(context.TODO(), outdatedServicesCM,
		metav1.CreateOptions{}); err != nil {
		return err
	}

	// Scale up the WMCO deployment to 1
	if err := tc.scaleWMCODeployment(1); err != nil {
		return err
	}
	return nil
}

// scaleWMCODeployment scales the WMCO operator to the given replicas. If the deployment is managed by OLM, updating the
// replicas only scales the deployment to 0 or 1. If we want to scale the deployment to more than 1 replicas, we need to
// make changes in replicas defined in the corresponding CSV.
func (tc *testContext) scaleWMCODeployment(desiredReplicas int32) error {
	// update the windows-machine-config-operator deployment to the desired replicas - 0 or 1
	err := wait.Poll(deploymentRetryInterval, deploymentTimeout, func() (done bool, err error) {

		patchData := fmt.Sprintf(`{"spec":{"replicas":%v}}`, desiredReplicas)

		_, err = tc.client.K8s.AppsV1().Deployments(wmcoNamespace).Patch(context.TODO(), resourceName,
			types.MergePatchType, []byte(patchData), metav1.PatchOptions{})
		if err != nil {
			log.Printf("error patching operator deployment : %v", err)
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return err
	}

	// wait for the windows-machine-config-operator to scale up/down
	err = wait.Poll(deploymentRetryInterval, deploymentTimeout, func() (done bool, err error) {
		deployment, err := tc.client.K8s.AppsV1().Deployments(wmcoNamespace).Get(context.TODO(), resourceName,
			metav1.GetOptions{})
		if err != nil {
			log.Printf("error getting operator deployment: %v", err)
			return false, nil
		}
		return deployment.Status.ReadyReplicas == desiredReplicas, nil
	})

	return err
}

// deployWindowsWorkloadAndTester tests if the Windows Webserver deployment is available.
// This is achieved by creating a Job object that continuously curls the webserver every 5 seconds.
// returns a tearDown func that must be executed to cleanup resources
func (tc *testContext) deployWindowsWorkloadAndTester() (func(), error) {
	// create a Windows Webserver deployment
	deployment, err := tc.deployWindowsWebServer("win-webserver", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating Windows Webserver deployment for upgrade test: %w", err)
	}
	// create a clusterIP service which can be used to reach the Windows webserver
	intermediarySVC, err := tc.createService(deployment.Name, v1.ServiceTypeClusterIP, *deployment.Spec.Selector)
	if err != nil {
		_ = tc.deleteDeployment(deployment.Name)
		return nil, fmt.Errorf("error creating service for deployment %s: %w", deployment.Name, err)
	}
	// create a Job object that continuously curls the webserver every 5 seconds.
	testerJob, err := tc.createLinuxCurlerJob(windowsWorkloadTesterJob, intermediarySVC.Spec.ClusterIP, true)
	if err != nil {
		_ = tc.deleteDeployment(deployment.Name)
		_ = tc.deleteService(intermediarySVC.Name)
		return nil, fmt.Errorf("error creating linux job %s: %w", windowsWorkloadTesterJob, err)
	}
	// return a cleanup func
	return func() {
		// collect webserver pods logs
		tc.collectDeploymentLogs(deployment)
		tc.writePodLogs("job-name=" + testerJob.Name)
		// ignore errors while deleting the objects
		_ = tc.deleteDeployment(deployment.Name)
		_ = tc.deleteService(intermediarySVC.Name)
		_ = tc.deleteJob(testerJob.Name)
	}, nil
}

// TestUpgrade tests that things are functioning properly after an upgrade
func TestUpgrade(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	err = tc.waitForConfiguredWindowsNodes(int32(numberOfMachineNodes), false, false)
	assert.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(int32(numberOfBYOHNodes), false, true)
	assert.NoError(t, err, "timed out waiting for BYOH Windows nodes")

	// Basic testing to ensure the Node object is in a good state
	t.Run("Nodes ready", tc.testNodesBecomeReadyAndSchedulable)
	t.Run("Node annotations", tc.testNodeAnnotations)
	t.Run("Node Metadata", tc.testNodeMetadata)

	if inTreeUpgrade {
		// Deploy the CSI drivers and wait for a CSI node to be created
		// This is a requirement for storage workloads to go back to ready
		vsphereProvider, ok := tc.CloudProvider.(*vsphere.Provider)
		require.True(t, ok, "in tree upgrade must be ran on vSphere")
		require.NoError(t, vsphereProvider.EnsureWindowsCSIDrivers(tc.client.K8s))
		log.Printf("waiting for csinodes to reflect driver deployment")
		err = tc.waitForCSINodesWithDrivers(gc.allNodes())
		require.NoError(t, err)
	}

	// test that any workloads deployed on the node have not been broken by the upgrade
	t.Run("Workloads ready", tc.testWorkloadsAvailable)
	t.Run("Node Logs", tc.testNodeLogs)
}

// testWorkloadsAvailable tests that all workloads deployed on Windows nodes by the test suite are available
func (tc *testContext) testWorkloadsAvailable(t *testing.T) {
	err := wait.PollImmediateWithContext(context.TODO(), retry.Interval, retry.Timeout,
		func(ctx context.Context) (bool, error) {
			deployments, err := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				log.Printf("error getting deployment list: %s", err)
				return false, nil
			}
			for _, deployment := range deployments.Items {
				if deployment.Spec.Replicas == nil ||
					(*deployment.Spec.Replicas != deployment.Status.AvailableReplicas) {
					log.Printf("waiting for %s to become available", deployment.GetName())
					return false, nil
				}
			}
			return true, nil
		})
	assert.NoError(t, err)
}

// waitForCSINodes waits for a CSINode to exist for each node with a driver loaded, indicating CSI functionality has
// been enabled for the given node.
func (tc *testContext) waitForCSINodesWithDrivers(nodes []v1.Node) error {
	return wait.PollImmediate(retry.Interval, 10*time.Minute, func() (bool, error) {
		csinodes, err := tc.client.K8s.StorageV1().CSINodes().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Printf("error listing CSINodes: %s", err)
			return false, nil
		}
		for _, node := range nodes {
			var ready bool
			for _, csinode := range csinodes.Items {
				if csinode.GetName() == node.GetName() {
					if len(csinode.Spec.Drivers) != 1 {
						log.Printf("CSINode for %s is missing driver", node.GetName())
						break
					}
					ready = true
					break
				}
			}
			if !ready {
				return false, nil
			}
		}
		return true, nil
	})
}
