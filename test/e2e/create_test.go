package e2e

import (
	"context"
	"log"
	"testing"
	"time"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wmc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig"
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
	t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t) })
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
}

// waitForWindowsNode waits for the Windows node to be created by the operator.
func (tc *testContext) waitForWindowsNode() error {
	var nodes *v1.NodeList
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 20 minutes per node. The value comes from nodeCreationTime variable
	err := wait.Poll(nodeRetryInterval, time.Duration(gc.numberOfNodes)*nodeCreationTime, func() (done bool, err error) {
		nodes, err = tc.kubeclient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: wmc.WindowsOSLabel})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("Waiting for availability of %d windows nodes\n", gc.numberOfNodes)
				return false, nil
			}
			return false, err
		}
		if len(nodes.Items) == gc.numberOfNodes {
			log.Println("Created the required number of Windows worker nodes")
			return true, nil
		}
		log.Printf("still waiting for %d number of Windows worker nodes to be available\n", gc.numberOfNodes)
		return false, nil
	})

	for _, node := range nodes.Items {
		tc.nodes = append(tc.nodes, node)
	}

	return err
}
