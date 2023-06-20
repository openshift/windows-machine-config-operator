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
	var node *core.Node
	var cmData *servicescm.Data
	err = func() error {
		cm := &core.ConfigMap{}
		node, err := controller.GetAssociatedNode(directClient, addrs)
		if err != nil {
			// If no node is found, fetch the most recently created services ConfigMap for best effort cleanup
			cm, err = servicescm.GetLatest(directClient, ctx, configMapNamespace)
			if err != nil {
				return fmt.Errorf("cannot get latest services ConfigMap from namespace %s: %w", configMapNamespace, err)
			}
			cmData, err = servicescm.Parse(cm.Data)
			return err
		}
		// Otherwise, fetch the ConfigMap tied to the node's desired version annotation
		desiredVersion, present := node.Annotations[metadata.DesiredVersionAnnotation]
		if !present {
			return fmt.Errorf("node %s missing desired version annotation", node.Name)
		}
		err = directClient.Get(ctx,
			client.ObjectKey{Namespace: configMapNamespace, Name: servicescm.NamePrefix + desiredVersion}, cm)
		if err != nil {
			return err
		}
		cmData, err = servicescm.Parse(cm.Data)
		return err
	}()
	if err != nil {
		return err
	}

	svcMgr, err := manager.New()
	if err != nil {
		klog.Exitf("could not create service manager: %s", err.Error())
	}
	defer svcMgr.Disconnect()
	if err = removeServices(svcMgr, cmData.Services); err != nil {
		return err
	}
	cleanupContainers()

	if node != nil {
		return metadata.RemoveVersionAnnotation(ctx, directClient, *node)
	}
	return nil
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
