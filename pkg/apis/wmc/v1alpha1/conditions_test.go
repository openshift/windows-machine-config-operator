package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestGetWindowsMachineConfigCondition tests the GetWindowsMachineConfigCondition function works works when the
// condition is missing or present
func TestGetWindowsMachineConfigCondition(t *testing.T) {
	testIO := []struct {
		name              string
		statusIn          WindowsMachineConfigStatus
		conditionType     WindowsMachineConfigConditionType
		expectedCondition *WindowsMachineConfigCondition
	}{
		{
			name: "Condition missing",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
				},
			},
			conditionType:     Reconciling,
			expectedCondition: nil,
		},
		{
			name: "Condition present",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:   Reconciling,
						Status: corev1.ConditionTrue,
					},
				},
			},
			conditionType: Reconciling,
			expectedCondition: &WindowsMachineConfigCondition{
				Type:   Reconciling,
				Status: corev1.ConditionTrue,
			},
		},
	}

	for _, tt := range testIO {
		t.Run(tt.name, func(t *testing.T) {
			actual := tt.statusIn.GetWindowsMachineConfigCondition(tt.conditionType)
			assert.Equal(t, tt.expectedCondition, actual)
		})

	}
}

// TestGetWindowsMachineConfigCondition tests the SetWindowsMachineConfigCondition function works when the condition is
// already present or not.
func TestSetWindowsMachineConfigCondition(t *testing.T) {
	testIO := []struct {
		name           string
		statusIn       WindowsMachineConfigStatus
		conditionIn    WindowsMachineConfigCondition
		expectedStatus WindowsMachineConfigStatus
	}{
		{
			name: "New condition",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
				},
			},
			conditionIn: WindowsMachineConfigCondition{
				Type:               Reconciling,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Unix(1, 1),
				Reason:             "Reason",
				Message:            "Message",
			},
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "Reason",
						Message:            "Message",
					},
				},
			},
		},
		{
			name: "Condition present with same state and reason",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "Reason",
						Message:            "Message",
					},
				},
			},
			conditionIn: WindowsMachineConfigCondition{
				Type:               Reconciling,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Unix(2, 2),
				Reason:             "Reason",
				Message:            "New Message",
			},
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "Reason",
						Message:            "Message",
					},
				},
			},
		},
		{
			name: "Condition present with same state and new reason",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "Reason",
						Message:            "Message",
					},
				},
			},
			conditionIn: WindowsMachineConfigCondition{
				Type:               Reconciling,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.Unix(2, 2),
				Reason:             "New Reason",
				Message:            "New Message",
			},
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "New Reason",
						Message:            "New Message",
					},
				},
			},
		},
		{
			name: "Condition present with new state and new reason",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionTrue,
						LastTransitionTime: metav1.Unix(1, 1),
						Reason:             "Reason",
						Message:            "Message",
					},
				},
			},
			conditionIn: WindowsMachineConfigCondition{
				Type:               Reconciling,
				Status:             corev1.ConditionFalse,
				LastTransitionTime: metav1.Unix(2, 2),
				Reason:             "New Reason",
				Message:            "New Message",
			},
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:               Reconciling,
						Status:             corev1.ConditionFalse,
						LastTransitionTime: metav1.Unix(2, 2),
						Reason:             "New Reason",
						Message:            "New Message",
					},
				},
			},
		},
	}

	for _, tt := range testIO {
		t.Run(tt.name, func(t *testing.T) {
			tt.statusIn.SetWindowsMachineConfigCondition(tt.conditionIn)
			assert.Equal(t, tt.expectedStatus, tt.statusIn)
		})

	}
}

// TestRemoveWindowsMachineConfigCondition tests that the RemoveWindowsMachineConfigCondition function works when the
// condition is either missing or present
func TestRemoveWindowsMachineConfigCondition(t *testing.T) {
	testIO := []struct {
		name           string
		statusIn       WindowsMachineConfigStatus
		conditionType  WindowsMachineConfigConditionType
		expectedStatus WindowsMachineConfigStatus
	}{
		{
			name: "Condition missing",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
				},
			},
			conditionType: Reconciling,
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
				},
			},
		},
		{
			name: "Condition present",
			statusIn: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
					{
						Type:   Reconciling,
						Status: corev1.ConditionTrue,
					},
				},
			},
			conditionType: Reconciling,
			expectedStatus: WindowsMachineConfigStatus{
				Conditions: []WindowsMachineConfigCondition{
					{
						Type:   Degraded,
						Status: corev1.ConditionTrue,
					},
				},
			},
		},
	}

	for _, tt := range testIO {
		t.Run(tt.name, func(t *testing.T) {
			tt.statusIn.RemoveMachineConfigCondition(tt.conditionType)
			assert.Equal(t, tt.expectedStatus, tt.statusIn)
		})

	}
}
