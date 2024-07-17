/*
Copyright 2021.

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

package controllers

import (
	"context"
	"fmt"

	config "github.com/openshift/api/config/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/registries"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

//+kubebuilder:rbac:groups="config.openshift.io",resources=imagedigestmirrorsets,verbs=get;list;watch
//+kubebuilder:rbac:groups="config.openshift.io",resources=imagetagmirrorsets,verbs=get;list;watch

const (
	// RegistryController is the name of this controller in logs and other outputs.
	RegistryController = "registry"
)

// registryReconciler holds the info required to reconcile image registry settings on Windows nodes
type registryReconciler struct {
	instanceReconciler
}

// NewRegistryReconciler returns a pointer to a new registryReconciler
func NewRegistryReconciler(mgr manager.Manager, clusterConfig cluster.Config,
	watchNamespace string) (*registryReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	return &registryReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(RegistryController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     watchNamespace,
			recorder:           mgr.GetEventRecorderFor(RegistryController),
			platform:           clusterConfig.Platform(),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for objects
// related to image registry config and aims to move the current state of the cluster closer to the desired state.
func (r *registryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	r.log = r.log.WithValues(RegistryController, req.NamespacedName)

	configFiles, err := registries.GenerateConfigFiles(ctx, r.client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Transfer the generated registry config folder to each Windows node, completely replacing any existing config
	nodes := &core.NodeList{}
	if err := r.client.List(ctx, nodes, client.MatchingLabels{core.LabelOSStable: "windows"}); err != nil {
		return ctrl.Result{}, err
	}
	r.signer, err = signer.Create(types.NamespacedName{Namespace: r.watchNamespace, Name: secrets.PrivateKeySecret},
		r.client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to create signer from private key secret: %w", err)
	}
	for _, node := range nodes.Items {
		winInstance, err := r.instanceFromNode(&node)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to create instance object from node: %w", err)
		}
		nc, err := nodeconfig.NewNodeConfig(r.client, r.k8sclientset, r.clusterServiceCIDR, r.watchNamespace,
			winInstance, r.signer, nil, nil, r.platform)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create new nodeconfig: %w", err)
		}

		r.log.Info("updating containerd config", "directory", windows.ContainerdConfigDir, "node", node.Name)
		// TODO: If this flakes for any one node, we have to loop over all nodes again and re-transfer the directory to
		// all nodes. We should fix this as part of https://issues.redhat.com/browse/WINC-1306
		if err := nc.Windows.ReplaceDir(configFiles, windows.ContainerdConfigDir); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *registryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mirrorSetPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
	}
	secretPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isGlobalPullSecret(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// get update event only when secret data is changed
			if isGlobalPullSecret(e.ObjectNew) {
				if string(e.ObjectOld.(*core.Secret).Data[core.DockerConfigJsonKey][:]) !=
					string(e.ObjectNew.(*core.Secret).Data[core.DockerConfigJsonKey][:]) {
					return true
				}
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isGlobalPullSecret(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isGlobalPullSecret(e.Object)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&config.ImageDigestMirrorSet{}, builder.WithPredicates(mirrorSetPredicate)).
		Watches(&config.ImageTagMirrorSet{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(mirrorSetPredicate)).
		Watches(&core.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapToRegistryRequest), builder.WithPredicates(secretPredicate)).
		Complete(r)
}

// mapToRegistryRequest is a mapping function that returns one request upon a nameless/namespaceless object
func (r *registryReconciler) mapToRegistryRequest(_ context.Context, _ client.Object) []reconcile.Request {
	// construct a request in order to queue up a reconcile; the object's inner data is not used by this controller
	return []reconcile.Request{{}}
}

// isGlobalPullSecret returns true if the provided object is the cluster's global pull secret
func isGlobalPullSecret(obj client.Object) bool {
	return obj.GetName() == registries.GlobalPullSecretName && obj.GetNamespace() == registries.GlobalPullSecretNamespace
}
