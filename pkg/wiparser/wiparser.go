package wiparser

import (
	"context"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/netutil"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
)

// InstanceConfigMap is the name of the ConfigMap where VMs to be configured should be described.
const InstanceConfigMap = "windows-instances"

// GetInstances returns a list of Windows instances by parsing the Windows instance configMap.
func GetInstances(c client.Client, namespace string) ([]*instance.Info, error) {
	configMap := &core.ConfigMap{}
	err := c.Get(context.TODO(), kubeTypes.NamespacedName{Namespace: namespace,
		Name: InstanceConfigMap}, configMap)
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return nil, errors.Wrapf(err, "could not retrieve Windows instance ConfigMap %s",
			InstanceConfigMap)
	}

	nodes := &core.NodeList{}
	if err := c.List(context.TODO(), nodes, client.MatchingLabels{core.LabelOSStable: "windows"}); err != nil {
		return nil, errors.Wrap(err, "error listing nodes")
	}

	windowsInstances, err := Parse(configMap.Data, nodes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse instances from ConfigMap %s", configMap.Name)
	}
	return windowsInstances, nil
}

// Parse returns the list of instances specified in the Windows instances data. This function should be passed a list
// of Nodes in the cluster, as each instance returned will contain a reference to its associated Node, if it has one
// in the given NodeList. If an instance does not have an associated node from the NodeList, the node reference will
// be nil.
func Parse(instancesData map[string]string, nodes *core.NodeList) ([]*instance.Info, error) {
	if nodes == nil {
		return nil, errors.New("nodes cannot be nil")
	}
	instances := make([]*instance.Info, 0)
	// Get information about the instances from each entry. The expected key/value format for each entry is:
	// <address>: username=<username>
	for address, data := range instancesData {
		username, err := extractUsername(data)
		if err != nil {
			return instances, errors.Wrapf(err, "unable to get username for %s", address)
		}

		// Node is only guaranteed to be found when looking for its IP address
		resolvedAddress, err := netutil.ResolveToIPv4Address(address)
		if err != nil {
			return nil, err
		}

		// Create instance info with the associated node if the described instance has one.
		// Address validation occurs upon construction.
		instanceInfo, err := instance.NewInfo(address, username, "", false, nodeutil.FindByAddress(resolvedAddress, nodes))
		if err != nil {
			return nil, err
		}
		instances = append(instances, instanceInfo)
	}
	return instances, nil
}

// GetNodeUsername retrieves the username associated with the given node from the instance ConfigMap data
func GetNodeUsername(instancesData map[string]string, node *core.Node) (string, error) {
	if node == nil {
		return "", errors.New("cannot get username for nil node")
	}
	// Find entry in ConfigMap that is associated to node via address
	for _, address := range node.Status.Addresses {
		if value, found := instancesData[address.Address]; found {
			return extractUsername(value)
		}
	}
	return "", errors.Errorf("unable to find instance associated with node %s", node.GetName())
}

// extractUsername returns the username string from data in the form username=<username>
func extractUsername(value string) (string, error) {
	splitData := strings.SplitN(value, "=", 2)
	if len(splitData) == 0 || splitData[0] != "username" {
		return "", errors.New("data has an incorrect format")
	}
	return splitData[1], nil
}
