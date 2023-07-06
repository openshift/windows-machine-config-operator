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

	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
)

const (
	// NodeController is the name of this controller in logs and other outputs.
	NodeController = "node"
)

// nodeReconciler holds the info required to reconcile a Node object, inclduing that of the underlying Windows instance
type nodeReconciler struct {
	instanceReconciler
}

// NewNodeReconciler returns a pointer to a new nodeReconciler
func NewNodeReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*nodeReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	return &nodeReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(NodeController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     watchNamespace,
			recorder:           mgr.GetEventRecorderFor(NodeController),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for a
// Node object and aims to move the current state of the cluster closer to the desired state.
func (r *nodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	r.log = r.log.WithValues(NodeController, req.NamespacedName)
	// Prevent WMCO upgrades while Node objects are being processed
	if err := condition.MarkAsBusy(r.client, r.watchNamespace, r.recorder, NodeController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err = markAsFreeOnSuccess(r.client, r.watchNamespace, r.recorder, NodeController, result.Requeue, err)
	}()

	// Fetch Node reference
	node := &core.Node{}
	if err := r.client.Get(ctx, req.NamespacedName, node); err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - return error to requeue the request.
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *nodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	windowsNodePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isWindowsNode(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isWindowsNode(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isWindowsNode(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Node{}, builder.WithPredicates(windowsNodePredicate)).
		Complete(r)
}

// isWindowsNode returns true if the given object is a Windows node
func isWindowsNode(obj runtime.Object) bool {
	node, ok := obj.(*core.Node)
	if !ok {
		return false
	}
	value, ok := node.Labels[core.LabelOSStable]
	return ok && value == "windows"
}
