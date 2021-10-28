package netutil

import (
	"net"

	"github.com/pkg/errors"
)

// ResolveToIPv4Address returns an IPv4 address associated with the given address. An error will be thrown if given
// an IPv6 address or a DNS address that does not resolve to an IPv4 network address.
func ResolveToIPv4Address(address string) (string, error) {
	if ip := net.ParseIP(address); ip != nil {
		// Address is either an IPv6 or IPv4 address
		ipv4 := ip.To4()
		if ipv4 == nil {
			return "", errors.Errorf("not an IPv4 network address: %s", ip.String())
		}
		return ipv4.String(), nil
	}

	// DNS address in this case
	ips, err := net.LookupIP(address)
	if err != nil {
		return "", errors.Wrapf(err, "lookup of address %s failed", address)
	}
	// Get first IPv4 address returned
	for _, returnedIP := range ips {
		if returnedIP.To4() != nil {
			return returnedIP.String(), nil
		}
	}
	return "", errors.Errorf("%s does not resolve to an IPv4 address", address)
}
