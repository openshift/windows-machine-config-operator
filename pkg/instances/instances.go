package instances

import (
	"net"
	"strings"

	"github.com/pkg/errors"
)

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

// parseHosts gets the lists of hosts specified in the configmap's data
func ParseHosts(configMapData map[string]string) ([]*InstanceInfo, error) {
	hosts := make([]*InstanceInfo, 0)
	// Get information about the hosts from each entry. The expected key/value format for each entry is:
	// <address>: username=<username>
	for address, data := range configMapData {
		if err := validateAddress(address); err != nil {
			return nil, errors.Wrapf(err, "invalid address %s", address)
		}
		splitData := strings.SplitN(data, "=", 2)
		if len(splitData) == 0 || splitData[0] != "username" {
			return hosts, errors.Errorf("data for entry %s has an incorrect format", address)
		}

		hosts = append(hosts, NewInstanceInfo(address, splitData[1], ""))
	}
	return hosts, nil
}

// validateAddress checks that the given address is either an ipv4 address, or resolves to any ip address
func validateAddress(address string) error {
	// first check if address is an IP address
	if parsedAddr := net.ParseIP(address); parsedAddr != nil {
		if parsedAddr.To4() != nil {
			return nil
		}
		// if the address parses into an IP but is not ipv4 it must be ipv6
		return errors.Errorf("ipv6 is not supported")
	}
	// Do a check that the DNS provided is valid
	addressList, err := net.LookupHost(address)
	if err != nil {
		return errors.Wrapf(err, "error looking up DNS")
	}
	if len(addressList) == 0 {
		return errors.Errorf("DNS did not resolve to an address")
	}
	return nil
}
