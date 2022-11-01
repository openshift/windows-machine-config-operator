package instance

import (
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/netutil"
	"github.com/openshift/windows-machine-config-operator/version"
)

// Info represents a instance that is meant to be joined to the cluster
type Info struct {
	// Address is the network address of the instance as specified by the associated ConfigMap entry.
	// Must be an IPv4 address or a DNS name that resolves to one.
	Address string
	// IPv4Address is the IPv4 address associated with the instance's given Address. May be the same value.
	IPv4Address string
	// Username is the name of a user that can be ssh'd into.
	Username string
	// NewHostname being set means that the instance's hostname should be changed. An empty value is a no-op.
	NewHostname string
	// SetNodeIP indicates if the instance should have the node-ip arg set when bootstrapping.
	SetNodeIP bool
	// Node is an optional pointer to the Node object associated with the instance, if it has one.
	Node *core.Node
}

// NewInfo returns a new Info. newHostname being set means that the instance's hostname should be
// changed. An empty value is a no-op.
func NewInfo(address, username, newHostname string, setNodeIP bool, node *core.Node) (*Info, error) {
	ipv4, err := netutil.ResolveToIPv4Address(address)
	if err != nil {
		return nil, errors.Wrapf(err, "invalid address %s, unable to create instance info", address)
	}
	return &Info{Address: address, IPv4Address: ipv4, Username: username, NewHostname: newHostname,
		SetNodeIP: setNodeIP, Node: node}, nil
}

// UpToDate returns true if the instance was configured by the current WMCO version
func (i *Info) UpToDate() bool {
	if i.Node == nil {
		return false
	}
	versionAnnotation, present := i.Node.GetAnnotations()[metadata.VersionAnnotation]
	return present && versionAnnotation == version.Get()
}

// UpgradeRequired returns true if the instance needs to go through the upgrade process
func (i *Info) UpgradeRequired() bool {
	// instance being up to date implies instance is fully upgraded
	if i.UpToDate() {
		return false
	}

	// Instance has no node and should not go through the upgrade process
	if i.Node == nil {
		return false
	}

	// Version annotation not being present means that the node has been created but not fully configured.
	// The upgrade process is not required, the node should just be configured normally.
	_, present := i.Node.GetAnnotations()[metadata.VersionAnnotation]
	if !present {
		return false
	}

	// Version annotation has an incorrect value, this was configured by an older version of WMCO and should be
	// fully deconfigured before being configured by the current version.
	return true
}
