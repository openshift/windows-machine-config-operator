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
	"net"
	"strings"

	config "github.com/openshift/api/config/v1"
	openshiftClient "github.com/openshift/client-go/config/clientset/versioned"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
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

	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/services"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
	"github.com/openshift/windows-machine-config-operator/version"
)

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=delete;get;list;patch;watch
//+kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=rolebindings,verbs=get;create;delete
//+kubebuilder:rbac:groups="rbac.authorization.k8s.io",resources=clusterrolebindings,verbs=get;create;delete

const (
	// BYOHLabel is a label that should be applied to all Windows nodes not associated with a Machine.
	BYOHLabel = "windowsmachineconfig.openshift.io/byoh"
	// UsernameAnnotation is a node annotation that contains the username used to log into the Windows instance
	UsernameAnnotation = "windowsmachineconfig.openshift.io/username"
	// ConfigMapController is the name of this controller in logs and other outputs.
	ConfigMapController = "configmap"
	// wicdRBACResourceName is the name of the resources associated with WICD's RBAC permissions
	wicdRBACResourceName = "windows-instance-config-daemon"
)

// ConfigMapReconciler reconciles a ConfigMap object
type ConfigMapReconciler struct {
	instanceReconciler
	servicesManifest *servicescm.Data
}

// NewConfigMapReconciler returns a pointer to a ConfigMapReconciler
func NewConfigMapReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*ConfigMapReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize Prometheus configuration: %w", err)
	}

	directClient, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, err
	}
	ign, err := ignition.New(directClient)
	if err != nil {
		return nil, fmt.Errorf("error creating ignition object: %w", err)
	}
	argsFromIgnition, err := ign.GetKubeletArgs()
	if err != nil {
		return nil, err
	}
	oc, err := openshiftClient.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("unable to create openshift client-go client: %w", err)
	}
	ccmEnabled, err := cluster.IsCloudControllerOwnedByCCM(oc)
	if err != nil {
		return nil, fmt.Errorf("error determining if CCM owns cloud controller: %w", err)
	}
	svcData, err := services.GenerateManifest(argsFromIgnition, clusterConfig.Network().VXLANPort(),
		clusterConfig.Platform(), ccmEnabled, ctrl.Log.V(1).Enabled())
	if err != nil {
		return nil, fmt.Errorf("error generating expected Windows service state: %w", err)
	}

	return &ConfigMapReconciler{
		instanceReconciler: instanceReconciler{
			client:               mgr.GetClient(),
			k8sclientset:         clientset,
			clusterServiceCIDR:   clusterConfig.Network().GetServiceCIDR(),
			log:                  ctrl.Log.WithName("controllers").WithName(ConfigMapController),
			watchNamespace:       watchNamespace,
			recorder:             mgr.GetEventRecorderFor(ConfigMapController),
			prometheusNodeConfig: pc,
			platform:             clusterConfig.Platform(),
		},
		servicesManifest: svcData,
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *ConfigMapReconciler) Reconcile(ctx context.Context,
	req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	_ = r.log.WithValues(ConfigMapController, req.NamespacedName)

	var err error
	// Prevent WMCO upgrades while BYOH nodes are being processed.
	if err := condition.MarkAsBusy(r.client, r.watchNamespace, r.recorder, ConfigMapController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		reconcileErr = markAsFreeOnSuccess(r.client, r.watchNamespace, r.recorder, ConfigMapController,
			result.Requeue, reconcileErr)
	}()

	// Create a new signer using the private key that the instances will be configured with
	r.signer, err = signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to create signer from private key secret: %w", err)
	}

	// Fetch the ConfigMap. The predicate will have filtered out any ConfigMaps that we should not reconcile
	// so it is safe to assume that all ConfigMaps being reconciled are one of:
	// 1. windows-instances, describing hosts that need to be present in the cluster.
	// 2. windows-services, describing expected configuration of WMCO-managed services on all Windows instances
	// 3. kube-apiserver-to-kubelet-client-ca, contains the CA for the kubelet to recognize the kube-apiserver
	// client certificate
	configMap := &core.ConfigMap{}
	if err := r.client.Get(ctx, req.NamespacedName, configMap); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			// Error reading the object - requeue the request.
			return ctrl.Result{}, err
		}
		if req.NamespacedName.Name == servicescm.Name {
			// Create and retrieve the services ConfigMap as it is not present
			if configMap, err = r.createServicesConfigMap(ctx); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	r.log.V(1).Info("Reconciling", "ConfigMap", req.NamespacedName)
	// At this point configMap will be set properly
	switch req.NamespacedName.Name {
	case servicescm.Name:
		return ctrl.Result{}, r.reconcileServices(ctx, configMap)
	case wiparser.InstanceConfigMap:
		return ctrl.Result{}, r.reconcileNodes(ctx, configMap)
	case certificates.KubeAPIServerServingCAConfigMapName:
		return ctrl.Result{}, r.reconcileKubeletClientCA(ctx, configMap)
	default:
		// Unexpected configmap, log and return no error so we don't requeue
		r.log.Error(fmt.Errorf("unexpected resource triggered reconcile"), "ConfigMap", req.NamespacedName)
	}
	return ctrl.Result{}, nil
}

// reconcileServices uses the data within the services ConfigMap to ensure WMCO-managed Windows services on
// Windows Nodes have the expected configuration and are in the expected state
func (r *ConfigMapReconciler) reconcileServices(ctx context.Context, windowsServices *core.ConfigMap) error {
	if err := r.removeOutdatedServicesConfigMaps(ctx); err != nil {
		return err
	}

	// If a ConfigMap with invalid values is found, WMCO will delete and recreate it with proper values
	data, err := servicescm.Parse(windowsServices.Data)
	if err != nil || data.ValidateExpectedContent(r.servicesManifest) != nil {
		// Deleting will trigger an event for the configmap_controller, which will re-create a proper ConfigMap
		if err = r.client.Delete(ctx, windowsServices); err != nil {
			return err
		}
		r.log.Info("Deleted invalid resource", "ConfigMap",
			kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: windowsServices.Name}, "Error", err.Error())
		return nil
	}
	// TODO: actually react to changes to the services ConfigMap
	return nil
}

// removeOutdatedServicesConfigMaps deletes any outdated services ConfigMaps, if all nodes have moved past that version
func (r *ConfigMapReconciler) removeOutdatedServicesConfigMaps(ctx context.Context) error {
	nodes := &core.NodeList{}
	if err := r.client.List(ctx, nodes, client.MatchingLabels{core.LabelOSStable: "windows"}); err != nil {
		return err
	}
	versionAnnotations := getVersionAnnotations(nodes.Items)

	servicesConfigMaps, err := servicescm.List(r.client, ctx, r.watchNamespace)
	if err != nil {
		return fmt.Errorf("unable to retrieve list of services ConfigMaps: %w", err)
	}
	for _, cm := range servicesConfigMaps {
		cmVersion := strings.TrimPrefix(cm.Name, servicescm.NamePrefix)
		if isTiedToRelevantVersion(cmVersion, versionAnnotations) {
			continue
		}
		// Remove any services ConfigMap tied to a WMCO version that no Windows nodes are at anymore
		if err := r.client.Delete(ctx, &cm); err != nil {
			return fmt.Errorf("could not delete outdated services ConfigMap %s: %w", cm.Name, err)
		}
		r.log.Info("Deleted outdated resource", "ConfigMap",
			kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: cm.Name})
	}
	return nil
}

// isTiedToRelevantVersion checks if the given version is the current WMCO version or is in the given map of versions
func isTiedToRelevantVersion(v string, versions map[string]struct{}) bool {
	if v == version.Get() {
		// The current WMCO version is always considered relevant
		return true
	}
	for version := range versions {
		if v == version {
			return true
		}
	}
	return false
}

// reconcileNodes corrects the discrepancy between the "expected" instances, and the "actual" Node list
func (r *ConfigMapReconciler) reconcileNodes(ctx context.Context, windowsInstances *core.ConfigMap) error {
	// Get the current list of Windows BYOH Nodes
	nodes := &core.NodeList{}
	err := r.client.List(ctx, nodes, client.MatchingLabels{BYOHLabel: "true", core.LabelOSStable: "windows"})
	if err != nil {
		return fmt.Errorf("error listing nodes: %w", err)
	}

	// Get the list of instances that are expected to be Nodes
	instances, err := wiparser.Parse(windowsInstances.Data, nodes)
	if err != nil {
		return fmt.Errorf("unable to parse instances from ConfigMap: %w", err)
	}

	r.log.Info("processing", "instances in", wiparser.InstanceConfigMap)
	// For each instance, ensure that it is configured into a node
	if err := r.ensureInstancesAreUpToDate(instances); err != nil {
		r.recorder.Eventf(windowsInstances, core.EventTypeWarning, "InstanceSetupFailure", err.Error())
		return err
	}

	// Ensure that only instances currently specified by the ConfigMap are joined to the cluster as nodes
	if err = r.deconfigureInstances(instances, nodes); err != nil {
		return fmt.Errorf("error removing undesired nodes from cluster: %w", err)
	}

	// Once all the proper Nodes are in the cluster, configure the prometheus endpoints.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return fmt.Errorf("unable to configure Prometheus: %w", err)
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
		// When platform type is none or Nutanix, kubelet will pick a random interface to use for the Node's IP. In that case we
		// should override that with the IP that the user is providing via the ConfigMap.
		instanceInfo.SetNodeIP = r.platform == config.NonePlatformType || r.platform == config.NutanixPlatformType
		encryptedUsername, err := crypto.EncryptToJSONString(instanceInfo.Username, privateKeyBytes)
		if err != nil {
			return fmt.Errorf("unable to encrypt username for instance %s: %w", instanceInfo.Address, err)
		}
		err = r.ensureInstanceIsUpToDate(instanceInfo, map[string]string{BYOHLabel: "true", nodeconfig.WorkerLabel: ""},
			map[string]string{UsernameAnnotation: encryptedUsername})
		if err != nil {
			// It is better to return early like this, instead of trying to configure as many instances as possible in a
			// single reconcile call, as it simplifies error collection. The order the map is read from is
			// psuedo-random, so the configuration effort for configurable hosts will not be blocked by a specific host
			// that has issues with configuration.
			return fmt.Errorf("error configuring host with address %s: %w", instanceInfo.Address, err)
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
		if hasAssociatedInstance(node.Status.Addresses, instances) {
			continue
		}

		// no instance found in the provided list, remove the node from the cluster
		if err := r.deconfigureInstance(&node); err != nil {
			return fmt.Errorf("unable to deconfigure instance with node %s: %w", node.GetName(), err)
		}
		r.recorder.Eventf(windowsInstances, core.EventTypeNormal, "InstanceTeardown",
			"Deconfigured node with addresses %v", node.Status.Addresses)
	}
	return nil
}

// hasAssociatedInstance returns true if any of the given addresses is associated with any instance in the given slice.
// The instance's network address must be a valid IPv4 address or resolve to one.
func hasAssociatedInstance(nodeAddresses []core.NodeAddress, instances []*instance.Info) bool {
	for _, nodeAddress := range nodeAddresses {
		for _, instanceInfo := range instances {
			// Direct match node network address whether it is a DNS name or an IP address
			if nodeAddress.Address == instanceInfo.Address || nodeAddress.Address == instanceInfo.IPv4Address {
				return true
			}
		}
		// Reverse lookup on node IP trying to find a match to an instance specified by DNS entry
		if parseAddr := net.ParseIP(nodeAddress.Address); parseAddr != nil {
			dnsAddresses, err := net.LookupAddr(nodeAddress.Address)
			if err != nil {
				// skip node's address without reverse lookup records
				continue
			}
			for _, dns := range dnsAddresses {
				for _, instanceInfo := range instances {
					if dns == instanceInfo.Address {
						return true
					}
				}
			}
		}
	}
	return false
}

// mapToInstancesConfigMap fulfills the MapFn type, while always returning a request to the windows-instance ConfigMap
func (r *ConfigMapReconciler) mapToInstancesConfigMap(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: wiparser.InstanceConfigMap},
	}}
}

// mapToServicesConfigMap fulfills the MapFn type, while always returning a request to the windows-services ConfigMap
func (r *ConfigMapReconciler) mapToServicesConfigMap(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: servicescm.Name},
	}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isValidConfigMap(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isValidConfigMap(e.ObjectNew) ||
				r.isKubeAPIServerServingCAConfigMap(e.ObjectNew)
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
		Watches(&core.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapToInstancesConfigMap),
			builder.WithPredicates(outdatedWindowsNodePredicate(true))).
		Watches(&core.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapToServicesConfigMap),
			builder.WithPredicates(windowsNodeVersionChangePredicate())).
		Complete(r)
}

// isValidConfigMap returns true if the ConfigMap object is the InstanceConfigMap
func (r *ConfigMapReconciler) isValidConfigMap(o client.Object) bool {
	return o.GetNamespace() == r.watchNamespace &&
		(o.GetName() == wiparser.InstanceConfigMap || o.GetName() == servicescm.Name)
}

// createServicesConfigMap creates a valid ServicesConfigMap and returns it
func (r *ConfigMapReconciler) createServicesConfigMap(ctx context.Context) (*core.ConfigMap, error) {
	windowsServices, err := servicescm.Generate(servicescm.Name, r.watchNamespace, r.servicesManifest)
	if err != nil {
		return nil, err
	}

	if err = r.client.Create(ctx, windowsServices); err != nil {
		return nil, err
	}
	r.log.Info("Created", "ConfigMap", kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: servicescm.Name})
	return windowsServices, nil
}

// createServicesConfigMapOnBootup creates a valid ServicesConfigMap
// ConfigMapReconciler.createServicesConfigMap() cannot be used in its stead as the cache has not been
// populated yet, which is why the typed client is used here as it calls the API server directly.
func (r *ConfigMapReconciler) createServicesConfigMapOnBootup() error {
	windowsServices, err := servicescm.Generate(servicescm.Name, r.watchNamespace, r.servicesManifest)
	if err != nil {
		return err
	}
	cm, err := r.k8sclientset.CoreV1().ConfigMaps(r.watchNamespace).Create(context.TODO(), windowsServices,
		meta.CreateOptions{})
	if err != nil {
		return err
	}
	r.log.Info("Created", "ConfigMap", kubeTypes.NamespacedName{Namespace: cm.Namespace, Name: cm.Name})
	return nil
}

// EnsureServicesConfigMapExists ensures that the ServicesConfigMap is present and valid on operator bootup
func (r *ConfigMapReconciler) EnsureServicesConfigMapExists() error {
	windowsServices, err := r.k8sclientset.CoreV1().ConfigMaps(r.watchNamespace).Get(context.TODO(), servicescm.Name,
		meta.GetOptions{})
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// If ConfigMap is not found, create it and return
			return r.createServicesConfigMapOnBootup()
		}
		return err
	}

	data, err := servicescm.Parse(windowsServices.Data)
	if err == nil && data.ValidateExpectedContent(r.servicesManifest) == nil {
		// data exists in expected state, do nothing
		return nil
	}

	// Delete and re-create the ConfigMap with the proper values
	if err = r.k8sclientset.CoreV1().ConfigMaps(r.watchNamespace).Delete(context.TODO(), windowsServices.Name,
		meta.DeleteOptions{}); err != nil {
		return err
	}
	r.log.Info("Deleted invalid resource", "ConfigMap",
		kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: servicescm.Name})
	return r.createServicesConfigMapOnBootup()
}

// EnsureWICDRBAC ensures the WICD RBAC resources exist as expected
func (r *ConfigMapReconciler) EnsureWICDRBAC() error {
	if err := r.ensureWICDRoleBinding(); err != nil {
		return err
	}
	return r.ensureWICDClusterRoleBinding()
}

// ensureWICDRoleBinding ensures the WICD RoleBinding resource exists as expected.
// Creates it if it doesn't exist, deletes and re-creates it if it exists with improper spec.
func (r *ConfigMapReconciler) ensureWICDRoleBinding() error {
	existingRB, err := r.k8sclientset.RbacV1().RoleBindings(r.watchNamespace).Get(context.TODO(), wicdRBACResourceName,
		meta.GetOptions{})
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return fmt.Errorf("unable to get RoleBinding %s/%s: %w", r.watchNamespace, wicdRBACResourceName, err)
	}

	expectedRB := &rbac.RoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: wicdRBACResourceName,
		},
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "Role",
			Name:     wicdRBACResourceName,
		},
		Subjects: []rbac.Subject{{
			Kind:      rbac.ServiceAccountKind,
			Name:      wicdRBACResourceName,
			Namespace: r.watchNamespace,
		}},
	}
	if err == nil {
		// check if existing RoleBinding's contents are as expected, delete it if not
		if existingRB.RoleRef == expectedRB.RoleRef &&
			len(existingRB.Subjects) == len(expectedRB.Subjects) && existingRB.Subjects[0] == expectedRB.Subjects[0] {
			return nil
		}
		err = r.k8sclientset.RbacV1().RoleBindings(r.watchNamespace).Delete(context.TODO(), wicdRBACResourceName,
			meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("unable to delete RoleBinding %s/%s: %w", r.watchNamespace, wicdRBACResourceName, err)
		}
		r.log.Info("Deleted malformed resource", "RoleBinding",
			kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: existingRB.Name})
	}
	// create proper resource if it does not exist
	_, err = r.k8sclientset.RbacV1().RoleBindings(r.watchNamespace).Create(context.TODO(), expectedRB,
		meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create RoleBinding %s/%s: %w", r.watchNamespace, wicdRBACResourceName, err)
	}
	r.log.Info("Created resource", "RoleBinding",
		kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: expectedRB.Name})
	return nil
}

// ensureWICDClusterRoleBinding ensures the WICD ClusterRoleBinding resource exists as expected.
// Creates it if it doesn't exist, deletes and re-creates it if it exists with improper spec.
func (r *ConfigMapReconciler) ensureWICDClusterRoleBinding() error {
	existingCRB, err := r.k8sclientset.RbacV1().ClusterRoleBindings().Get(context.TODO(), wicdRBACResourceName,
		meta.GetOptions{})
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return err
	}

	expectedCRB := &rbac.ClusterRoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: wicdRBACResourceName,
		},
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "ClusterRole",
			Name:     wicdRBACResourceName,
		},
		Subjects: []rbac.Subject{{
			Kind:      rbac.ServiceAccountKind,
			Name:      wicdRBACResourceName,
			Namespace: r.watchNamespace,
		}},
	}
	if err == nil {
		// check if existing ClusterRoleBinding's contents are as expected, delete it if not
		if existingCRB.RoleRef == expectedCRB.RoleRef &&
			len(existingCRB.Subjects) == len(expectedCRB.Subjects) && existingCRB.Subjects[0] == expectedCRB.Subjects[0] {
			return nil
		}
		err = r.k8sclientset.RbacV1().ClusterRoleBindings().Delete(context.TODO(), wicdRBACResourceName,
			meta.DeleteOptions{})
		if err != nil {
			return err
		}
		r.log.Info("Deleted malformed resource", "ClusterRoleBinding", existingCRB.Name)
	}
	// create proper resource if it does not exist
	_, err = r.k8sclientset.RbacV1().ClusterRoleBindings().Create(context.TODO(), expectedCRB, meta.CreateOptions{})
	if err == nil {
		r.log.Info("Created resource", "ClusterRoleBinding", expectedCRB.Name)
	}
	return err
}

// isKubeAPIServerServingCAConfigMap returns true if the provided object matches the ConfigMap that contains the
// CA for the kubelet to recognize the kube-apiserver client certificate
func (r *ConfigMapReconciler) isKubeAPIServerServingCAConfigMap(obj client.Object) bool {
	return obj.GetNamespace() == certificates.KubeApiServerOperatorNamespace &&
		obj.GetName() == certificates.KubeAPIServerServingCAConfigMapName
}
