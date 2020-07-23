package e2e

import (
	"context"
	"strings"
	"testing"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deletionTestSuite(t *testing.T) {
	t.Run("Deletion", func(t *testing.T) { testWindowsNodeDeletion(t) })
}

// testWindowsNodeDeletion tests the Windows node deletion from the cluster.
func testWindowsNodeDeletion(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// Get all the Machines in the
	machineSetList := &mapi.MachineSetList{}
	listOpts := []client.ListOption{
		client.InNamespace("openshift-machine-api"),
	}
	err = framework.Global.Client.List(context.TODO(), machineSetList, listOpts...)
	require.NoError(t, err)
	var windowsMachineSetWithLabel string
	var e2eMachineSetNames []string
	for _, machineSet := range machineSetList.Items {
		if strings.Contains(machineSet.Name, "e2e-windows-machineset-") {
			e2eMachineSetNames = append(e2eMachineSetNames, machineSet.Name)
		}
		if machineSet.Spec.Selector.MatchLabels["machine.openshift.io/os-id"] == "Windows" {
			windowsMachineSetWithLabel = machineSet.Name
		}
	}
	windowsMachineSet := &mapi.MachineSet{}
	err = framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: windowsMachineSetWithLabel,
		Namespace: "openshift-machine-api"}, windowsMachineSet)
	require.NoError(t, err)
	// Reset the number of nodes to be deleted to 0
	gc.numberOfNodes = 0
	// Delete the Windows VM that got created.
	windowsMachineSet.Spec.Replicas = &gc.numberOfNodes
	if err := framework.Global.Client.Update(context.TODO(), windowsMachineSet); err != nil {
		t.Fatalf("error updating windowsMachineSet custom resource  %v", err)
	}
	// As per testing, each windows VM is taking roughly 12 minutes to be shown up in the cluster
	err = testCtx.waitForWindowsNodes(gc.numberOfNodes, true, true)
	if err != nil {
		t.Fatalf("windows node deletion failed  with %v", err)
	}

	// Cleanup all the MachineSets created by us.
	for _, machineSetName := range e2eMachineSetNames {
		machineSet := &mapi.MachineSet{}
		err = framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: machineSetName,
			Namespace: "openshift-machine-api"}, machineSet)
		assert.NoError(t, framework.Global.Client.Delete(context.TODO(), machineSet))
	}
}
