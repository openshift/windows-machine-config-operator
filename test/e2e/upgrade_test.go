package e2e

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

const (
	// deploymentRetryInterval is the retry time for WMCO deployment to scale up/down
	deploymentRetryInterval = time.Second * 10
	// deploymentTimeout is the maximum duration to update WMCO deployment
	deploymentTimeout = time.Minute * 1
	// resourceName is the name of a resource in the watched namespace (e.g pod name, deployment name)
	resourceName = "windows-machine-config-operator"
	// parallelUpgradesCheckerJobName is a fixed name for the job that checks for the number of parallel upgrades
	parallelUpgradesCheckerJobName = "parallel-upgrades-checker"
)

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

// TestUpgrade tests that things are functioning properly after an upgrade
func TestUpgrade(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	// In the case that upgrading a Machine node require the deletion of the VM, bootstrap CSRs will need to be approved
	// Ensure the machine approver is scaled, as it may not be depending on the order of tests ran
	err = tc.scaleMachineApprover(1)
	require.NoError(t, err)

	// Attempt to collect all Node logs if tests fail
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		log.Printf("test failed, attempting to gather Node logs")
		tc.collectWindowsInstanceLogs()
	})

	err = tc.waitForConfiguredWindowsNodes(int32(numberOfMachineNodes), true, false)
	assert.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(int32(numberOfBYOHNodes), true, true)
	assert.NoError(t, err, "timed out waiting for BYOH Windows nodes")

	// Basic testing to ensure the Node object is in a good state
	t.Run("Nodes ready", tc.testNodesBecomeReadyAndSchedulable)
	t.Run("Node annotations", tc.testNodeAnnotations)
	t.Run("Node Metadata", tc.testNodeMetadata)

	// test that any workloads deployed on the node have not been broken by the upgrade
	t.Run("Workloads ready", tc.testWorkloadsAvailable)
	t.Run("Node Logs", tc.testNodeLogs)
	t.Run("Parallel Upgrades Checker", tc.testParallelUpgradesChecker)

}

// testParallelUpgradesChecker tests that the number of parallel upgrades does not exceed the max allowed
// in the lifetime of the job execution. This test is run after the upgrade is complete.
func (tc *testContext) testParallelUpgradesChecker(t *testing.T) {
	// get current Windows node state
	require.NoError(t, tc.loadExistingNodes(), "error getting the current Windows nodes in the cluster")
	if len(gc.allNodes()) < 2 {
		t.Skipf("Requires 2 or more nodes to run. Found %d nodes", len(gc.allNodes()))
	}
	failedPods, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: "job-name=" + parallelUpgradesCheckerJobName, FieldSelector: "status.phase=Failed"})
	require.NoError(t, err)
	_, err = tc.gatherPodLogs("job-name="+parallelUpgradesCheckerJobName, false)
	if err != nil {
		log.Printf("unable to gather logs: %v", err)
	}
	require.Equal(t, 0, len(failedPods.Items), "parallel upgrades check failed",
		"failed pod count", len(failedPods.Items))
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
