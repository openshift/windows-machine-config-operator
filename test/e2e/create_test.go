package e2e

import (
	"context"
	"log"
	"math"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

func creationTestSuite(t *testing.T) {
	// The order of tests here are important. Any node object related tests should be run only after
	// testWindowsNodeCreation as that initializes the node objects in the global context.
	if !t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t) }) {
		// No point in running the other tests if creation failed
		return
	}
	t.Run("Node Metadata", func(t *testing.T) { testNodeMetadata(t) })
	t.Run("NodeTaint validation", func(t *testing.T) { testNodeTaint(t) })
	t.Run("UserData validation", func(t *testing.T) { testUserData(t) })
	t.Run("UserData idempotent check", func(t *testing.T) { testUserDataTamper(t) })
	t.Run("Node Logs", func(t *testing.T) { testNodeLogs(t) })
	t.Run("Metrics validation", testMetrics)
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	// Create a private key secret with the known private key.
	require.NoError(t, testCtx.createPrivateKeySecret(true), "could not create known private key secret")

	t.Run("Windows Machines without the Windows label are not configured", func(t *testing.T) {
		// Test is platform agnostic so is not needed to be run for every supported platform.
		if testCtx.CloudProvider.GetType() != config.AzurePlatformType {
			t.Skipf("Skipping for %s", testCtx.CloudProvider.GetType())
		}
		ms, err := testCtx.createWindowsMachineSet(1, false)
		require.NoError(t, err, "failed to create Windows MachineSet")
		defer testCtx.deleteMachineSet(ms)
		err = testCtx.waitForWindowsNodes(1, true, true, false)
		assert.Error(t, err, "Windows node creation failed")
	})

	t.Run("Windows Machines with the Windows label are configured", func(t *testing.T) {
		_, err := testCtx.createWindowsMachineSet(gc.numberOfNodes, true)
		require.NoError(t, err, "failed to create Windows MachineSet")
		// We need to cover the case where a user changes the private key secret before the WMCO has a chance to
		// configure the Machine. In order to simulate that case we need to wait for the MachineSet to be fully
		// provisioned and then change the key. The the correct amount of nodes being configured is proof that the
		// mismatched Machine created with the mismatched key was deleted and replaced.
		// Depending on timing and configuration flakes this will either cause all Machines, or all Machines after
		// the first configured Machines to hit this scenario. This is a platform agonistic test so we run it only on
		// AWS.
		err = testCtx.waitForWindowsMachines(int(gc.numberOfNodes), "Provisioned")
		require.NoError(t, err, "error waiting for Windows Machines to be provisioned")
		if testCtx.CloudProvider.GetType() == config.AWSPlatformType {
			// Replace the known private key with a randomly generated one.
			err = testCtx.createPrivateKeySecret(false)
			require.NoError(t, err, "error replacing private key secret")
		}
		err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, false, false)
		assert.NoError(t, err, "Windows node creation failed")

		t.Run("Changing the private key ensures all Windows nodes use the new private key", func(t *testing.T) {
			// This test cannot be run on vSphere because this random key is not part of the vSphere template image.
			// Moreover this test is platform agnostic so is not needed to be run for every supported platform.
			if testCtx.CloudProvider.GetType() != config.AWSPlatformType {
				t.Skipf("Skipping for %s", testCtx.CloudProvider.GetType())
			}
			// Replace private key and check that new Machines are created using the new private key
			err = testCtx.createPrivateKeySecret(false)
			require.NoError(t, err, "error replacing private key secret")
			err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, false, false)
			assert.NoError(t, err, "error waiting for Windows nodes configured with newly created private key")
		})
	})
}

// createWindowsMachineSet creates given number of Windows Machines.
func (tc *testContext) createWindowsMachineSet(replicas int32, windowsLabel bool) (*mapi.MachineSet, error) {
	machineSet, err := tc.CloudProvider.GenerateMachineSet(windowsLabel, replicas)
	if err != nil {
		return nil, err
	}
	return tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Create(context.TODO(), machineSet, metav1.CreateOptions{})
}

// deleteMachineSet deletes the MachineSet passed to it
func (tc *testContext) deleteMachineSet(ms *mapi.MachineSet) error {
	return tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Delete(context.TODO(), ms.GetName(),
		metav1.DeleteOptions{})
}

// waitForWindowsMachines waits for a certain amount of Windows Machines to reach a certain phase
// if machineCount = 0, it implies we are only waiting for Machines to be deleted and the phase is
// ignored in this case.
func (tc *testContext) waitForWindowsMachines(machineCount int, phase string) error {
	if machineCount == 0 && phase != "" {
		return errors.New("expected phase to be to be an empty string if machineCount is 0")
	}

	var machines *mapi.MachineList
	machineStateTimeLimit := time.Minute * 5
	startTime := time.Now()
	// Increasing the time limit due to https://bugzilla.redhat.com/show_bug.cgi?id=1936556
	if tc.CloudProvider.GetType() == config.VSpherePlatformType {
		// When deleting Machines, set the time limit to 10 minutes
		if machineCount == 0 {
			machineStateTimeLimit = time.Minute * 10
		} else {
			machineStateTimeLimit = time.Minute * 20
		}
	}
	err := wait.Poll(retryInterval, machineStateTimeLimit, func() (done bool, err error) {
		machines, err = tc.client.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), metav1.ListOptions{
			LabelSelector: controllers.MachineOSLabel + "=Windows"})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("waiting for %d Windows Machines", machineCount)
				return false, nil
			}
			log.Printf("machine object listing failed: %v", err)
			return false, nil
		}
		if len(machines.Items) != machineCount {
			log.Printf("waiting for %d/%d Windows Machines", machineCount-len(machines.Items), machineCount)
			return false, nil
		}
		// A phase of "" skips the phase check
		if phase == "" {
			return true, nil
		}
		for _, machine := range machines.Items {
			if machine.Status.Phase == nil || *machine.Status.Phase != phase {
				return false, nil
			}
		}
		return true, nil
	})
	if phase == "" {
		phase = "deleted"
	}

	// Log the time elapsed while waiting for creation of the Machines
	endTime := time.Now()
	log.Printf("%v time is required for %d Machines to reach phase %s", endTime.Sub(startTime), len(machines.Items), phase)
	return err
}

// waitForWindowsNode waits until there exists nodeCount Windows nodes with the correct set of annotations.
// if waitForAnnotations = false, the function will return when the node object is first seen and not wait until
// the expected annotations are present.
// if expectError = true, the function will wait for duration of 10 minutes if we are deleting all nodes i.e. 0 nodesCount
// else 5 minutes for the nodes as the error would be thrown immediately, else we will wait for the duration given by
// nodeCreationTime variable which is 20 minutes increasing the overall wait time in test suite
func (tc *testContext) waitForWindowsNodes(nodeCount int32, waitForAnnotations, expectError, checkVersion bool) error {
	var nodes *v1.NodeList
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac, nodeconfig.VersionAnnotation,
		nodeconfig.PubKeyHashAnnotation}
	var creationTime time.Duration
	startTime := time.Now()
	if expectError {
		if nodeCount == 0 {
			creationTime = time.Minute * 10
		} else {
			// The time we expect to wait, if the windowsLabel is
			// not used while creating nodes.
			creationTime = time.Minute * 5
		}
	} else {
		creationTime = nodeCreationTime
	}

	pubKey, err := tc.getExpectedPublicKey()
	if err != nil {
		return errors.Wrap(err, "error getting the expected public key")
	}
	pubKeyAnnotation := nodeconfig.CreatePubKeyHashAnnotation(pubKey)

	// We are waiting 20 minutes for each windows VM to be shown up in the cluster. The value comes from
	// nodeCreationTime variable.  If we are testing a scale down from n nodes to 0, then we should
	// not take the number of nodes into account. If we are testing node creation without applying Windows label, we
	// should throw error within 5 mins.
	err = wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(nodeCount), 1))*creationTime, func() (done bool, err error) {
		nodes, err = tc.client.K8s.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("waiting for %d Windows nodes", gc.numberOfNodes)
				return false, nil
			}
			log.Printf("node object listing failed: %v", err)
			return false, nil
		}
		if len(nodes.Items) != int(nodeCount) {
			log.Printf("waiting for %d/%d Windows nodes", len(nodes.Items), gc.numberOfNodes)
			return false, nil
		}
		if !waitForAnnotations {
			return true, nil
		}
		// Wait for annotations to be present on the node objects in the scale up caseoc
		if nodeCount != 0 {
			log.Printf("waiting for annotations to be present on %d Windows nodes", nodeCount)
		}
		for _, node := range nodes.Items {
			// check node status
			readyCondition := false
			for _, condition := range node.Status.Conditions {
				if condition.Type == v1.NodeReady {
					readyCondition = true
				}
				if readyCondition && condition.Status != v1.ConditionTrue {
					log.Printf("node %v is expected to be in Ready state", node.Name)
					return false, nil
				}
			}
			if !readyCondition {
				log.Printf("expected node Status to have condition type Ready for node %v", node.Name)
				return false, nil
			}

			for _, annotation := range annotations {
				_, found := node.Annotations[annotation]
				if !found {
					log.Printf("node %s does not have annotation: %s", node.GetName(), annotation)
					return false, nil
				}
			}
			if checkVersion {
				operatorVersion, err := getWMCOVersion()
				if err != nil {
					log.Printf("error getting operator version : %v", err)
					return false, nil
				}
				if node.Annotations[nodeconfig.VersionAnnotation] != operatorVersion {
					log.Printf("node %s has mismatched version annotation %s. expected: %s", node.GetName(),
						node.Annotations[nodeconfig.VersionAnnotation], operatorVersion)
					return false, nil
				}
			}
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] != pubKeyAnnotation {
				log.Printf("node %s has mismatched pubkey annotation value %s expected: %s", node.GetName(),
					node.Annotations[nodeconfig.PubKeyHashAnnotation], pubKeyAnnotation)
				return false, nil
			}
		}

		return true, nil
	})

	// Initialize/update nodes to avoid staleness
	gc.nodes = nodes.Items
	// Log the time elapsed while waiting for creation of the nodes
	endTime := time.Now()
	log.Printf("%v time is required to configure %v nodes", endTime.Sub(startTime), gc.numberOfNodes)

	return err
}
