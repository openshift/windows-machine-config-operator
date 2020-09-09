package controller

import (
	"github.com/openshift/windows-machine-config-operator/pkg/clusternetwork"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// AddToManagerFuncs is a list of functions to add all Controllers to the Manager
var AddToManagerFuncs []func(manager.Manager, clusternetwork.ClusterNetworkConfig, string) error

// AddToManager adds all Controllers to the Manager
func AddToManager(m manager.Manager, config clusternetwork.ClusterNetworkConfig, watchNamespace string) error {
	for _, f := range AddToManagerFuncs {
		if err := f(m, config, watchNamespace); err != nil {
			return err
		}
	}
	return nil
}
