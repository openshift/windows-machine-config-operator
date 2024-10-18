package controllers

import (
	"context"
	"fmt"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"strconv"

	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	monv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	monclient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
)

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

const (
	// MetricControllers is the name of this controller in logs and other outputs.
	MetricControllers = "metrics"
)

type metricReconciler struct {
	*monclient.MonitoringV1Client
	instanceReconciler
}

func NewMetricReconciler(mgr manager.Manager, clusterConfig cluster.Config, cfg *rest.Config, watchNamespace string) (*metricReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}
	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating monitoring client: %w", err)
	}
	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize Prometheus configuration: %w", err)
	}

	return &metricReconciler{
		MonitoringV1Client: mclient,
		instanceReconciler: instanceReconciler{
			client:               mgr.GetClient(),
			log:                  ctrl.Log.WithName("controllers").WithName(MetricControllers),
			k8sclientset:         clientset,
			clusterServiceCIDR:   clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:       watchNamespace,
			recorder:             mgr.GetEventRecorderFor(MetricControllers),
			prometheusNodeConfig: pc,
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for a
// Node object and aims to move the current state of the cluster closer to the desired state.
func (r *metricReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	r.log = r.log.WithValues(NodeController, req.NamespacedName)
	// Prevent WMCO upgrades while Node objects are being processed
	if err := condition.MarkAsBusy(r.client, r.watchNamespace, r.recorder, NodeController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		err = markAsFreeOnSuccess(r.client, r.watchNamespace, r.recorder, NodeController, result.Requeue, err)
	}()
	// validate if cluster monitoring is enabled in the operator namespace
	enabled, err := r.validate(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error validating cluster monitoring label: %s", err)
	}
	// Proceed only if monitoring is enabled
	if !enabled {
		return ctrl.Result{}, nil
	}
	if err = r.Configure(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("error setting up metrics configurations: %w", err)
	}
	// configure Prometheus for Windows instances configured as nodes
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to configure Prometheus: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *metricReconciler) validate(ctx context.Context) (bool, error) {
	// validate if metrics label is added to namespace
	labelValue := false
	var err error
	wmcoNamespace, err := r.k8sclientset.CoreV1().Namespaces().Get(ctx, r.watchNamespace, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("error getting operator namespace: %w", err)
	}
	// if the label exists, update value from default of false
	if value, ok := wmcoNamespace.Labels["openshift.io/cluster-monitoring"]; ok {
		labelValue, err = strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("monitoring label must have a boolean value: %w", err)
		}
	}
	if !labelValue {
		r.recorder.Eventf(wmcoNamespace, v1.EventTypeWarning, "labelValidationFailed",
			"Cluster monitoring openshift.io/cluster-monitoring=true label is not enabled in %s namespace", r.watchNamespace)
	}
	metricsEnabled = labelValue
	return metricsEnabled, nil
}

func (r *metricReconciler) Configure(ctx context.Context) error {
	if err := r.ensureServiceMonitor(); err != nil {
		return fmt.Errorf("error ensuring serviceMonitor exists: %w", err)
	}
	var subsets []v1.EndpointSubset
	existingEndpoint, err := r.k8sclientset.CoreV1().Endpoints(r.watchNamespace).Get(ctx, metrics.WindowsMetricsResource, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving %s endpoint: %w", metrics.WindowsMetricsResource, err)
		}
	} else {
		subsets = existingEndpoint.Subsets
		err = r.k8sclientset.CoreV1().Endpoints(r.watchNamespace).Delete(ctx, metrics.WindowsMetricsResource, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting %s endpoint: %w", metrics.WindowsMetricsResource, err)
		}
	}
	if err := r.createEndpoint(subsets); err != nil {
		return fmt.Errorf("error creating metrics Endpoint: %w", err)
	}
	return nil
}

// ensureServiceMonitor creates a serviceMonitor object in the operator namespace if it does not exist.
func (r *metricReconciler) ensureServiceMonitor() error {
	// get existing serviceMonitor object if it exists
	existingSM, err := r.ServiceMonitors(r.watchNamespace).Get(context.TODO(), metrics.WindowsMetricsResource, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"error retrieving %s serviceMonitor: %w", metrics.WindowsMetricsResource, err)
	}

	serverName := fmt.Sprintf("%s.%s.svc", metrics.WindowsMetricsResource, r.watchNamespace)
	replacement := "$1"
	expectedSM := &monv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metrics.WindowsMetricsResource,
			Namespace: r.watchNamespace,
			Labels: map[string]string{
				"name": metrics.WindowsMetricsResource,
			},
		},
		Spec: monv1.ServiceMonitorSpec{
			Endpoints: []monv1.Endpoint{
				{
					HonorLabels:     true,
					Interval:        "30s",
					Path:            "/metrics",
					Port:            "metrics",
					Scheme:          "https",
					BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
					TLSConfig: &monv1.TLSConfig{
						CAFile: "/etc/prometheus/configmaps/serving-certs-ca-bundle/service-ca.crt",
						SafeTLSConfig: monv1.SafeTLSConfig{
							ServerName: &serverName,
						},
					},
					RelabelConfigs: []monv1.RelabelConfig{
						{
							Action:      "replace",
							Regex:       "(.*)",
							Replacement: &replacement,
							TargetLabel: "instance",
							SourceLabels: []monv1.LabelName{
								"__meta_kubernetes_endpoint_address_target_name",
							},
						},
					},
				},
			},
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"name": metrics.WindowsMetricsResource,
				},
			},
		},
	}

	if err == nil {
		// check if existing serviceMonitor's contents are as expected, delete it if not
		if existingSM.Name == expectedSM.Name && existingSM.Namespace == expectedSM.Namespace &&
			reflect.DeepEqual(existingSM.Spec, expectedSM.Spec) {
			return nil
		}
		err = r.ServiceMonitors(r.watchNamespace).Delete(context.TODO(), metrics.WindowsMetricsResource,
			metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("unable to delete service monitor %s/%s: %w", r.watchNamespace, metrics.WindowsMetricsResource,
				err)
		}
		r.log.Info("Deleted malformed resource", "serviceMonitor", metrics.WindowsMetricsResource,
			"namespace", r.watchNamespace)
	}

	_, err = r.ServiceMonitors(r.watchNamespace).Create(context.TODO(), expectedSM, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service monitor: %w", err)
	}
	return nil
}

// createEndpoint creates an endpoint object in the operator namespace.
// WMCO is no longer creating a service with a selector therefore no Endpoint
// object is created and WMCO needs to create the Endpoint object.
// We cannot create endpoints as a part of manifests deployment as
// Endpoints resources are not currently OLM-supported for bundle creation.
func (r *metricReconciler) createEndpoint(subsets []v1.EndpointSubset) error {
	// create new Endpoint
	newEndpoint := &v1.Endpoints{
		TypeMeta: metav1.TypeMeta{
			Kind: "Endpoints",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      metrics.WindowsMetricsResource,
			Namespace: r.watchNamespace,
			Labels:    map[string]string{"name": metrics.WindowsMetricsResource},
		},
		Subsets: subsets,
	}
	_, err := r.k8sclientset.CoreV1().Endpoints(r.watchNamespace).Create(context.TODO(),
		newEndpoint, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating metrics Endpoint: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *metricReconciler) SetupWithManager(mgr ctrl.Manager) error {
	metricsPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isMonitoringEnabled(e.Object, r.watchNamespace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return isMonitoringEnabled(e.ObjectNew, r.watchNamespace)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	windowsNodePredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isWindowsNode(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isWindowsNode(e.Object)
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Namespace{}, builder.WithPredicates(metricsPredicate)).
		Watches(&v1.Node{}, &handler.EnqueueRequestForObject{}, builder.WithPredicates(windowsNodePredicate)).
		Complete(r)
}

// isMonitoringEnabled returns true if the given object namespace has monitoring label set to true
func isMonitoringEnabled(obj runtime.Object, watchNamespace string) bool {
	namespace, ok := obj.(*v1.Namespace)
	if !ok {
		return false
	}
	if namespace.GetName() != watchNamespace {
		return false
	}
	if value, ok := namespace.Labels["openshift.io/cluster-monitoring"]; ok {
		labelValue, err := strconv.ParseBool(value)
		if err != nil {
			return false
		}
		return labelValue
	}
	return false
}
