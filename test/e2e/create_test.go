package e2e

import (
	"context"
	"log"
	"math"
	"strings"
	"testing"
	"time"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/nodeconfig"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	instanceType        = "m5a.large"
	credentialAccountID = "default"
	wmcCRName           = "instance"
)

func creationTestSuite(t *testing.T) {
	// The order of tests here are important. testValidateSecrets is what populates the windowsVMs slice in the gc.
	// testNetwork needs that to check if the HNS networks have been installed. Ideally we would like to run testNetwork
	// before testValidateSecrets and testConfigMapValidation but we cannot as the source of truth for the credentials
	// are the secrets but they are created only after the VMs have been fully configured.
	// Any node object related tests should be run only after testNodeCreation as that initializes the node objects in
	// the global context.
	t.Run("WMC CR validation", testWMCValidation)
	// the failure behavior test will be skipped if gc.nodes = 0
	t.Run("failure behavior", testFailureSuite)
	t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t) })
	t.Run("Status", func(t *testing.T) { testStatusWhenSuccessful(t) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t) })
	t.Run("Network validation", testNetwork)
	t.Run("Label validation", func(t *testing.T) { testWorkerLabel(t) })
	t.Run("NodeTaint validation", func(t *testing.T) { testNodeTaint(t) })
	t.Run("User Data validation", func(t *testing.T) { testUserData(t) })
	t.Run("Machine Creation", func(t *testing.T) { testWindowsMachineCreation(t) })
}

// testWindowsMachineCreation tests the creation of the Windows machines and checks if WMCO is properly watching
// for the Windows machines created
func testWindowsMachineCreation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()
	testCases := []struct {
		testCase  string
		isWindows bool
	}{
		{
			testCase:  "Create a Windows VM with label",
			isWindows: true,
		},
		{
			testCase:  "Create a Windows VM without label",
			isWindows: false,
		},
	}
	var machineSetNames = make([]string, 0, len(testCases))
	expectedEventCount := 0
	for _, test := range testCases {
		machineSet, err := testCtx.CloudProvider.GenerateMachineSet(test.isWindows)
		require.NoError(t, err)
		err = framework.Global.Client.Create(context.TODO(), machineSet,
			&framework.CleanupOptions{TestContext: testCtx.osdkTestCtx, Timeout: cleanupTimeout,
				RetryInterval: cleanupRetryInterval})
		require.NoError(t, err)
		log.Printf("MachineSet Name %s", machineSet.Name)
		machineSetNames = append(machineSetNames, machineSet.Name)
		if test.isWindows {
			expectedEventCount++
		}
	}
	require.NoError(t, testCtx.waitForProvisionedEvent(expectedEventCount, machineSetNames))
}

// waitForProvisionedEvent waits for 2 minutes for the required number of machines to come to the provisioned state.
func (tc *testContext) waitForProvisionedEvent(expectedEventCount int, machineSetNames []string) error {
	var actualEventCount int
	timeOut := 2 * time.Minute
	startTime := time.Now()
	for i := 0; time.Since(startTime) <= timeOut; i++ {
		eventsList, err := tc.kubeclient.CoreV1().Events("openshift-machine-api").List(context.TODO(),
			metav1.ListOptions{})
		if err != nil {
			log.Printf("event list failed: %v", err)
			continue
		}
		actualEventCount = 0
		for _, event := range eventsList.Items {
			if event.Reason == "WMCO Setup" {
				for _, machineSetName := range machineSetNames {
					if strings.Contains(event.Message, machineSetName) {
						actualEventCount++
					}
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
	if actualEventCount == expectedEventCount {
		return nil
	}
	return errors.Errorf("expected event count %d but got %d", expectedEventCount, actualEventCount)
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// create WMCO custom resource
	if _, err := testCtx.createWMC(gc.numberOfNodes, gc.sshKeyPair); err != nil {
		t.Fatalf("error creating wcmo custom resource  %v", err)
	}
	if err := testCtx.waitForWindowsNodes(gc.numberOfNodes, true); err != nil {
		t.Fatalf("windows node creation failed  with %v", err)
	}
	log.Printf("Created %d Windows worker nodes", len(gc.nodes))
}

// createWMC creates a WMC object with the given replicas and keypair
func (tc *testContext) createWMC(replicas int, keyPair string) (*operator.WindowsMachineConfig, error) {
	wmco := &operator.WindowsMachineConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WindowsMachineConfig",
			APIVersion: "wmc.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      wmcCRName,
			Namespace: tc.namespace,
		},
		Spec: operator.WindowsMachineConfigSpec{
			InstanceType: instanceType,
			AWS:          &operator.AWS{CredentialAccountID: credentialAccountID, SSHKeyPair: keyPair},
			Replicas:     replicas,
		},
	}
	return wmco, framework.Global.Client.Create(context.TODO(), wmco,
		&framework.CleanupOptions{TestContext: tc.osdkTestCtx,
			Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval})
}

// waitForWindowsNode waits until there exists nodeCount Windows nodes with the correct set of annotations.
// if waitForAnnotations = false, the function will return when the node object is first seen and not wait until
// the expected annotations are present.
func (tc *testContext) waitForWindowsNodes(nodeCount int, waitForAnnotations bool) error {
	var nodes *v1.NodeList
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac}

	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 20 minutes per node. The value comes from nodeCreationTime variable.  If we are testing a
	// scale down from n nodes to 0, then we should not take the number of nodes into account.
	err := wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(nodeCount), 1))*nodeCreationTime, func() (done bool, err error) {
		nodes, err = tc.kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("waiting for %d Windows nodes", gc.numberOfNodes)
				return false, nil
			}
			return false, err
		}
		if len(nodes.Items) != nodeCount {
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
			for _, annotation := range annotations {
				_, found := node.Annotations[annotation]
				if !found {
					return false, nil
				}
			}
		}

		return true, nil
	})

	// Initialize/update nodes to avoid staleness
	gc.nodes = nodes.Items

	return err
}
