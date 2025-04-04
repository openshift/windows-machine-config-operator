// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

import (
	operatorv1 "github.com/openshift/api/operator/v1"
)

// NodeDisruptionPolicySpecActionApplyConfiguration represents a declarative configuration of the NodeDisruptionPolicySpecAction type for use
// with apply.
type NodeDisruptionPolicySpecActionApplyConfiguration struct {
	Type    *operatorv1.NodeDisruptionPolicySpecActionType `json:"type,omitempty"`
	Reload  *ReloadServiceApplyConfiguration               `json:"reload,omitempty"`
	Restart *RestartServiceApplyConfiguration              `json:"restart,omitempty"`
}

// NodeDisruptionPolicySpecActionApplyConfiguration constructs a declarative configuration of the NodeDisruptionPolicySpecAction type for use with
// apply.
func NodeDisruptionPolicySpecAction() *NodeDisruptionPolicySpecActionApplyConfiguration {
	return &NodeDisruptionPolicySpecActionApplyConfiguration{}
}

// WithType sets the Type field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Type field is set to the value of the last call.
func (b *NodeDisruptionPolicySpecActionApplyConfiguration) WithType(value operatorv1.NodeDisruptionPolicySpecActionType) *NodeDisruptionPolicySpecActionApplyConfiguration {
	b.Type = &value
	return b
}

// WithReload sets the Reload field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Reload field is set to the value of the last call.
func (b *NodeDisruptionPolicySpecActionApplyConfiguration) WithReload(value *ReloadServiceApplyConfiguration) *NodeDisruptionPolicySpecActionApplyConfiguration {
	b.Reload = value
	return b
}

// WithRestart sets the Restart field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Restart field is set to the value of the last call.
func (b *NodeDisruptionPolicySpecActionApplyConfiguration) WithRestart(value *RestartServiceApplyConfiguration) *NodeDisruptionPolicySpecActionApplyConfiguration {
	b.Restart = value
	return b
}
