package instances

// InstanceInfo represents a host that is meant to be joined to the cluster
type InstanceInfo struct {
	Address     string
	Username    string
	NewHostname string
}

// NewInstanceInfo returns a new instanceInfo. newHostname being set means that the instance's hostname should be
// changed. An empty value is a no-op.
func NewInstanceInfo(address, username, newHostname string) *InstanceInfo {
	return &InstanceInfo{Address: address, Username: username, NewHostname: newHostname}
}
