package e2e

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/openshift/windows-machine-config-operator/pkg/apis"
	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wmc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

var (
	retryInterval        = time.Second * 5
	timeout              = time.Minute * 60
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 5
)

// TestWMCO sets up the testing suite for WMCO.
func TestWMCO(t *testing.T) {
	if err := setupWMCOResources(); err != nil {
		t.Fatalf("%v", err)
	}
	// TODO: In future, we'd like to skip the teardown for each test. As of now, since we just have deletion it should
	// 		be ok to call destroy directly.
	//		Jira Story: https://issues.redhat.com/browse/WINC-283
	t.Run("create", testWindowsNodeCreation)
	t.Run("destroy", testWindowsNodeDeletion)
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func testWindowsNodeCreation(t *testing.T) {
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
			Name:      "instance",
			Namespace: namespace,
		},
		Spec: operator.WindowsMachineConfigSpec{
			InstanceType: "m4.large",
			AWS:          &operator.AWS{CredentialAccountID: "default", SSHKeyPair: "libra"},
			Replicas:     1,
		},
	}
	if err = framework.Global.Client.Create(context.TODO(), wmco,
		&framework.CleanupOptions{TestContext: testCtx,
			Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval}); err != nil {
		t.Fatalf("error creating wcmo custom resource  %v", err)
	}
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 60 minutes.
	err = waitForWindowsNode(framework.Global.KubeClient, wmco.Spec.Replicas, retryInterval, timeout)
	if err != nil {
		t.Fatalf("windows node creation failed  with %v", err)
	}

}

// waitForWindowsNode to be created waits for the Windows node to be created. As of now, we're waiting for 60 minutes
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

// setupWMCO setups the resources needed to run WMCO tests
func setupWMCOResources() error {
	wmcoList := &operator.WindowsMachineConfigList{}
	err := framework.AddToFrameworkScheme(apis.AddToScheme, wmcoList)
	if err != nil {
		return errors.Wrap(err, "failed setting up test suite")
	}
	return nil
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx := framework.NewTestCtx(t)
	namespace, err := testCtx.GetNamespace()
	if err != nil {
		t.Fatal(err)
	}
	// get WMCO custom resource
	wmco := &operator.WindowsMachineConfig{}
	// Get the WMCO resource called instance
	if err := framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: "instance", Namespace: namespace}, wmco); err != nil {
		t.Fatalf("error getting wcmo custom resource  %v", err)
	}
	// Delete the Windows VM that got created.
	wmco.Spec.Replicas = 0
	if err := framework.Global.Client.Update(context.TODO(), wmco); err != nil {
		t.Fatalf("error updating wcmo custom resource  %v", err)
	}
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 60 minutes.
	err = waitForWindowsNode(framework.Global.KubeClient, wmco.Spec.Replicas, retryInterval, timeout)
	if err != nil {
		t.Fatalf("windows node deletion failed  with %v", err)
	}
	defer testCtx.Cleanup()
}
