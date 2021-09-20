package e2e

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
)

const (
	// deploymentRetryInterval is the retry time for WMCO deployment to scale up/down
	deploymentRetryInterval = time.Second * 10
	// deploymentTimeout is the maximum duration to update WMCO deployment
	deploymentTimeout = time.Minute * 1
	// resourceName is the name of a resource in the watched namespace (e.g pod name, deployment name)
	resourceName = "windows-machine-config-operator"
	// resourceNamespace is the namespace the resources are deployed in
	resourceNamespace = "openshift-windows-machine-config-operator"
	// windowsWorkloadTesterJob is the name of the job created to test Windows workloads
	windowsWorkloadTesterJob = "windows-workload-tester"
)

// getNodeTypeAffinities generate node affinity for non-BYOH and BYOH nodes if present
func getNodeTypeAffinities() ([]*v1.Affinity, error) {
	var affinities []*v1.Affinity
	if gc.numberOfMachineNodes >= 1 {
		affinityNoByoh, err := getNodeAffinityForLabel(v1.NodeSelectorOpDoesNotExist, controllers.BYOHLabel)
		if err != nil {
			return nil, errors.Wrapf(err, "error creating node affinity without BYOH label")
		}
		affinities = append(affinities, affinityNoByoh)
	}
	if gc.numberOfBYOHNodes >= 1 {
		affinityByoh, err := getNodeAffinityForLabel(v1.NodeSelectorOpIn, controllers.BYOHLabel, "true")
		if err != nil {
			return nil, errors.Wrapf(err, "error creating node affinity for BYOH label")
		}
		affinities = append(affinities, affinityByoh)
	}
	return affinities, nil
}

// generateLinuxWorkloadTesterCommand creates a Job to curl Windows webserver every 5 seconds.
// If the webserver is not accessible for few minutes Curl Windows webserver
// exit the pod with an error.
func generateLinuxWorkloadTesterCommand(clusterIP string) []string {
	return []string{
		"bash",
		"-c",
		" while true;" +
			" do curl " + clusterIP + ";" +
			" if [ $? != 0 ]; then" +
			" sleep 60;" +
			" curl " + clusterIP + " || exit 1;" +
			" fi; " +
			"sleep 5; " +
			"done"}
}

// upgradeTestSuite tests behaviour of the operator when an upgrade takes place.
func upgradeTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// generate node affinities
	affinities, err := getNodeTypeAffinities()
	require.NoError(t, err, "error getting node affinity")
	// loop affinities to ensure deployed workloads will exclusively run based
	// on the given affinity, e.g.  BYOH and/or Machine nodes
	for _, affinity := range affinities {
		// test if Windows workloads are running by creating a Job that curls the workloads continuously.
		deployment, err := testCtx.deployWindowsWebServer("win-webserver", affinity)
		require.NoErrorf(t, err, "error creating Windows Webserver deployment for upgrade test")
		defer testCtx.deleteDeployment(deployment.Name)

		// create a clusterIP service which can be used to reach the Windows webserver
		intermediarySVC, err := testCtx.createService(deployment.Name, v1.ServiceTypeClusterIP, *deployment.Spec.Selector)
		require.NoErrorf(t, err, "error creating service for deployment %s", deployment.Name)
		defer testCtx.deleteService(intermediarySVC.Name)

		// create a Job object that continuously curls the webserver every 5 seconds.
		testerJob, err := testCtx.createLinuxJob(windowsWorkloadTesterJob, generateLinuxWorkloadTesterCommand(intermediarySVC.Spec.ClusterIP))
		require.NoErrorf(t, err, "error creating linux job %s", windowsWorkloadTesterJob)
		defer testCtx.deleteJob(testerJob.Name)
	}

	// apply configuration steps before running the upgrade tests
	err = testCtx.configureUpgradeTest()
	require.NoError(t, err, "error configuring upgrade")

	// get current Windows node state
	// TODO: waitForWindowsNodes currently loads nodes into global context, so we need this (even though BYOH
	// 		 nodes are not being upgraded/tested here). Remove as part of https://issues.redhat.com/browse/WINC-620
	err = testCtx.waitForWindowsNodes(gc.numberOfMachineNodes, false, true, false)
	require.NoError(t, err, "wrong number of Machine controller nodes found")
	err = testCtx.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	require.NoError(t, err, "wrong number of ConfigMap controller nodes found")

	t.Run("Operator version upgrade", testUpgradeVersion)
}

// testUpgradeVersion tests the upgrade scenario of the operator. The node version annotation is changed when
// the operator is shut-down. The function tests if the operator on restart deletes the machines and recreates
// them on version annotation mismatch.
func testUpgradeVersion(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// Test the node metadata and if the version annotation corresponds to the current operator version
	testNodeMetadata(t)
	// Test if prometheus is reconfigured with ip addresses of newly configured nodes
	testPrometheus(t)

	// TODO: Fix matching label for jobs. See https://issues.redhat.com/browse/WINC-673
	// Test if there was any downtime for Windows workloads by checking the failure on the Job pods.
	pods, err := testCtx.client.K8s.CoreV1().Pods(testCtx.workloadNamespace).List(context.TODO(),
		metav1.ListOptions{FieldSelector: "status.phase=Failed",
			LabelSelector: "job-name=" + windowsWorkloadTesterJob + "-job"})

	require.NoError(t, err)
	require.Equal(t, 0, len(pods.Items), "Windows workloads inaccessible for significant amount of time during upgrade")

}

// configureUpgradeTest carries out steps required before running tests for upgrade scenario.
// The steps include -
// 1. Scale down the operator to 0.
// 2. Change Windows node version annotation to an invalid value
// 3. Scale up the operator to 1
func (tc *testContext) configureUpgradeTest() error {
	// Scale down the WMCO deployment to 0
	if err := tc.scaleWMCODeployment(0); err != nil {
		return err
	}

	// tamper version annotation on all nodes
	machineNodes, err := tc.listFullyConfiguredWindowsNodes(false)
	if err != nil {
		return errors.Wrap(err, "error getting list of fully configured Machine nodes")
	}
	byohNodes, err := tc.listFullyConfiguredWindowsNodes(true)
	if err != nil {
		return errors.Wrap(err, "error getting list of fully configured BYOH nodes")
	}

	for _, node := range append(machineNodes, byohNodes...) {
		patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, metadata.VersionAnnotation, "badVersion")
		_, err := tc.client.K8s.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType,
			[]byte(patchData), metav1.PatchOptions{})
		if err != nil {
			return err
		}
		log.Printf("Node Annotation changed to %v", node.Annotations[metadata.VersionAnnotation])
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

		_, err = tc.client.K8s.AppsV1().Deployments(resourceNamespace).Patch(context.TODO(), resourceName,
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
		deployment, err := tc.client.K8s.AppsV1().Deployments(resourceNamespace).Get(context.TODO(), resourceName,
			metav1.GetOptions{})
		if err != nil {
			log.Printf("error getting operator deployment: %v", err)
			return false, nil
		}
		return deployment.Status.ReadyReplicas == desiredReplicas, nil
	})

	return err
}
