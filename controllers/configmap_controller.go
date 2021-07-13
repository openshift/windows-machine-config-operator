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
	"net"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
)

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps/finalizers,verbs=update

const (
	// BYOHAnnotation is an an anotation that should be applied to all Windows nodes not associated with a Machine.
	BYOHAnnotation = "windowsmachineconfig.openshift.io/byoh"
	// InstanceConfigMap is the name of the ConfigMap where VMs to be configured should be described.
	InstanceConfigMap = "windows-instances"
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
			log:                  ctrl.Log.WithName("controllers").WithName("ConfigMap"),
			watchNamespace:       watchNamespace,
			recorder:             mgr.GetEventRecorderFor("configmap"),
			vxlanPort:            clusterConfig.Network().VXLANPort(),
			prometheusNodeConfig: pc,
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

// parseHosts gets the lists of hosts specified in the configmap's data
func (r *ConfigMapReconciler) parseHosts(configMapData map[string]string) ([]*instances.InstanceInfo, error) {
	hosts := make([]*instances.InstanceInfo, 0)
	// Get information about the hosts from each entry. The expected key/value format for each entry is:
	// <address>: username=<username>
	for address, data := range configMapData {
		if err := validateAddress(address); err != nil {
			return nil, errors.Wrapf(err, "invalid address %s", address)
		}
		splitData := strings.SplitN(data, "=", 2)
		if len(splitData) == 0 || splitData[0] != "username" {
			return hosts, errors.Errorf("data for entry %s has an incorrect format", address)
		}

		hosts = append(hosts, instances.NewInstanceInfo(address, splitData[1], ""))
	}
	return hosts, nil
}

// validateAddress checks that the given address is either an ipv4 address, or resolves to any ip address
func validateAddress(address string) error {
	// first check if address is an IP address
	if parsedAddr := net.ParseIP(address); parsedAddr != nil {
		if parsedAddr.To4() != nil {
			return nil
		}
		// if the address parses into an IP but is not ipv4 it must be ipv6
		return errors.Errorf("ipv6 is not supported")
	}
	// Do a check that the DNS provided is valid
	addressList, err := net.LookupHost(address)
	if err != nil {
		return errors.Wrapf(err, "error looking up DNS")
	}
	if len(addressList) == 0 {
		return errors.Errorf("DNS did not resolve to an address")
	}
	return nil
}

// reconcileNodes corrects the discrepancy between the "expected" hosts slice, and the "actual" nodelist
func (r *ConfigMapReconciler) reconcileNodes(ctx context.Context, instances *core.ConfigMap) error {
	// Get the list of instances that are expected to be Nodes
	hosts, err := r.parseHosts(instances.Data)
	if err != nil {
		return errors.Wrapf(err, "unable to parse hosts from configmap")
	}

	nodes := &core.NodeList{}
	if err := r.client.List(ctx, nodes); err != nil {
		return errors.Wrap(err, "error listing nodes")
	}

	// For each host, ensure that it is configured into a node. On error of any host joining, return error and requeue.
	// It is better to return early like this, instead of trying to configure as many nodes as possible in a single
	// reconcile call, as it simplifies error collection. The order the map is read from is psuedo-random, so the
	// configuration effort for configurable hosts will not be blocked by a specific host that has issues with
	// configuration.
	for _, host := range hosts {
		err := r.ensureNodeIsConfigured(host, nodes)
		if err != nil {
			r.recorder.Eventf(instances, core.EventTypeWarning, "InstanceSetupFailure",
				"unable to join instance with address %s to the cluster", host.Address)
			return errors.Wrapf(err, "error configuring host with address %s", host.Address)
		}
	}
	// TODO: Remove BYOH nodes that are not associated with the parsed host list
	//       https://issues.redhat.com/browse/WINC-582

	// Once all the proper Nodes are in the cluster, configure the prometheus endpoints.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return errors.Wrap(err, "unable to configure Prometheus")
	}
	return nil
}

// ensureNodeIsConfigured ensures that the given instance has an associated Node
func (r *ConfigMapReconciler) ensureNodeIsConfigured(instance *instances.InstanceInfo, nodes *core.NodeList) error {
	_, found := findNode(instance.Address, nodes)
	if found {
		// TODO: Check version for upgrade case https://issues.redhat.com/browse/WINC-580 and remove and re-add the node
		//       if needed. Possibly also do this if the node is not in the `Ready` state.
		return nil
	}

	if err := r.configureInstance(instance, map[string]string{BYOHAnnotation: "true"}); err != nil {
		return errors.Wrap(err, "error configuring node")
	}

	return nil
}

// findNode returns a pointer to the node with an address matching the given address and a bool indicating if the node
// was found or not.
func findNode(address string, nodes *core.NodeList) (*core.Node, bool) {
	for _, node := range nodes.Items {
		for _, nodeAddress := range node.Status.Addresses {
			if address == nodeAddress.Address {
				return &node, true
			}
		}
	}
	return nil, false
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			if e.Object.GetNamespace() == r.watchNamespace && e.Object.GetName() == InstanceConfigMap {
				return true
			}
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectNew.GetNamespace() == r.watchNamespace && e.ObjectNew.GetName() == InstanceConfigMap {
				return true
			}
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.ConfigMap{}, builder.WithPredicates(configMapPredicate)).
		Complete(r)
}
