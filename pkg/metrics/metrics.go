package metrics

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
	"k8s.io/client-go/kubernetes"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

//+kubebuilder:rbac:groups="",resources=services;services/finalizers,verbs=create;get;delete
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get
//+kubebuilder:rbac:groups="",resources=nodes,verbs=list
//+kubebuilder:rbac:groups="monitoring.coreos.com",resources=servicemonitors,verbs=create;get;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=*

var (
	log = ctrl.Log.WithName("metrics")
	// metricsEnabled specifies if metrics are enabled in the current cluster
	metricsEnabled = true
)

const (
	// metricsPortName specifies the portname used for Prometheus monitoring
	PortName = "metrics"
	// Host is the host address used by Windows metrics
	Host = "0.0.0.0"
	// Port is the port number on which windows-exporter is exposed.
	Port int32 = 9182
	// WindowsMetricsResource is the name for objects created for Prometheus monitoring
	// by current operator version. Its name is defined through the bundle manifests
	WindowsMetricsResource = "windows-exporter"
)

// Config holds the information required to interact with metrics objects
type Config struct {
	// a handle that allows us to interact with the Kubernetes API.
	*kubernetes.Clientset
	// a handle that allows us to interact with the Monitoring API.
	*monclient.MonitoringV1Client
	// namespace is the namespace in which metrics objects are created
	namespace string
	// recorder to generate events
	recorder record.EventRecorder
}

// NewConfig creates a new instance for Config  to be used by the caller.
func NewConfig(mgr manager.Manager, cfg *rest.Config, namespace string) (*Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config should not be nil")
	}
	oclient, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating config client: %w", err)
	}
	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("error creating monitoring client: %w", err)
	}
	return &Config{Clientset: oclient,
		MonitoringV1Client: mclient,
		namespace:          namespace,
		recorder:           mgr.GetEventRecorderFor("metrics"),
	}, nil
}

// Configure takes care of all the required configuration steps
// for Prometheus monitoring like validating monitoring label
// and creating metrics Endpoints object.
func (c *Config) Configure(ctx context.Context) error {
	// validate if cluster monitoring is enabled in the operator namespace
	enabled, err := c.validate(ctx)
	if err != nil {
		return fmt.Errorf("error validating cluster monitoring label: %s", err)
	}
	// Create Metrics Endpoint object only if monitoring is enabled
	if !enabled {
		return nil
	}
	if err := c.ensureServiceMonitor(); err != nil {
		return fmt.Errorf("error ensuring serviceMonitor exists: %w", err)
	}
	// In the case of an operator restart, a previous Endpoint object will be deleted and a new one will
	// be created to ensure we have a correct spec.
	var subsets []v1.EndpointSubset
	existingEndpoint, err := c.CoreV1().Endpoints(c.namespace).Get(ctx, WindowsMetricsResource, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("error retrieving %s endpoint: %w", WindowsMetricsResource, err)
		}
	} else {
		subsets = existingEndpoint.Subsets
		err = c.CoreV1().Endpoints(c.namespace).Delete(ctx, WindowsMetricsResource, metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting %s endpoint: %w", WindowsMetricsResource, err)
		}
	}
	if err := c.createEndpoint(subsets); err != nil {
		return fmt.Errorf("error creating metrics Endpoint: %w", err)
	}
	return nil
}

// validate will verify if cluster monitoring is enabled in the operator namespace. If the label is set to false or not
// present, it will log and send warning events to the user. If the label holds a non-boolean value, returns an error.
func (c *Config) validate(ctx context.Context) (bool, error) {
	// validate if metrics label is added to namespace
	wmcoNamespace, err := c.CoreV1().Namespaces().Get(ctx, c.namespace, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("error getting operator namespace: %w", err)
	}

	labelValue := false
	// if the label exists, update value from default of false
	if value, ok := wmcoNamespace.Labels["openshift.io/cluster-monitoring"]; ok {
		labelValue, err = strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("monitoring label must have a boolean value: %w", err)
		}
	}
	if !labelValue {
		c.recorder.Eventf(wmcoNamespace, v1.EventTypeWarning, "labelValidationFailed",
			"Cluster monitoring openshift.io/cluster-monitoring=true label is not enabled in %s namespace", c.namespace)
	}
	metricsEnabled = labelValue
	return metricsEnabled, nil
}

// createEndpoint creates an endpoint object in the operator namespace.
// WMCO is no longer creating a service with a selector therefore no Endpoint
// object is created and WMCO needs to create the Endpoint object.
// We cannot create endpoints as a part of manifests deployment as
// Endpoints resources are not currently OLM-supported for bundle creation.
func (c *Config) createEndpoint(subsets []v1.EndpointSubset) error {
	// create new Endpoint
	newEndpoint := &v1.Endpoints{
		TypeMeta: metav1.TypeMeta{
			Kind: "Endpoints",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      WindowsMetricsResource,
			Namespace: c.namespace,
			Labels:    map[string]string{"name": WindowsMetricsResource},
		},
		Subsets: subsets,
	}
	_, err := c.CoreV1().Endpoints(c.namespace).Create(context.TODO(),
		newEndpoint, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating metrics Endpoint: %w", err)
	}
	return nil
}

// ensureServiceMonitor creates a serviceMonitor object in the operator namespace if it does not exist.
func (c *Config) ensureServiceMonitor() error {
	// get existing serviceMonitor object if it exists
	existingSM, err := c.ServiceMonitors(c.namespace).Get(context.TODO(), WindowsMetricsResource, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf(
			"error retrieving %s serviceMonitor: %w", WindowsMetricsResource, err)
	}

	serverName := fmt.Sprintf("%s.%s.svc", WindowsMetricsResource, c.namespace)
	instanceLabel := "$1"
	portLabel := "$1:9182"
	jobLabel := WindowsMetricsResource
	attachMetadataBool := true
	expectedSM := &monv1.ServiceMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WindowsMetricsResource,
			Namespace: c.namespace,
			Labels: map[string]string{
				"name": WindowsMetricsResource,
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
							Replacement: &instanceLabel,
							TargetLabel: "instance",
							SourceLabels: []monv1.LabelName{
								"__meta_kubernetes_endpoint_address_target_name",
							},
						},
						{ // Include only Windows nodes for this servicemonitor
							Action: "keep",
							Regex:  "windows",
							SourceLabels: []monv1.LabelName{
								"__meta_kubernetes_node_label_kubernetes_io_os",
							},
						},
						{ // Change the port from the kubelet port 10250 to 9182
							Action:      "replace",
							Regex:       "(.+)(?::\\d+)",
							Replacement: &portLabel,
							TargetLabel: "__address__",
							SourceLabels: []monv1.LabelName{
								"__address__",
							},
						},
						{ // Update the job label from kubelet to windows-exporter
							Action:      "replace",
							Replacement: &jobLabel,
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
		err = c.ServiceMonitors(c.namespace).Delete(context.TODO(), WindowsMetricsResource,
			metav1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("unable to delete service monitor %s/%s: %w", c.namespace, WindowsMetricsResource,
				err)
		}
		log.Info("Deleted malformed resource", "serviceMonitor", WindowsMetricsResource,
			"namespace", c.namespace)
	}

	_, err = c.ServiceMonitors(c.namespace).Create(context.TODO(), expectedSM, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating service monitor: %w", err)
	}
	return nil
}
