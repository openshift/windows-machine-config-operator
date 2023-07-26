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
	"github.com/openshift/windows-machine-config-operator/pkg/daemon/envvar"
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
	mergedCMData, err := getMergedCMData(ctx, directClient, configMapNamespace, node)
	if err != nil {
		return err
	}
	if err = removeServices(svcMgr, mergedCMData.Services); err != nil {
		return err
	}
	restartRequired, err := ensureEnvVarsAreRemoved(mergedCMData.WatchedEnvironmentVars)
	if err != nil {
		return err
	}
	// rebooting instance to unset the environment variables at the process level as expected
	if restartRequired {
		// Applying the reboot annotation results in an event picked up by WMCO's node controller to reboot the instance
		if err = metadata.ApplyRebootAnnotation(ctx, directClient, *node); err != nil {
			return fmt.Errorf("error setting reboot annotation on node %s: %w", node.Name, err)
		}
	}
	cleanupContainers()

	if node != nil {
		return metadata.RemoveVersionAnnotation(ctx, directClient, *node)
	}
	return nil
}

// getMergedCMData attempts to get the latest and the version CM data specified by the node's version annotation
// It returns the merged CM Data containing services and the watched environment variables
func getMergedCMData(ctx context.Context, cli client.Client,
	configMapNamespace string, node *core.Node) (*servicescm.Data, error) {
	// get data from the latest services ConfigMap
	latestCM, err := servicescm.GetLatest(cli, ctx, configMapNamespace)
	if err != nil {
		return nil, fmt.Errorf("cannot get latest services ConfigMap from namespace %s: %w",
			configMapNamespace, err)
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
		return latestCMData, nil
	}
	// If the instance was configured using latestCM, return that
	if versionCM.GetName() == latestCM.GetName() {
		return latestCMData, nil
	}
	mergedServices := mergeServices(latestCMData.Services, versionCMData.Services)
	mergedEnvVars := merge(latestCMData.WatchedEnvironmentVars, versionCMData.WatchedEnvironmentVars)
	return &servicescm.Data{
		Services:               mergedServices,
		WatchedEnvironmentVars: mergedEnvVars,
	}, nil
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

// merge returns a combined list of the given lists
func merge(e1, e2 []string) []string {
	watchedEnvVars := make(map[string]struct{})
	for _, item := range e1 {
		watchedEnvVars[item] = struct{}{}
	}
	for _, item := range e2 {
		watchedEnvVars[item] = struct{}{}
	}
	var merged []string
	for item := range watchedEnvVars {
		merged = append(merged, item)
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

// ensureEnvVarsAreRemoved removes all WICD configured ENV variables from this instance
func ensureEnvVarsAreRemoved(watchedEnvVars []string) (bool, error) {
	return envvar.EnsureVarsAreUpToDate(map[string]string{}, watchedEnvVars)
}

// cleanupContainers makes a best effort to stop all processes with the name containerd-shim-runhcs-v1, stopping
// any containers which were not able to be drained from the Node.
func cleanupContainers() {
	cmdRunner := powershell.NewCommandRunner()
	cmdRunner.Run("Stop-Process -Force -Name containerd-shim-runhcs-v1")
	return
}
