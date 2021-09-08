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

	config "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

const (
	// BYOHLabel is a label that should be applied to all Windows nodes not associated with a Machine.
	BYOHLabel = "windowsmachineconfig.openshift.io/byoh"
	// UsernameAnnotation is a node annotation that contains the username used to log into the Windows instance
	UsernameAnnotation = "windowsmachineconfig.openshift.io/username"
)

// ConfigMapReconciler reconciles a ConfigMap object
type ConfigMapReconciler struct {
	instanceReconciler
}

// NewConfigMapReconciler returns a pointer to a ConfigMapReconciler
func NewConfigMapReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*ConfigMapReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, errors.Wrap(err, "unable to initialize Prometheus configuration")
	}
	return &ConfigMapReconciler{
		instanceReconciler: instanceReconciler{
			client:               mgr.GetClient(),
			k8sclientset:         clientset,
			clusterServiceCIDR:   clusterConfig.Network().GetServiceCIDR(),
			log:                  ctrl.Log.WithName("controllers").WithName("configmap"),
			watchNamespace:       watchNamespace,
			recorder:             mgr.GetEventRecorderFor("configmap"),
			vxlanPort:            clusterConfig.Network().VXLANPort(),
			prometheusNodeConfig: pc,
			platform:             clusterConfig.Platform(),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.log.WithValues("configmap", req.NamespacedName)

	var err error
	// Create a new signer using the private key that the instances will be configured with
	r.signer, err = signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "unable to create signer from private key secret")
	}

	// Fetch the ConfigMap. The predicate will have filtered out any ConfigMaps that we should not reconcile
	// so it is safe to assume that all ConfigMaps being reconciled describe hosts that need to be present in the
	// cluster. This also handles the case when the reconciliation is kicked off by the InstanceConfigMap being deleted.
	// In the deletion case, an empty InstanceConfigMap will be reconciled now resulting in all existing BYOH nodes
	// being deleted.
	configMap := &core.ConfigMap{}
	if err := r.client.Get(ctx, req.NamespacedName, configMap); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			// Error reading the object - requeue the request.
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, r.reconcileNodes(ctx, configMap)
}

// reconcileNodes corrects the discrepancy between the "expected" instances, and the "actual" Node list
func (r *ConfigMapReconciler) reconcileNodes(ctx context.Context, windowsInstances *core.ConfigMap) error {
	// Get the current list of Windows BYOH Nodes
	nodes := &core.NodeList{}
	err := r.client.List(ctx, nodes, client.MatchingLabels{BYOHLabel: "true", core.LabelOSStable: "windows"})
	if err != nil {
		return errors.Wrap(err, "error listing nodes")
	}

	// Get the list of instances that are expected to be Nodes
	instances, err := wiparser.Parse(windowsInstances.Data, nodes)
	if err != nil {
		return errors.Wrap(err, "unable to parse instances from ConfigMap")
	}

	r.log.Info("processing", "instances in", wiparser.InstanceConfigMap)
	// For each instance, ensure that it is configured into a node
	if err := r.ensureInstancesAreUpToDate(instances); err != nil {
		r.recorder.Eventf(windowsInstances, core.EventTypeWarning, "InstanceSetupFailure", err.Error())
		return err
	}

	// Ensure that only instances currently specified by the ConfigMap are joined to the cluster as nodes
	if err = r.deconfigureInstances(instances, nodes); err != nil {
		return errors.Wrap(err, "error removing undesired nodes from cluster")
	}

	// Once all the proper Nodes are in the cluster, configure the prometheus endpoints.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return errors.Wrap(err, "unable to configure Prometheus")
	}
	return nil
}

// ensureInstancesAreUpToDate configures all instances that require configuration
func (r *ConfigMapReconciler) ensureInstancesAreUpToDate(instances []*instance.Info) error {
	// Get private key to encrypt instance usernames
	privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return err
	}
	windowsInstances := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: wiparser.InstanceConfigMap,
		Namespace: r.watchNamespace}}
	for _, instanceInfo := range instances {
		// When platform type is none, kubelet will pick a random interface to use for the Node's IP. In that case we
		// should override that with the IP that the user is providing via the ConfigMap.
		instanceInfo.SetNodeIP = r.platform == config.NonePlatformType
		encryptedUsername, err := crypto.EncryptToJSONString(instanceInfo.Username, privateKeyBytes)
		if err != nil {
			return errors.Wrapf(err, "unable to encrypt username for instance %s", instanceInfo.Address)
		}
		err = r.ensureInstanceIsUpToDate(instanceInfo, map[string]string{BYOHLabel: "true", nodeconfig.WorkerLabel: ""},
			map[string]string{UsernameAnnotation: encryptedUsername})
		if err != nil {
			// It is better to return early like this, instead of trying to configure as many instances as possible in a
			// single reconcile call, as it simplifies error collection. The order the map is read from is
			// psuedo-random, so the configuration effort for configurable hosts will not be blocked by a specific host
			// that has issues with configuration.
			return errors.Wrapf(err, "error configuring host with address %s", instanceInfo.Address)
		}
		r.recorder.Eventf(windowsInstances, core.EventTypeNormal, "InstanceSetup",
			"Configured instance with address %s as a worker node", instanceInfo.Address)
	}
	return nil
}

// deconfigureInstances removes all BYOH nodes that are not specified in the given instances slice, and
// deconfigures the instances associated with them. The nodes parameter should be a list of all Windows BYOH nodes.
func (r *ConfigMapReconciler) deconfigureInstances(instances []*instance.Info, nodes *core.NodeList) error {
	windowsInstances := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: wiparser.InstanceConfigMap,
		Namespace: r.watchNamespace}}
	for _, node := range nodes.Items {
		// Check for instances associated with this node
		if hasEntry := hasAssociatedInstance(&node, instances); hasEntry {
			continue
		}
		// no instance found in the provided list, remove the node from the cluster
		if err := r.deconfigureInstance(&node); err != nil {
			return errors.Wrapf(err, "unable to deconfigure instance with node %s", node.GetName())
		}
		r.recorder.Eventf(windowsInstances, core.EventTypeNormal, "InstanceTeardown",
			"Deconfigured node with addresses %v", node.Status.Addresses)
	}
	return nil
}

// hasAssociatedInstance returns true if the given node is associated with an instance in the given slice
func hasAssociatedInstance(node *core.Node, instances []*instance.Info) bool {
	for _, instanceInfo := range instances {
		for _, nodeAddress := range node.Status.Addresses {
			if instanceInfo.Address == nodeAddress.Address {
				return true
			}
		}
	}
	return false
}

// mapToConfigMap fulfills the MapFn type, while always returning a request to the windows-instance ConfigMap
func (r *ConfigMapReconciler) mapToConfigMap(_ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: wiparser.InstanceConfigMap},
	}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isValidConfigMap(e.ObjectNew)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		Watches(&source.Kind{Type: &core.Node{}}, handler.EnqueueRequestsFromMapFunc(r.mapToConfigMap),
			builder.WithPredicates(windowsNodePredicate(true))).
		Complete(r)
}

// isValidConfigMap returns true if the ConfigMap object is the InstanceConfigMap
func (r *ConfigMapReconciler) isValidConfigMap(o client.Object) bool {
	return o.GetNamespace() == r.watchNamespace && o.GetName() == wiparser.InstanceConfigMap
}
