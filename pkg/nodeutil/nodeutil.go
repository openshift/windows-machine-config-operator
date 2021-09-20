package nodeutil

import (
	core "k8s.io/api/core/v1"
)

// FindByAddress returns a pointer to the node within the given list with an address matching the given address, or
// nil if the node was found.
func FindByAddress(address string, nodes *core.NodeList) *core.Node {
	for _, node := range nodes.Items {
		for _, nodeAddress := range node.Status.Addresses {
			if address == nodeAddress.Address {
				return &node
			}
		}
	}
	return nil
}
