package windowsmachineconfig

import (
	"context"
	"github.com/pkg/errors"
	"strings"

	wmcapi "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StatusManager is in charge of updating the WindowsMachineConfig object status
type StatusManager struct {
	// client is the client used to interact with the API
	client client.Client
	// wmcName is the name of the WMC resource we will update
	wmcName types.NamespacedName
	// joinedVMCount is the count of Windows VMs that have joined the cluster as a node
	joinedVMCount int
	// degradedCondition is the condition of type "degraded" to apply to the WMC object status
	degradedCondition wmcapi.WindowsMachineConfigCondition
	// conditionsToSet are all conditions except for the degraded condition to apply to the WMC object status
	conditionsToSet []wmcapi.WindowsMachineConfigCondition
}

// NewStatusManager returns a new instance of StatusManager
func NewStatusManager(client client.Client, name types.NamespacedName) *StatusManager {
	return &StatusManager{
		client:  client,
		wmcName: name,
	}
}

// setStatusConditions sets the status conditions applied when updateStatus() is called.
func (s *StatusManager) setStatusConditions(conditions []wmcapi.WindowsMachineConfigCondition) {
	if conditions != nil {
		s.conditionsToSet = conditions
	}
}

// updateStatus updates the status of the WMC in the cluster via the client
func (s *StatusManager) updateStatus() error {
	object := &wmcapi.WindowsMachineConfig{}
	err := s.client.Get(context.TODO(), s.wmcName, object)
	if err != nil {
		return errors.Wrapf(err, "could not get %v", s.wmcName)
	}

	object.Status.JoinedVMCount = s.joinedVMCount
	for _, condition := range s.conditionsToSet {
		object.Status.SetWindowsMachineConfigCondition(condition)
	}
	if (s.degradedCondition != wmcapi.WindowsMachineConfigCondition{}) {
		object.Status.SetWindowsMachineConfigCondition(s.degradedCondition)
	}
	log.V(1).Info("updating status", "status", object.Status)
	if err = s.client.Status().Update(context.TODO(), object); err != nil {
		return errors.Wrapf(err, "could not update status")
	}
	return nil
}

// setDegradedCondition updates the status manager's degradedCondition field based on errors from reconciliation
func (s *StatusManager) setDegradedCondition(reconcileErrs []ReconcileError) {
	// If there are no degradation reasons the WMC is not degraded
	if reconcileErrs == nil || len(reconcileErrs) == 0 {
		s.degradedCondition = *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionFalse, "", "")
		return
	}

	// Add all reasons separated by a comma
	degradedReason := ""
	degradedMessage := ""
	for _, reconcileErr := range reconcileErrs {
		if reconcileErr == nil {
			continue
		}
		degradedMessage += reconcileErr.Error() + ","
		degradedReason += reconcileErr.reason() + ","
	}
	degradedReason = strings.TrimSuffix(degradedReason, ",")
	degradedMessage = strings.TrimSuffix(degradedMessage, ",")

	s.degradedCondition = *wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionTrue,
		degradedReason, degradedMessage)
}
