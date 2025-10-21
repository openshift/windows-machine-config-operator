package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	monv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	monclient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
)

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services;services/finalizers,verbs=create;get;delete
//+kubebuilder:rbac:groups="monitoring.coreos.com",resources=servicemonitors,verbs=create;delete;get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=*

const (
	// MetricController is the name of this controller in logs and other outputs.
	MetricController = "metrics"
	// monitoringLabel is the label added to the watch namespace to indicate cluster monitoring is enabled.
	monitoringLabel = "openshift.io/cluster-monitoring"
)

type metricReconciler struct {
	*monclient.MonitoringV1Client
	instanceReconciler
	monitoringEnabled bool
}

func NewMetricReconciler(mgr manager.Manager, clusterConfig cluster.Config, cfg *rest.Config,
	watchNamespace string) (*metricReconciler, error) {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}
	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating monitoring client: %w", err)
	}
	return &metricReconciler{
		MonitoringV1Client: mclient,
		instanceReconciler: instanceReconciler{
			client:             mgr.GetClient(),
			log:                ctrl.Log.WithName("controllers").WithName(MetricController),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterConfig.Network().GetServiceCIDR(),
			watchNamespace:     watchNamespace,
			recorder:           mgr.GetEventRecorderFor(MetricController),
		},
	}, nil
}

// Reconcile is part of the main kubernetes reconciliation loop which reads that state of the cluster for a
// Node object and aims to move the current state of the cluster closer to the desired state.
func (r *metricReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	r.log.V(1).Info("reconciling", "name", req.NamespacedName.String())
	// validate if cluster monitoring is enabled in the operator namespace
	enabled, err := r.validate(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error validating cluster monitoring label: %s", err)
	}
	// Proceed only if monitoring is enabled
	if !enabled {
		return ctrl.Result{}, nil
	}
	if err := r.ensureServiceMonitor(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("error ensuring serviceMonitor exists: %w", err)
	}
	return ctrl.Result{}, nil
}

// validate will verify if cluster monitoring is enabled in the operator namespace. If the label is set to false or not
// present, it will log and send warning events to the user. If the label holds a non-boolean value, returns an error.
func (r *metricReconciler) validate(ctx context.Context) (bool, error) {
	labelBool := false
	// validate if metrics label is added to the operator namespace
	wmcoNamespace, err := r.k8sclientset.CoreV1().Namespaces().Get(ctx, r.watchNamespace, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("error getting operator namespace: %w", err)
	}
	if value, ok := wmcoNamespace.Labels[monitoringLabel]; ok {
		labelBool, err = strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("monitoring label must have a boolean value: %w", err)
		}
	}
	if r.monitoringEnabled != labelBool && labelBool {
		// If the label exists, log the updated value
		r.recorder.Eventf(wmcoNamespace, v1.EventTypeNormal, "monitoringEnabled",
			"Cluster monitoring %s label is enabled in %s namespace",
			monitoringLabel, r.watchNamespace)
	} else if r.monitoringEnabled != labelBool && !labelBool {
		// If the label is removed, log the removal
		r.recorder.Eventf(wmcoNamespace, v1.EventTypeWarning, "monitoringDisabled",
			"Cluster monitoring %s label is not enabled in %s namespace",
			monitoringLabel, r.watchNamespace)
	}
	r.monitoringEnabled = labelBool
	return r.monitoringEnabled, nil
}

// ensureServiceMonitor creates a serviceMonitor object in the operator namespace if it does not exist.
func (r *metricReconciler) ensureServiceMonitor(ctx context.Context) error {
	// get existing serviceMonitor object if it exists
	existingSM, err := r.ServiceMonitors(r.watchNamespace).Get(ctx, metrics.WindowsMetricsResource, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"error retrieving %s serviceMonitor: %w", metrics.WindowsMetricsResource, err)
	}

	serverName := fmt.Sprintf("%s.%s.svc", metrics.WindowsMetricsResource, r.watchNamespace)
	replacement0 := "$1"
	replacement1 := "$1:9182"
	replacement2 := metrics.WindowsMetricsResource
	attachMetadataBool := true
	expectedSM := &monv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      metrics.WindowsMetricsResource,
			Namespace: r.watchNamespace,
			Labels: map[string]string{
				"name": metrics.WindowsMetricsResource,
			},
		},
		Spec: monv1.ServiceMonitorSpec{
			AttachMetadata: &monv1.AttachMetadata{
				Node: &attachMetadataBool,
			},
			Endpoints: []monv1.Endpoint{
				{
					HonorLabels:     true,
					Interval:        "30s",
					Path:            "/metrics",
					Port:            "https-metrics",
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
							Replacement: &replacement0,
							TargetLabel: "instance",
							SourceLabels: []monv1.LabelName{
								"__meta_kubernetes_endpoint_address_target_name",
							},
						},
						{ // Include only Windows nodes for this serviceMonitor
							Action: "keep",
							Regex:  "windows",
							SourceLabels: []monv1.LabelName{
								"__meta_kubernetes_node_label_kubernetes_io_os",
							},
						},
						{ // Change the port from the kubelet port 10250 to 9182
							Action:      "replace",
							Regex:       "(.+)(?::\\d+)",
							Replacement: &replacement1,
							TargetLabel: "__address__",
							SourceLabels: []monv1.LabelName{
								"__address__",
							},
						},
						{ // Update the job label from kubelet to windows-exporter
							Action:      "replace",
							Replacement: &replacement2,
							TargetLabel: "job",
						},
					},
				},
			},
			NamespaceSelector: monv1.NamespaceSelector{
				MatchNames: []string{"kube-system"},
			},
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"k8s-app": "kubelet",
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
		err = r.ServiceMonitors(r.watchNamespace).Delete(ctx, metrics.WindowsMetricsResource,
			metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("unable to delete service monitor %s/%s: %w", r.watchNamespace, metrics.WindowsMetricsResource,
				err)
		}
		r.log.Info("Deleted malformed resource", "serviceMonitor", metrics.WindowsMetricsResource,
			"namespace", r.watchNamespace)
	}

	_, err = r.ServiceMonitors(r.watchNamespace).Create(ctx, expectedSM, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service monitor: %w", err)
	}
	return nil
}

// mapToWatchNamespace fulfills the MapFn type, while always returning a request to the operator watch namespace
func (r *metricReconciler) mapToWatchNamespace(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() == r.watchNamespace {
		return []reconcile.Request{{
			NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: metrics.WindowsMetricsResource},
		}}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *metricReconciler) SetupWithManager(mgr ctrl.Manager) error {
	metricsPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isOperatorNamespace(e.Object, r.watchNamespace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool { return isOperatorNamespace(e.ObjectNew, r.watchNamespace) },
		GenericFunc: func(e event.GenericEvent) bool {
			return isOperatorNamespace(e.Object, r.watchNamespace)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Namespace{}, builder.WithPredicates(metricsPredicate)).
		Watches(&monv1.ServiceMonitor{}, handler.EnqueueRequestsFromMapFunc(r.mapToWatchNamespace)).
		Complete(r)
}

// isOperatorNamespace returns true if the given object is the operator namespace
func isOperatorNamespace(obj runtime.Object, watchNamespace string) bool {
	namespace, ok := obj.(*v1.Namespace)
	if !ok {
		return false
	}
	return namespace.GetName() == watchNamespace
}
