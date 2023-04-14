package condition

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	operators "github.com/operator-framework/api/pkg/operators/v2"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeWait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patcher "github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

//+kubebuilder:rbac:groups="operators.coreos.com",resources=operatorconditions,verbs=get;list;patch;update;watch

const (
	upgradeableTrueReason   = "upgradeIsSafe"
	upgradeableTrueMessage  = "The operator is safe for upgrade"
	upgradeableFalseReason  = "upgradeIsNotSafe"
	upgradeableFalseMessage = "The operator is currently processing sub-components. At least one controller is busy."
	// OperatorConditionName is an environment variable set by OLM identifying the operator's OperatorCondition CR
	OperatorConditionName = "OPERATOR_CONDITION_NAME"
)

var (
	// opCondName is the name of the OLM-managed OperatorCondition resource. Empty if operator is not OLM-managed
	opCondName string
	// busyControllers is a representation of a set that holds the names of controllers that are currently reconciling
	busyControllers map[string]struct{}
	// opCondBusyControllersLock is a mutex lock to ensure thread-safe access of two shared entities,
	// the OperatorCondition resource and busyControllers set
	opCondBusyControllersLock sync.Mutex
)

// init runs once, initializing global variables
func init() {
	opCondName = getOperatorConditionCRName()
	busyControllers = make(map[string]struct{})
}

// exported methods

// MarkAsFree removes the given controller from the set of busy controllers, sets the Upgradeable condition to True
// to allow OLM to perform operator upgrades. No-op if operator is not OLM-managed
func MarkAsFree(c client.Client, watchNamespace string, recorder record.EventRecorder, controllerName string) error {
	// Check if operator is OLM-managed
	if opCondName == "" {
		return nil
	}

	opCondBusyControllersLock.Lock()
	defer opCondBusyControllersLock.Unlock()

	// Remove given controller from busy controllers set
	delete(busyControllers, controllerName)
	// If at least one other controller is still busy, no-op as Upgradeable should not be set to True yet
	if len(busyControllers) > 0 {
		return nil
	}

	opCond, err := get(c, watchNamespace)
	if err != nil {
		recorder.Eventf(opCond, core.EventTypeWarning, "OperatorConditionError",
			"Failed to get OperatorCondition CR: %v", err)
		return err
	}
	// If Upgradeable condition is already True, no-op to avoid redundant API calls
	if Validate(opCond.Spec.Conditions, operators.Upgradeable, meta.ConditionTrue) {
		return nil
	}

	err = set(c, opCond, operators.Upgradeable, meta.ConditionTrue, upgradeableTrueReason, upgradeableTrueMessage)
	if err != nil {
		recorder.Eventf(opCond, core.EventTypeWarning, "OperatorConditionError",
			"Failed to patch OperatorCondition CR. Manual operator upgrades may be needed: %v", err)
		return err
	}
	return nil
}

// MarkAsBusy marks the given conroller as busy and sets the Upgradeable condition to False
// to prevent operator upgrades by OLM. No-op if operator is not OLM-managed
func MarkAsBusy(c client.Client, watchNamespace string, recorder record.EventRecorder, controllerName string) error {
	// Check if operator is OLM-managed
	if opCondName == "" {
		return nil
	}

	opCondBusyControllersLock.Lock()
	defer opCondBusyControllersLock.Unlock()

	// Add given controller to busy controllers set. struct{} used as value since it takes up 0 bytes
	busyControllers[controllerName] = struct{}{}

	opCond, err := get(c, watchNamespace)
	if err != nil {
		recorder.Eventf(opCond, core.EventTypeWarning, "OperatorConditionError",
			"Failed to get OperatorCondition CR: %v", err)
		return err
	}
	// If Upgradeable condition is already False, no-op to avoid redundant API calls
	if Validate(opCond.Spec.Conditions, operators.Upgradeable, meta.ConditionFalse) {
		return nil
	}

	err = set(c, opCond, operators.Upgradeable, meta.ConditionFalse, upgradeableFalseReason, upgradeableFalseMessage)
	if err != nil {
		recorder.Eventf(opCond, core.EventTypeWarning, "OperatorConditionError",
			"Failed to patch OperatorCondition CR. Be cautious of automatic operator upgrades: %v", err)
		return err
	}
	return nil
}

// Validate checks that the given condition is present and holds the expected status value within the given list
func Validate(conditions []meta.Condition, conditionType string, expectedStatus meta.ConditionStatus) bool {
	for _, cond := range conditions {
		if cond.Type == conditionType {
			return cond.Status == expectedStatus
		}
	}
	return false
}

// interal helper methods

// set sets the given condition's values within the given OperatorCondition.
// Only returns after waiting for a made change to take effect.
func set(c client.Client, opCond *operators.OperatorCondition, conditionType string, status meta.ConditionStatus,
	reason, message string) error {
	newCond := meta.Condition{
		Type:    conditionType,
		Status:  status,
		Reason:  reason,
		Message: message,
	}
	if err := patch(c, opCond, newCond); err != nil {
		return err
	}
	// Wait to ensure change is actually picked up
	return wait(c, opCond.GetNamespace(), newCond.Type, newCond.Status)
}

// get retrieves the OperatorCondition resource from the given namespace
func get(c client.Client, watchNamespace string) (*operators.OperatorCondition, error) {
	opCond := &operators.OperatorCondition{}
	err := c.Get(context.TODO(), types.NamespacedName{Namespace: watchNamespace, Name: opCondName}, opCond)
	if err != nil {
		return nil, fmt.Errorf("unable to get OperatorCondition CR %s from namespace %s: %w",
			opCondName, watchNamespace, err)
	}
	return opCond, nil
}

// patch modifies the given condition within the given OperatorCondition object.
// Creates the Condition if not present, overrides it otherwise
func patch(c client.Client, opCond *operators.OperatorCondition, newCond meta.Condition) error {
	newCond.LastTransitionTime = meta.Now()
	patchData, err := json.Marshal([]*patcher.JSONPatch{
		patcher.NewJSONPatch("add", "/spec/conditions", []meta.Condition{newCond})})
	if err != nil {
		return fmt.Errorf("unable to generate patch request body for Condition %v: %w", newCond, err)
	}
	if err = c.Patch(context.TODO(), opCond, client.RawPatch(types.JSONPatchType, patchData)); err != nil {
		return fmt.Errorf("unable to apply patch %s to OperatorCondition %s: %w", patchData, opCond.GetName(), err)
	}
	return nil
}

// wait repeatedly checks if the OperatorCondition resource's Status has been updated
func wait(c client.Client, watchNamespace, condType string, expectedStatus meta.ConditionStatus) error {
	err := kubeWait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		opCond, err := get(c, watchNamespace)
		if err != nil {
			return false, err
		}
		return Validate(opCond.Status.Conditions, condType, expectedStatus), nil
	})
	if err != nil {
		return fmt.Errorf("failed to verify condition type %s has status %s: %w", condType, expectedStatus, err)
	}
	return nil
}

// getOperatorConditionCRName returns the name of the OLM-managed OperatorCondition custom resource.
// This is used to communicate current conditions to OLM, e.g. whether operators upgrades would be safe
func getOperatorConditionCRName() string {
	name, present := os.LookupEnv(OperatorConditionName)
	if !present {
		return ""
	}
	return name
}
