package windowsmachineconfig

import (
	"fmt"
	"strings"
	"testing"

	wmcapi "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

// TestNewDegradedCondition tests that the newDegradedCondition function returns the correct values when either 0, 1,
// or multiple reconcile errors have been seen
func TestGetDegradedCondition(t *testing.T) {
	testIO := []struct {
		name              string
		reconcileErrs     []ReconcileError
		expectedCondition wmcapi.WindowsMachineConfigCondition
	}{
		{
			name:              "Nil input",
			reconcileErrs:     nil,
			expectedCondition: *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionFalse, "", ""),
		},
		{
			name:              "Empty input",
			reconcileErrs:     []ReconcileError{},
			expectedCondition: *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionFalse, "", ""),
		},
		{
			name:              "Single degraded reason",
			reconcileErrs:     []ReconcileError{&reconcileError{degradationReason: "reason1", err: fmt.Errorf("message1")}},
			expectedCondition: *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionTrue, "reason1", "reason1: message1"),
		},
		{
			name: "Multiple degraded reasons",
			reconcileErrs: []ReconcileError{
				&reconcileError{degradationReason: "reason1", err: fmt.Errorf("message1")},
				&reconcileError{degradationReason: "reason2", err: fmt.Errorf("message2a")},
				&reconcileError{degradationReason: "reason2", err: fmt.Errorf("message2b")}},
			expectedCondition: *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionTrue, "reason1,reason2,reason2", "reason1: message1, reason2: message2a, reason2: message2b"),
		},
	}
	for _, tt := range testIO {
		t.Run(tt.name, func(t *testing.T) {
			s := &StatusManager{}
			s.setDegradedCondition(tt.reconcileErrs)
			actual := s.degradedCondition

			assert.Equal(t, tt.expectedCondition.Status, actual.Status)

			expectedReasons := strings.Split(tt.expectedCondition.Reason, ",")
			actualReasons := strings.Split(actual.Reason, ",")
			assert.ElementsMatch(t, expectedReasons, actualReasons)

			expectedMessage := strings.Split(tt.expectedCondition.Message, ",")
			for i, msg := range expectedMessage {
				expectedMessage[i] = strings.TrimSpace(msg)
			}
			actualMessage := strings.Split(actual.Message, ",")
			for i, msg := range actualMessage {
				actualMessage[i] = strings.TrimSpace(msg)
			}
			assert.ElementsMatch(t, expectedMessage, actualMessage)
		})
	}

}
