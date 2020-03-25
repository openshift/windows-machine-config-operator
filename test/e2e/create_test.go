package e2e

import (
	"context"
	"log"
	"testing"
	"time"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wmc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	instanceType        = "m5a.large"
	credentialAccountID = "default"
	SSHKeyPair          = "libra"
	wmcCRName           = "instance"
)

func creationTestSuite(t *testing.T) {
	numOfNodesToBeCreated := 1
	t.Run("Creation", func(t *testing.T) { testWindowsNodeCreation(t, numOfNodesToBeCreated) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t, numOfNodesToBeCreated) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t, numOfNodesToBeCreated) })
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T, nodeCount int) {
	testCtx := framework.NewTestCtx(t)
	namespace, err := testCtx.GetNamespace()
	if err != nil {
		t.Fatal(err)
	}
	// create WMCO custom resource
	wmco := &operator.WindowsMachineConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WindowsMachineConfig",
			APIVersion: "wmc.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      wmcCRName,
			Namespace: namespace,
		},
		Spec: operator.WindowsMachineConfigSpec{
			InstanceType: instanceType,
			AWS:          &operator.AWS{CredentialAccountID: credentialAccountID, SSHKeyPair: SSHKeyPair},
			Replicas:     nodeCount,
		},
	}
	if err = framework.Global.Client.Create(context.TODO(), wmco,
		&framework.CleanupOptions{TestContext: testCtx,
			Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval}); err != nil {
		t.Fatalf("error creating wcmo custom resource  %v", err)
	}
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 60 minutes. The value comes from timeout variable
	err = waitForWindowsNode(framework.Global.KubeClient, wmco.Spec.Replicas, retryInterval, timeout)
	if err != nil {
		t.Fatalf("windows node creation failed  with %v", err)
	}

}

// waitForWindowsNode to be created waits for the Windows node to be created.
func waitForWindowsNode(kubeclient kubernetes.Interface, expectedNodeCount int, retryInterval, timeout time.Duration) error {
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		nodes, err := kubeclient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: wmc.WindowsOSLabel})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("Waiting for availability of %d windows nodes\n", expectedNodeCount)
				return false, nil
			}
			return false, err
		}
		if len(nodes.Items) == expectedNodeCount {
			log.Println("Created the required number of Windows worker nodes")
			return true, nil
		}
		log.Printf("still waiting for %d number of Windows worker nodes to be available\n", expectedNodeCount)
		return false, nil
	})
	return err
}
