/*
Copyright 2024.

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

	mcfg "github.com/openshift/api/machineconfiguration/v1"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
)

//+kubebuilder:rbac:groups="machineconfiguration.openshift.io",resources=controllerconfigs,verbs=list;watch

const (
	// ControllerConfigController is the name of this controller in logs and other outputs.
	ControllerConfigController = "controllerconfig"
)

// ControllerConfigReconciler holds the info required to reconcile information held in ControllerConfigs
type ControllerConfigReconciler struct {
	instanceReconciler
}

// NewControllerConfigReconciler returns a pointer to a new ControllerConfigReconciler
func NewControllerConfigReconciler(mgr manager.Manager, clusterConfig cluster.Config,
	watchNamespace string) (*ControllerConfigReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	return &ControllerConfigReconciler{
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(ControllerConfigController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     watchNamespace,
			recorder:           mgr.GetEventRecorderFor(ControllerConfigController),
		},
	}, nil
}

// Reconcile reacts to ControllerConfig changes in order to ensure the correct state of certificates on Windows nodes
func (r *ControllerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cc mcfg.ControllerConfig
	err := r.client.Get(ctx, req.NamespacedName, &cc)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.signer, err = signer.Create(ctx, kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to create signer from private key secret: %w", err)
	}

	// fetch all Windows nodes (Machine and BYOH instances)
	winNodes := &core.NodeList{}
	if err = r.client.List(ctx, winNodes, client.MatchingLabels{core.LabelOSStable: "windows"}); err != nil {
		return ctrl.Result{}, fmt.Errorf("error listing Windows nodes: %w", err)
	}
	// loop Windows nodes and trigger kubelet CA update
	for _, winNode := range winNodes.Items {
		if err := r.updateKubeletCA(ctx, winNode, cc.Spec.KubeAPIServerServingCAData); err != nil {
			return ctrl.Result{}, fmt.Errorf("error updating kubelet CA certificate in node %s: %w", winNode.Name, err)
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ControllerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mccPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetName() == nodeconfig.MccName
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetName() == nodeconfig.MccName
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetName() == nodeconfig.MccName
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&mcfg.ControllerConfig{}, builder.WithPredicates(mccPredicate)).
		Complete(r)
}
