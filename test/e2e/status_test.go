package e2e

import (
	"context"
	"fmt"
	"log"
	"math"
	"testing"
	"time"

	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// testStatusWhenSuccessful ensures that the status matches the expected state of the operator when the operator should
// work correctly
func testStatusWhenSuccessful(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	// wait until reconcile is complete
	_, err = testCtx.waitForCondition(operator.Reconciling, corev1.ConditionFalse)
	require.NoError(t, err, "error waiting for degraded condition")

	// get WMC custom resource
	wmc := &operator.WindowsMachineConfig{}
	err = framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: wmcCRName,
		Namespace: testCtx.namespace}, wmc)
	require.NoError(t, err, "Could not retrieve instance of WMC")

	assert.Equal(t, wmc.Spec.Replicas, wmc.Status.JoinedVMCount, "Num of nodes in status not equal to spec")

	degraded := wmc.Status.GetWindowsMachineConfigCondition(operator.Degraded)
	require.NotNil(t, degraded)
	assert.Equal(t, corev1.ConditionFalse, degraded.Status, "Status shows operator degraded")
}

// testFailureSuite contains tests which involve invoking a reconcile failure. Requires gc.numberOfNodes to be at
// least 1 to run.
func testFailureSuite(t *testing.T) {
	if gc.numberOfNodes < 1 {
		t.Skip("testFailureSuite requires 1 or more nodes to run")
	}
	t.Run("VM provision failure", testVMProvisionFail)
}

// cleanupWMC attempts to scale down the Windows nodes to 0 and delete the WMC object
func (tc *testContext) cleanupWMC() {
	wmc := &operator.WindowsMachineConfig{}
	err := framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: wmcCRName, Namespace: tc.namespace},
		wmc)
	if err != nil {
		log.Printf("could not get WMC for cleanup: %v", err)
		return
	}

	wmc.Spec.Replicas = 0
	if err = framework.Global.Client.Update(context.TODO(), wmc); err != nil {
		log.Printf("error updating WMC replica field: %v", err)
	} else {
		// if no error updating the WMC CR, wait for nodes to hit 0
		if err = tc.waitForWindowsNodes(0, false); err != nil {
			log.Printf("error waiting for Windows nodes to scale down %v", err)
		}
	}
	if err = framework.Global.Client.Delete(context.TODO(), wmc); err != nil {
		log.Printf("error removing WMC CR during cleanup: %v", err)
	}
}

// testVMProvisionFail tests that the status has the correct degradation reason on failure
func testVMProvisionFail(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	// create WMC custom resource with key pair that will fail
	_, err = testCtx.createWMC(gc.numberOfNodes, "fakeKeyPair")
	require.NoError(t, err, "error creating WMC CR")
	defer testCtx.cleanupWMC()

	degradedCondition, err := testCtx.waitForCondition(operator.Degraded, corev1.ConditionTrue)
	require.NoError(t, err, "error waiting for degraded condition")

	assert.Contains(t, degradedCondition.Reason, operator.VMCreationFailureReason)
}

// waitForConditions returns when the CR status has a condition of type `conditionType` in state `state`
func (tc *testContext) waitForCondition(conditionType operator.WindowsMachineConfigConditionType, state corev1.ConditionStatus) (*operator.WindowsMachineConfigCondition, error) {
	var condition *operator.WindowsMachineConfigCondition
	err := wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(gc.numberOfNodes), 1))*nodeCreationTime, func() (done bool, err error) {
		log.Printf("Waiting for condition %s to have status %s", conditionType, state)
		wmc := &operator.WindowsMachineConfig{}
		err = framework.Global.Client.Get(context.TODO(), types.NamespacedName{Name: wmcCRName,
			Namespace: tc.namespace}, wmc)
		if err != nil {
			return true, errors.Wrap(err, "could not get WMC object")
		}
		log.Printf("Status %+v", wmc.Status)
		if condition = wmc.Status.GetWindowsMachineConfigCondition(conditionType); condition != nil && condition.Status == state {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not get condition")
	}
	if condition == nil {
		return nil, fmt.Errorf("timed out waiting for condition")
	}
	return condition, nil
}
