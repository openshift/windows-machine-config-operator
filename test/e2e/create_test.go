package e2e

import (
	"context"
	"log"
	"testing"
	"time"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/nodeconfig"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	instanceType        = "m5a.large"
	credentialAccountID = "default"
	SSHKeyPair          = "libra"
	wmcCRName           = "instance"
)

func creationTestSuite(t *testing.T) {
	// The order of tests here are important. testValidateSecrets is what populates the windowsVMs slice in the gc.
	// testNetwork needs that to check if the HNS networks have been installed. Ideally we would like to run testNetwork
	// before testValidateSecrets and testConfigMapValidation but we cannot as the source of truth for the credentials
	// are the secrets but they are created only after the VMs have been fully configured.
	// Any node object related tests should be run only after testNodeCreation as that initializes the node objects in
	// the global context.
	t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t) })
	t.Run("Network validation", testNetwork)
	t.Run("Label validation", func(t *testing.T) { testWorkerLabel(t) })
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	// create WMCO custom resource
	wmco := &operator.WindowsMachineConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WindowsMachineConfig",
			APIVersion: "wmc.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      wmcCRName,
			Namespace: testCtx.namespace,
		},
		Spec: operator.WindowsMachineConfigSpec{
			InstanceType: instanceType,
			AWS:          &operator.AWS{CredentialAccountID: credentialAccountID, SSHKeyPair: SSHKeyPair},
			Replicas:     gc.numberOfNodes,
		},
	}
	if err = framework.Global.Client.Create(context.TODO(), wmco,
		&framework.CleanupOptions{TestContext: testCtx.osdkTestCtx,
			Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval}); err != nil {
		t.Fatalf("error creating wcmo custom resource  %v", err)
	}
	err = testCtx.waitForWindowsNode()
	if err != nil {
		t.Fatalf("windows node creation failed  with %v", err)
	}
	log.Printf("Created %d Windows worker nodes", len(gc.nodes))
}

// waitForWindowsNode waits for the Windows node with the correct set of annotations to be created by the operator.
func (tc *testContext) waitForWindowsNode() error {
	var nodes *v1.NodeList
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac}

	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 20 minutes per node. The value comes from nodeCreationTime variable
	err := wait.Poll(nodeRetryInterval, time.Duration(gc.numberOfNodes)*nodeCreationTime, func() (done bool, err error) {
		nodes, err = tc.kubeclient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("waiting for %d Windows nodes", gc.numberOfNodes)
				return false, nil
			}
			return false, err
		}
		if len(nodes.Items) != gc.numberOfNodes {
			log.Printf("waiting for %d/%d Windows nodes", len(nodes.Items), gc.numberOfNodes)
			return false, nil
		}

		// Wait for annotations to be present on the node objects in the scale up caseoc
		if gc.numberOfNodes != 0 {
			log.Printf("waiting for annotations to be present on %d Windows nodes", gc.numberOfNodes)
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
