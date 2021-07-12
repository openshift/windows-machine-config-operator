package instances

import core "k8s.io/api/core/v1"

// InstanceInfo represents a host that is meant to be joined to the cluster
type InstanceInfo struct {
	// Address is the network address of the instance
	Address string
	// Username is the name of a user that can be ssh'd into.
	Username string
	// NewHostname being set means that the instance's hostname should be changed. An empty value is a no-op.
	NewHostname string
	// Node is an optional pointer to the Node object associated with the instance, if it has one.
	Node *core.Node
}

// NewInstanceInfo returns a new instanceInfo. newHostname being set means that the instance's hostname should be
// changed. An empty value is a no-op.
func NewInstanceInfo(address, username, newHostname string, node *core.Node) *InstanceInfo {
	return &InstanceInfo{Address: address, Username: username, NewHostname: newHostname, Node: node}
}
