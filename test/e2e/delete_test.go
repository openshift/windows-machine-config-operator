package e2e

import (
	"context"
	"testing"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func deletionTestSuite(t *testing.T) {
	// Reset the number of nodes to be deleted to 0
	gc.numberOfNodes = 0
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t) })
	t.Run("Status", func(t *testing.T) { testStatusWhenSuccessful(t) })
	t.Run("ConfigMap validation", func(t *testing.T) { testConfigMapValidation(t) })
	t.Run("Secrets validation", func(t *testing.T) { testValidateSecrets(t) })
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// get WMCO custom resource
	wmco := &operator.WindowsMachineConfig{}
	// Get the WMCO resource called instance
	if err := framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: wmcCRName, Namespace: testCtx.namespace}, wmco); err != nil {
		t.Fatalf("error getting wcmo custom resource  %v", err)
	}
	// Delete the Windows VM that got created.
	wmco.Spec.Replicas = int32(gc.numberOfNodes)
	if err := framework.Global.Client.Update(context.TODO(), wmco); err != nil {
		t.Fatalf("error updating wcmo custom resource  %v", err)
	}
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster, so to be on safe
	// side, let's make it as 60 minutes.
	err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true)
	if err != nil {
		t.Fatalf("windows node deletion failed  with %v", err)
	}
	testCtx.cleanup()
}
