//go:build windows

/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cleanup

import (
	"context"
	"fmt"

	core "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/daemon/controller"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/manager"
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/powershell"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
)

// Deconfigure removes all managed services from the instance and the version annotation, if it has an associated node.
// If we are able to get the services ConfigMap tied to the desired version, all services defined in it are cleaned up.
// Otherwise, cleanup is based on the latest services ConfigMap.
// TODO: remove services with the OpenShift managed tag in best effort cleanup https://issues.redhat.com/browse/WINC-853
func Deconfigure(cfg *rest.Config, ctx context.Context, configMapNamespace string) error {
	// Cannot use a cached client as no manager will be started to populate cache
	directClient, err := controller.NewDirectClient(cfg)
	if err != nil {
		return fmt.Errorf("could not create authenticated client from service account: %w", err)
	}
	addrs, err := controller.LocalInterfaceAddresses()
	if err != nil {
		return err
	}
	node, err := controller.GetAssociatedNode(directClient, addrs)
	if err != nil {
		klog.Infof("no associated node found")
	}
	svcMgr, err := manager.New()
	if err != nil {
		klog.Exitf("could not create service manager: %s", err.Error())
	}
	defer svcMgr.Disconnect()
	services, err := getServicesToRemove(ctx, directClient, node, configMapNamespace)
	if err != nil {
		return err
	}
	if err = removeServices(svcMgr, services); err != nil {
		return err
	}
	cleanupContainers()

	if node != nil {
		return metadata.RemoveVersionAnnotation(ctx, directClient, *node)
	}
	return nil
}

// getServicesToRemove returns a list of services that should be removed as part of the cleanup process
// returns the merged Data of the latest ConfigMap, and the ConfigMap specified by the node's version annotation
func getServicesToRemove(ctx context.Context, cli client.Client, node *core.Node, configMapNamespace string) ([]servicescm.Service, error) {
	// get data from the latest services ConfigMap
	latestCM, err := servicescm.GetLatest(cli, ctx, configMapNamespace)
	if err != nil {
		return nil, fmt.Errorf("cannot get latest services ConfigMap from namespace %s: %w", configMapNamespace, err)
	}
	latestCMData, err := servicescm.Parse(latestCM.Data)
	if err != nil {
		return nil, err
	}

	// attempt to get the ConfigMap specified by the version annotation
	var versionCM core.ConfigMap
	var versionCMData *servicescm.Data
	err = func() error {
		if node == nil {
			return fmt.Errorf("no node object present")
		}
		version, present := node.Annotations[metadata.VersionAnnotation]
		if !present {
			return fmt.Errorf("node is missing version annotation")
		}
		err = cli.Get(ctx, client.ObjectKey{Namespace: configMapNamespace, Name: servicescm.NamePrefix + version},
			&versionCM)
		if err != nil {
			return err
		}
		versionCMData, err = servicescm.Parse(versionCM.Data)
		if err != nil {
			return err
		}
		return nil
	}()
	if err != nil {
		klog.Infof("error getting services ConfigMap associated with version annotation, "+
			"falling back to use latest services ConfigMap: %s", err)
		return latestCMData.Services, nil
	}

	// If the instance was configured using latestCM, return the services from that
	if versionCM.GetName() == latestCM.GetName() {
		klog.Infof("removing the services specified in %s", latestCM.GetName())
		return latestCMData.Services, nil
	}
	// merge the two ConfigMaps into one, so all potential services are listed
	klog.Infof("removing the services specified in %s and %s", versionCM.GetName(), latestCM.GetName())
	return mergeServices(latestCMData.Services, versionCMData.Services), nil
}

// mergeServices combines the list of services, prioritizing the data given by s1
func mergeServices(s1, s2 []servicescm.Service) []servicescm.Service {
	services := make(map[string]servicescm.Service)
	for _, service := range s1 {
		services[service.Name] = service
	}
	for _, service := range s2 {
		if _, present := services[service.Name]; !present {
			services[service.Name] = service
		}
	}
	var merged []servicescm.Service
	for _, service := range services {
		merged = append(merged, service)
	}
	return merged

}

// removeServices uses the given manager to remove all the given Windows services from this instance.
func removeServices(svcMgr manager.Manager, services []servicescm.Service) error {
	// Build up log message and failures
	servicesRemoved := []string{}
	failedRemovals := []error{}
	// The services are ordered by increasing priority already, so stop them in reverse order to avoid dependency issues
	for i := len(services) - 1; i >= 0; i-- {
		service := services[i]
		if err := svcMgr.DeleteService(service.Name); err != nil {
			failedRemovals = append(failedRemovals, err)
		} else {
			servicesRemoved = append(servicesRemoved, service.Name)
		}
	}
	klog.Infof("removed services: %q", servicesRemoved)
	if len(failedRemovals) > 0 {
		return fmt.Errorf("%#v", failedRemovals)
	}
	return nil
}

// cleanupContainers makes a best effort to stop all processes with the name containerd-shim-runhcs-v1, stopping
// any containers which were not able to be drained from the Node.
func cleanupContainers() {
	cmdRunner := powershell.NewCommandRunner()
	cmdRunner.Run("Stop-Process -Force -Name containerd-shim-runhcs-v1")
	return
}
