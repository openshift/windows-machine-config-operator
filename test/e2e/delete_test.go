package e2e

import (
	"context"
	"testing"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"k8s.io/apimachinery/pkg/types"
)

func deletionTestSuite(t *testing.T) {
	nodeCount := 0
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t, nodeCount) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t, nodeCount) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t, nodeCount) })
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T, nodeCount int) {
	testCtx := framework.NewTestCtx(t)
	namespace, err := testCtx.GetNamespace()
	if err != nil {
		t.Fatal(err)
	}

	// get WMCO custom resource
	wmco := &operator.WindowsMachineConfig{}
	// Get the WMCO resource called instance
	if err := framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: wmcCRName, Namespace: namespace}, wmco); err != nil {
		t.Fatalf("error getting wcmo custom resource  %v", err)
	}
	// Delete the Windows VM that got created.
	wmco.Spec.Replicas = nodeCount
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
