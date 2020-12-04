package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io/ioutil"
	"log"
	"math"
	"testing"
	"time"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/nodeconfig"
)

func creationTestSuite(t *testing.T) {
	// The order of tests here are important. testValidateSecrets is what populates the windowsVMs slice in the gc.
	// testNetwork needs that to check if the HNS networks have been installed. Ideally we would like to run testNetwork
	// before testValidateSecrets and testConfigMapValidation but we cannot as the source of truth for the credentials
	// are the secrets but they are created only after the VMs have been fully configured.
	// Any node object related tests should be run only after testNodeCreation as that initializes the node objects in
	// the global context.
	if !t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t) }) {
		// No point in running the other tests if creation failed
		return
	}
	t.Run("Network validation", testNetwork)
	t.Run("Node Metadata", func(t *testing.T) { testNodeMetadata(t) })
	t.Run("NodeTaint validation", func(t *testing.T) { testNodeTaint(t) })
	t.Run("UserData validation", func(t *testing.T) { testUserData(t) })
	t.Run("UserData idempotent check", func(t *testing.T) { testUserDataTamper(t) })
	t.Run("Node Logs", func(t *testing.T) { testNodeLogs(t) })
	t.Run("Metrics validation", testMetrics)
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
	testCases := []struct {
		testCase  string
		isWindows bool
		replicas  int32
	}{
		{
			// We need to create Windows MachineSet without label first, as we are using waitForWindowsNodes method.
			testCase:  "Create a Windows MachineSet without label",
			isWindows: false,
			replicas:  1,
		},
		{
			testCase:  "Create a Windows MachineSet with label",
			isWindows: true,
			replicas:  gc.numberOfNodes,
		},
	}
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	// Create a private key secret with a randomly generated value. This is changed to the provided private key later in
	// the test.
	require.NoError(t, testCtx.createPrivateKeySecret(false), "could not create private key secret")

	for _, test := range testCases {
		if err := testCtx.createWindowsMachineSet(test.replicas, test.isWindows); err != nil {
			t.Fatalf("failed to create Windows MachineSet %v ", err)
		}
		if test.isWindows {
			// We need to cover the case where a user changes the private key secret before the WMCO has a chance to
			// configure the Machine. In order to simulate that case we need to wait for the MachineSet to be fully
			// provisioned and then change the key. The the correct amount of nodes being configured is proof that the
			// mismatched Machine created with the mismatched key was deleted and replaced.
			// Depending on timing and configuration flakes this will either cause all Machines, or all Machines after
			// the first configured Machines to hit this scenario.
			err := testCtx.waitForWindowsMachines(int(test.replicas), "Provisioned")
			require.NoError(t, err, "error waiting for Windows Machines to be provisioned")
			err = testCtx.createPrivateKeySecret(true)
			require.NoError(t, err, "error replacing private key secret")

		}
		err := testCtx.waitForWindowsNodes(test.replicas, true, !test.isWindows, false)

		if err != nil && test.isWindows {
			t.Fatalf("windows node creation failed  with %v", err)
		}
		if test.isWindows {
			log.Printf("Created %d Windows worker nodes", test.replicas)
		}
	}
}

// createWindowsMachineSet creates given number of Windows Machines.
func (tc *testContext) createWindowsMachineSet(replicas int32, windowsLabel bool) error {
	machineSet, err := tc.CloudProvider.GenerateMachineSet(windowsLabel, replicas)
	if err != nil {
		return err
	}
	return framework.Global.Client.Create(context.TODO(), machineSet,
		&framework.CleanupOptions{TestContext: tc.osdkTestCtx,
			Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval})
}

// waitForWindowsMachines waits for a certain amount of Windows Machines to reach a certain phase
func (tc *testContext) waitForWindowsMachines(machineCount int, phase string) error {
	machineCreationTimeLimit := time.Minute * 5
	return wait.Poll(retryInterval, machineCreationTimeLimit, func() (done bool, err error) {
		var machines mapi.MachineList
		err = framework.Global.Client.List(context.TODO(), &machines,
			[]client.ListOption{client.MatchingLabels{windowsmachine.MachineOSLabel: "Windows"},
				client.InNamespace("openshift-machine-api")}...)
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
		for _, machine := range machines.Items {
			if machine.Status.Phase == nil || *machine.Status.Phase != phase {
				return false, nil
			}
		}
		return true, nil
	})
}

// waitForWindowsNode waits until there exists nodeCount Windows nodes with the correct set of annotations.
// if waitForAnnotations = false, the function will return when the node object is first seen and not wait until
// the expected annotations are present.
// if expectError = true, the function will wait for duration of 5 minutes for the nodes as the error would be thrown
// immediately, else we will wait for the duration given by nodeCreationTime variable which is 20 minutes increasing
// the overall wait time in test suite
func (tc *testContext) waitForWindowsNodes(nodeCount int32, waitForAnnotations, expectError, checkVersion bool) error {
	var nodes *v1.NodeList
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac, nodeconfig.VersionAnnotation}
	var creationTime time.Duration
	startTime := time.Now()
	if expectError {
		// The time we expect to wait, if the windowsLabel is
		// not used while creating nodes.
		creationTime = time.Minute * 5
	} else {
		creationTime = nodeCreationTime
	}

	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 20 minutes per node. The value comes from nodeCreationTime variable.  If we are testing a
	// scale down from n nodes to 0, then we should not take the number of nodes into account. If we are testing node
	// creation without applying Windows label, we should throw error within 5 mins.
	err := wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(nodeCount), 1))*creationTime, func() (done bool, err error) {
		nodes, err = tc.kubeclient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
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
			for _, annotation := range annotations {
				_, found := node.Annotations[annotation]
				if !found {
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
					return false, nil
				}
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

// generatePrivateKey generates a random RSA private key
func generatePrivateKey() ([]byte, error) {
	var keyData []byte
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, errors.Wrap(err, "error generating key")
	}
	var privateKey = &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	buf := bytes.NewBuffer(keyData)
	err = pem.Encode(buf, privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding generated private key")
	}
	return buf.Bytes(), nil
}

// createPrivateKeySecret ensures that a private key secret exists with the correct data
func (tc *testContext) createPrivateKeySecret(useKnownKey bool) error {
	if err := tc.ensurePrivateKeyDeleted(); err != nil {
		return errors.Wrap(err, "error ensuring any existing private key is removed")
	}
	var keyData []byte
	var err error
	if useKnownKey {
		keyData, err = ioutil.ReadFile(gc.privateKeyPath)
		if err != nil {
			return errors.Wrapf(err, "unable to read private key data from file %s", gc.privateKeyPath)
		}
	} else {
		keyData, err = generatePrivateKey()
		if err != nil {
			return errors.Wrap(err, "error generating private key")
		}
	}

	privateKeySecret := core.Secret{
		Data: map[string][]byte{secrets.PrivateKeySecretKey: keyData},
		ObjectMeta: metav1.ObjectMeta{
			Name:      secrets.PrivateKeySecret,
			Namespace: tc.namespace,
		},
	}
	_, err = tc.kubeclient.CoreV1().Secrets(tc.namespace).Create(context.TODO(), &privateKeySecret, metav1.CreateOptions{})
	return err
}

// ensurePrivateKeyDeleted ensures that the privateKeySecret is deleted
func (tc *testContext) ensurePrivateKeyDeleted() error {
	secretsClient := tc.kubeclient.CoreV1().Secrets(tc.namespace)
	if _, err := secretsClient.Get(context.TODO(), secrets.PrivateKeySecret, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			// secret doesnt exist, do nothing
			return nil
		}
		return errors.Wrap(err, "could not get private key secret")
	}
	// Secret exists, delete it
	return secretsClient.Delete(context.TODO(), secrets.PrivateKeySecret, metav1.DeleteOptions{})
}
