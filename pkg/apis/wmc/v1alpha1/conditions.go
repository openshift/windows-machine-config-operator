package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Reconciling indicates if the controller is currently attempting to reconcile
	Reconciling WindowsMachineConfigConditionType = "Reconciling"
	// Degraded indicates that the operator is in a degraded state
	Degraded WindowsMachineConfigConditionType = "Degraded"

	// CloudProviderAPIFailure indicates there was a failure using the cloud provider API with the provided credentials
	CloudProviderAPIFailureReason string = "CloudProviderAPIFailure"
	// VMCreationFailureReason is a reason for the degraded condition type, specifically due to the
	// creation of the VM through the cloud provider
	VMCreationFailureReason string = "VMCreationFailure"
	// VMConfigurationFailureReason is a reason for the degraded condition type, specifically due to
	// the configuration required to turn a VM into a node
	VMConfigurationFailureReason string = "VMConfigurationFailure"
	// VMTerminationFailureReason indicates if there was an issue terminating an existing VM
	VMTerminationFailureReason string = "VMTerminationFailure"
	// TrackerFailureReason indicates there was a problem tracking and storing VM information
	TrackerFailureReason string = "TrackerFailure"
	// StatusFailureReason indicates there was an issue updating the status for the operator
	StatusFailureReason string = "StatusFailure"
)

// NewWindowsMachineConfigCondition creates a new WindowsMachineConfig condition.
func NewWindowsMachineConfigCondition(condType WindowsMachineConfigConditionType, status corev1.ConditionStatus, reason, message string) *WindowsMachineConfigCondition {
	return &WindowsMachineConfigCondition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// GetWindowsMachineConfigCondition returns the condition with the provided type.
func (s *WindowsMachineConfigStatus) GetWindowsMachineConfigCondition(condType WindowsMachineConfigConditionType) *WindowsMachineConfigCondition {
	for i := range s.Conditions {
		c := s.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// SetWindowsMachineConfigCondition updates the WindowsMachineConfigStatus to include the provided condition. If the
// condition that we are about to add already exists and has the same status and reason then we are not going to update.
func (s *WindowsMachineConfigStatus) SetWindowsMachineConfigCondition(condition WindowsMachineConfigCondition) {
	// Do not update status if the previous status is the same for the same reason
	currentCond := s.GetWindowsMachineConfigCondition(condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}
	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}
	newConditions := filterOutWindowsMachineConfigCondition(s.Conditions, condition.Type)
	s.Conditions = append(newConditions, condition)
}

// filterOutCondition returns a new slice of WindowsMachineConfig conditions without conditions with the provided type.
func filterOutWindowsMachineConfigCondition(conditions []WindowsMachineConfigCondition, condType WindowsMachineConfigConditionType) []WindowsMachineConfigCondition {
	var newConditions []WindowsMachineConfigCondition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

// RemoveMachineConfigCondition removes the WindowsMachineConfig condition with the provided type.
func (s *WindowsMachineConfigStatus) RemoveMachineConfigCondition(condType WindowsMachineConfigConditionType) {
	s.Conditions = filterOutWindowsMachineConfigCondition(s.Conditions, condType)
}
