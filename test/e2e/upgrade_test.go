package e2e

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	nc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/nodeconfig"
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
)

// upgradeTestSuite tests behaviour of the operator when an upgrade takes place.
func upgradeTestSuite(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// apply configuration steps before running the upgrade tests
	err = testCtx.configureUpgradeTest()
	require.NoError(t, err, "error configuring upgrade")

	t.Run("Operator version upgrade", testUpgradeVersion)
	t.Run("Version annotation tampering", testTamperAnnotation)
}

// testUpgradeVersion tests the upgrade scenario of the operator. The node version annotation is changed when
// the operator is shut-down. The function tests if the operator on restart deletes the machines and recreates
// them on version annotation mismatch.
func testUpgradeVersion(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, false, true)
	require.NoError(t, err, "windows node upgrade failed")
	// Test if the version annotation corresponds to the current operator version
	testVersionAnnotation(t)
}

// testTamperAnnotation tests if the operator deletes machines and recreates them, if the node annotation is changed to an invalid value
// with the expected annotation when the operator is in running state
func testTamperAnnotation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// tamper node annotation
	nodes, err := testCtx.kubeclient.CoreV1().Nodes().List(context.TODO(),
		metav1.ListOptions{LabelSelector: nc.WindowsOSLabel})
	require.NoError(t, err)

	for _, node := range nodes.Items {
		patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, nc.VersionAnnotation, "badVersion")
		_, err := testCtx.kubeclient.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, []byte(patchData), metav1.PatchOptions{})
		require.NoError(t, err)
		if err == nil {
			break
		}
	}

	err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, false, true)
	require.NoError(t, err, "windows node upgrade failed")
	// Test if the version annotation corresponds to the current operator version
	testVersionAnnotation(t)
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

	// Override the Windows Node Version Annotation
	nodes, err := tc.kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nc.WindowsOSLabel})
	if err != nil {
		return err
	}
	if len(nodes.Items) != int(gc.numberOfNodes) {
		return errors.Wrapf(nil, "unexpected number of nodes %v", gc.numberOfNodes)
	}

	for _, node := range nodes.Items {
		patchData := fmt.Sprintf(`{"metadata":{"annotations":{"%s":"%s"}}}`, nc.VersionAnnotation, "badVersion")
		_, err := tc.kubeclient.CoreV1().Nodes().Patch(context.TODO(), node.Name, types.MergePatchType, []byte(patchData), metav1.PatchOptions{})
		if err != nil {
			return err
		}
		log.Printf("Node Annotation changed to %v", node.Annotations[nc.VersionAnnotation])
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

		_, err = tc.kubeclient.AppsV1().Deployments(resourceNamespace).Patch(context.TODO(), resourceName,
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
		deployment, err := tc.kubeclient.AppsV1().Deployments(resourceNamespace).Get(context.TODO(), resourceName,
			metav1.GetOptions{})
		if err != nil {
			log.Printf("error getting operator deployment: %v", err)
			return false, nil
		}
		return deployment.Status.ReadyReplicas == desiredReplicas, nil
	})

	return err
}
