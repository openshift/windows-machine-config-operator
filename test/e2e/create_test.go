package e2e

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

func creationTestSuite(t *testing.T) {
	// The order of tests here are important. Any node object related tests should be run only after
	// testWindowsNodeCreation as that initializes the node objects in the global context.
	if !t.Run("Creation", testWindowsNodeCreation) {
		// No point in running the other tests if creation failed
		return
	}
	t.Run("Node Metadata", testNodeMetadata)
	t.Run("Services running", testExpectedServicesRunning)
	t.Run("NodeTaint validation", testNodeTaint)
	t.Run("CSR Validation", func(t *testing.T) { testCSRApproval(t) })
	t.Run("UserData validation", testUserData)
	t.Run("UserData idempotent check", testUserDataTamper)
	t.Run("Node Logs", testNodeLogs)
	t.Run("Metrics validation", testMetrics)
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	// Create a private key secret with the known private key.
	require.NoError(t, testCtx.createPrivateKeySecret(true), "could not create known private key secret")

	t.Run("Machine controller", testCtx.testMachineConfiguration)
	// BYOH creation must occur after the Machine creation, as the MachineConfiguration tests change the private key
	// multiple times, and BYOH doesnt have the functionality of rotating keys on the VMs. This would result in BYOH
	// failing the pub key annotation validation as it compares the current private key secret with the annotation.
	// TODO: Remove this dependency by rotating keys as part of https://issues.redhat.com/browse/WINC-655
	t.Run("ConfigMap controller", testCtx.testBYOHConfiguration)

}

// deleteWindowsInstanceConfigMap deletes the windows-instances configmap if it exists
func (tc *testContext) deleteWindowsInstanceConfigMap() error {
	err := tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Delete(context.TODO(), wiparser.InstanceConfigMap,
		metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// createWindowsInstanceConfigMap creates a ConfigMap for the ConfigMap controller to act on, comprised of the Machines
// in the given MachineList
func (tc *testContext) createWindowsInstanceConfigMap(machines *mapi.MachineList) error {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: wiparser.InstanceConfigMap,
		},
		Data: make(map[string]string),
	}
	for _, machine := range machines.Items {
		addr, err := controllers.GetAddress(machine.Status.Addresses)
		if err != nil {
			return errors.Wrap(err, "unable to get usable address")
		}
		cm.Data[addr] = "username=" + tc.vmUsername()
	}
	_, err := tc.client.K8s.CoreV1().ConfigMaps(tc.namespace).Create(context.TODO(), cm, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "unable to create configmap")
	}
	return nil
}

// testMachineConfiguration tests that the Windows Machine controller can properly configure Machines
func (tc *testContext) testMachineConfiguration(t *testing.T) {
	if gc.numberOfMachineNodes == 0 {
		t.Skip("Machine Controller testing disabled")
	}
	_, err := tc.createWindowsMachineSet(gc.numberOfMachineNodes, true)
	require.NoError(t, err, "failed to create Windows MachineSet")
	// We need to cover the case where a user changes the private key secret before the WMCO has a chance to
	// configure the Machine. In order to simulate that case we need to wait for the MachineSet to be fully
	// provisioned and then change the key. The correct amount of nodes being configured is proof that the
	// mismatched Machine created with the mismatched key was deleted and replaced.
	// Depending on timing and configuration flakes this will either cause all Machines, or all Machines after
	// the first configured Machines to hit this scenario. This is a platform agonistic test so we run it only on
	// Azure.
	_, err = tc.waitForWindowsMachines(int(gc.numberOfMachineNodes), "Provisioned", true)
	require.NoError(t, err, "error waiting for Windows Machines to be provisioned")
	if tc.CloudProvider.GetType() == config.AzurePlatformType {
		// Replace the known private key with a randomly generated one.
		err = tc.createPrivateKeySecret(false)
		require.NoError(t, err, "error replacing private key secret")
	}
	err = tc.waitForWindowsNodes(gc.numberOfMachineNodes, false, false, false)
	assert.NoError(t, err, "Windows node creation failed")
}

// testBYOHConfiguration tests that the ConfigMap controller can properly configure VMs
func (tc *testContext) testBYOHConfiguration(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH testing disabled")
	}

	// Patch the CVO with overrides spec value for cluster-machine-approver deployment
	// Doing so, stops CVO from creating/updating its deployment hereafter.
	patchData := `[{"op":"add","path":"/spec/overrides","value":[{"kind":"Deployment","group":"apps/v1","name":"machine-approver","namespace":"openshift-cluster-machine-approver","unmanaged":true}]}]`
	_, err := tc.client.Config.ConfigV1().ClusterVersions().Patch(context.TODO(), "version", types.JSONPatchType, []byte(patchData), metav1.PatchOptions{})

	// Scale the Cluster Machine Approver Deployment to 0
	// This is required for testing BYOH CSR approval feature so that BYOH instances
	// CSR's are not approved by Cluster Machine Approver
	expectedPodCount := int32(0)
	err = tc.scaleMachineApproverDeployment(&expectedPodCount)
	require.NoError(t, err, "failed to scale Machine Approver pods")

	_, err = tc.createWindowsMachineSet(gc.numberOfBYOHNodes, false)
	require.NoError(t, err, "failed to create Windows MachineSet")
	machines, err := tc.waitForWindowsMachines(int(gc.numberOfBYOHNodes), "Provisioned", false)
	require.NoError(t, err, "Machines did not reach expected state")
	t.Run("VM is configured by ConfigMap controller", func(t *testing.T) {
		err = tc.createWindowsInstanceConfigMap(machines)
		require.NoError(t, err, "error creating ConfigMap")
		err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes, false, false, true)
		assert.NoError(t, err, "Windows node creation failed")
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
// ignored in this case. Returns the set of Machines that matched the provided windowsLabel criteria.
// TODO: Have this function take in a list of Windows Machines to wait for https://issues.redhat.com/browse/WINC-620
func (tc *testContext) waitForWindowsMachines(machineCount int, phase string, windowsLabel bool) (*mapi.MachineList, error) {
	if machineCount == 0 && phase != "" {
		return nil, errors.New("expected phase to be to be an empty string if machineCount is 0")
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

	listOptions := metav1.ListOptions{LabelSelector: clusterinfo.MachineE2ELabel + "=true"}
	if windowsLabel {
		listOptions.LabelSelector += "," + controllers.MachineOSLabel + "=Windows"
	} else {
		listOptions.LabelSelector += "," + controllers.MachineOSLabel + "!=Windows"
	}
	err := wait.Poll(retryInterval, machineStateTimeLimit, func() (done bool, err error) {
		machines, err = tc.client.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
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
			// If waiting for a provisioned Machine, ensure the Machine is fully attached to the network and has an
			// assigned IPv4 address.
			if phase != "Provisioned" {
				continue
			}
			hasIPv4 := false
			for _, address := range machine.Status.Addresses {
				if address.Type != v1.NodeInternalIP {
					continue
				}
				if net.ParseIP(address.Address) != nil && net.ParseIP(address.Address).To4() != nil {
					hasIPv4 = true
					break
				}
			}
			if !hasIPv4 {
				return false, nil
			}
		}
		return true, nil
	})
	if phase == "" {
		phase = "deleted"
	}

	// Log the time elapsed while waiting for creation of the Machines
	var machineType string
	if windowsLabel {
		machineType = "with the Windows label"
	} else {
		machineType = "without the Windows label"
	}
	endTime := time.Now()
	log.Printf("%v time is required for %d Machines %s to reach phase %s", endTime.Sub(startTime),
		len(machines.Items), machineType, phase)
	return machines, err
}

// waitForWindowsNode waits until there exists nodeCount Windows nodes with the correct set of annotations.
// if expectError = true, the function will wait for duration of 10 minutes if we are deleting all nodes i.e. 0 nodesCount
// else 5 minutes for the nodes as the error would be thrown immediately, else we will wait for the duration given by
// nodeCreationTime variable which is 20 minutes increasing the overall wait time in test suite
func (tc *testContext) waitForWindowsNodes(nodeCount int32, expectError, checkVersion bool, isBYOH bool) error {
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac, metadata.VersionAnnotation,
		nodeconfig.PubKeyHashAnnotation}
	if isBYOH {
		annotations = append(annotations, controllers.UsernameAnnotation)
	}

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

	privKey, pubKey, err := tc.getExpectedKeyPair()
	if err != nil {
		return errors.Wrap(err, "error getting the expected public/private key pair")
	}
	pubKeyAnnotation := nodeconfig.CreatePubKeyHashAnnotation(pubKey)

	// We are waiting 20 minutes for each windows VM to be shown up in the cluster. The value comes from
	// nodeCreationTime variable.  If we are testing a scale down from n nodes to 0, then we should
	// not take the number of nodes into account. If we are testing node creation without applying Windows label, we
	// should throw error within 5 mins.
	err = wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(nodeCount), 1))*creationTime, func() (done bool, err error) {
		nodes, err := tc.listFullyConfiguredWindowsNodes(isBYOH)
		if err != nil {
			log.Printf("failed to get list of configured Windows nodes: %s", err)
			return false, nil
		}

		for _, node := range nodes {
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
			if node.Spec.Unschedulable {
				log.Printf("expected node %s to be schedulable", node.Name)
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
				if node.Annotations[metadata.VersionAnnotation] != operatorVersion {
					log.Printf("node %s has mismatched version annotation %s. expected: %s", node.GetName(),
						node.Annotations[metadata.VersionAnnotation], operatorVersion)
					return false, nil
				}
			}
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] != pubKeyAnnotation {
				log.Printf("node %s has mismatched pubkey annotation value %s expected: %s", node.GetName(),
					node.Annotations[nodeconfig.PubKeyHashAnnotation], pubKeyAnnotation)
				return false, nil
			}
			// For BYOH nodes, ensure username annotation is decipherable and correct. Skip if deconfiguring node
			if isBYOH && !expectError {
				username, err := crypto.DecryptFromJSONString(node.Annotations[controllers.UsernameAnnotation], privKey)
				if err != nil {
					log.Printf("error decrypting username annotation for node %s: %s", node.Name, err)
					return false, nil
				}
				if username != tc.vmUsername() {
					log.Printf("username %s does not match expected value %s for node %s:", username, tc.vmUsername(),
						node.Name)
					return false, nil
				}
			}
		}
		// Now verify that we have found all the nodes being waited for
		if len(nodes) != int(nodeCount) {
			log.Printf("waiting for %d/%d Windows nodes", len(nodes), nodeCount)
			return false, nil
		}

		// Initialize/update nodes to avoid staleness
		if isBYOH {
			gc.byohNodes = nodes
		} else {
			gc.machineNodes = nodes
		}
		return true, nil
	})

	// Log the time elapsed while waiting for creation of the nodes
	endTime := time.Now()
	log.Printf("%v time is required to configure %v nodes", endTime.Sub(startTime), nodeCount)

	return err
}

// listFullyConfiguredWindowsNodes returns a slice of nodes. If isBYOH is set to true, the nodes returned will be
// BYOH nodes, else they will be nodes configured by the Machine controller.
// A node is considered fully configured once it has the WMCO version annotation applied to it.
func (tc *testContext) listFullyConfiguredWindowsNodes(isBYOH bool) ([]v1.Node, error) {
	labelSelector := v1.LabelOSStable + "=windows"
	if isBYOH {
		// BYOH label is set to true
		labelSelector = fmt.Sprintf("%s,%s=true", labelSelector, controllers.BYOHLabel)
	} else {
		// BYOH label is not set
		labelSelector = fmt.Sprintf("%s,!%s", labelSelector, controllers.BYOHLabel)
	}
	nodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(),
		metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, errors.Wrap(err, "unable to list nodes")
	}
	var windowsNodes []v1.Node
	for _, node := range nodes.Items {
		// filter out nodes that haven't been fully configured
		if _, present := node.Annotations[metadata.VersionAnnotation]; !present {
			continue
		}
		windowsNodes = append(windowsNodes, node)
	}
	return windowsNodes, nil
}

// scaleMachineApproverDeployment scales the Machine Approver deployment pods to the expectedPodCount
func (tc *testContext) scaleMachineApproverDeployment(expectedPodCount *int32) error {
	deployment, err := tc.client.K8s.AppsV1().Deployments("openshift-cluster-machine-approver").Get(context.TODO(),
		"machine-approver", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error listing Cluster Machine Approver deployment")
	}

	deployment.Spec.Replicas = expectedPodCount
	_, err = tc.client.K8s.AppsV1().Deployments("openshift-cluster-machine-approver").Update(context.TODO(),
		deployment, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "error updating Cluster Machine Approver deployment")
	}
	retryInterval := retry.Interval
	retryTimeout := retry.Timeout
	if err = wait.Poll(retryInterval, retryTimeout, func() (bool, error) {
		if deployment.Spec.Replicas == expectedPodCount {
			return true, nil
		}
		return false, nil
	}); err != nil {
		return errors.Wrap(err, "error waiting for Cluster Machine Approver deployment to be scaled")
	}
	return nil
}
