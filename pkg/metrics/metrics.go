package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pkg/errors"
	monclient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
)

//+kubebuilder:rbac:groups="",resources=services;services/finalizers,verbs=create;get;delete
//+kubebuilder:rbac:groups="",resources=endpoints,verbs=create;get;delete;update;patch
//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get
//+kubebuilder:rbac:groups="",resources=nodes,verbs=list
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;delete
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

// PrometheusNodeConfig holds the information required to configure Prometheus, so that it can scrape metrics from the
// given endpoint address
type PrometheusNodeConfig struct {
	// k8sclientset is a handle that allows us to interact with the Kubernetes API.
	k8sclientset *kubernetes.Clientset
	// namespace is the namespace in which metrics endpoints object is created
	namespace string
}

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

// patchEndpoint contains information regarding patching metrics Endpoint
type patchEndpoint struct {
	// op defines patch operation to be performed on the Endpoints object
	Op string `json:"op"`
	// path defines the location of the patch
	Path string `json:"path"`
	// value defines the data to be patched
	Value []v1.EndpointSubset `json:"value"`
}

// NewPrometheuopsNodeConfig creates a new instance for prometheusNodeConfig  to be used by the caller.
func NewPrometheusNodeConfig(clientset *kubernetes.Clientset, watchNamespace string) (*PrometheusNodeConfig, error) {

	return &PrometheusNodeConfig{
		k8sclientset: clientset,
		namespace:    watchNamespace,
	}, nil
}

// NewConfig creates a new instance for Config  to be used by the caller.
func NewConfig(mgr manager.Manager, cfg *rest.Config, namespace string) (*Config, error) {
	if cfg == nil {
		return nil, errors.New("config should not be nil")
	}
	oclient, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error creating config client")
	}
	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "error creating monitoring client")
	}
	return &Config{Clientset: oclient,
		MonitoringV1Client: mclient,
		namespace:          namespace,
		recorder:           mgr.GetEventRecorderFor("metrics"),
	}, nil
}

// syncMetricsEndpoint updates the endpoint object with the new list of IP addresses from the Windows nodes and the
// metrics port.
func (pc *PrometheusNodeConfig) syncMetricsEndpoint(nodeEndpointAdressess []v1.EndpointAddress) error {
	// Update EndpointSubset field with list of Windows Nodes endpoint addresses and required metrics port information
	// We need to patch the entire endpoint subset field, since addresses and ports both fields are deleted when there
	// are no Windows nodes.
	var subsets []v1.EndpointSubset
	if nodeEndpointAdressess != nil {
		subsets = []v1.EndpointSubset{{
			Addresses: nodeEndpointAdressess,
			Ports: []v1.EndpointPort{{
				Name:     PortName,
				Port:     Port,
				Protocol: v1.ProtocolTCP,
			}},
		}}
	}

	patchData := []patchEndpoint{{
		Op:    "replace",
		Path:  "/subsets",
		Value: subsets,
	}}
	// convert patch data to bytes
	patchDataBytes, err := json.Marshal(patchData)
	if err != nil {
		return errors.Wrap(err, "unable to get patch data in bytes")
	}

	_, err = pc.k8sclientset.CoreV1().Endpoints(pc.namespace).
		Patch(context.TODO(), WindowsMetricsResource, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
	return errors.Wrap(err, "unable to sync metrics endpoints")
}

// Configure patches the endpoint object to reflect the current list Windows nodes.
func (pc *PrometheusNodeConfig) Configure() error {
	// Check if metrics are enabled in current cluster
	if !metricsEnabled {
		log.Info("install the prometheus-operator to enable Prometheus configuration")
		return nil
	}
	// get list of Windows nodes that are in Ready phase
	nodes, err := pc.k8sclientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel,
		FieldSelector: "spec.unschedulable=false"})
	if err != nil {
		return errors.Wrap(err, "could not get Windows nodes")
	}

	// get Metrics Endpoints object
	endpoints, err := pc.k8sclientset.CoreV1().Endpoints(pc.namespace).Get(context.TODO(),
		WindowsMetricsResource, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "could not get metrics endpoints %v", WindowsMetricsResource)
	}

	if !isEndpointsValid(nodes, endpoints) {
		// check if we can get list of endpoint addresses
		windowsIPList := getNodeEndpointAddresses(nodes)
		// sync metrics endpoints object with the current list of addresses
		if err := pc.syncMetricsEndpoint(windowsIPList); err != nil {
			return errors.Wrap(err, "error updating endpoints object with list of endpoint addresses")
		}
	}
	log.Info("Prometheus configured", "endpoints", WindowsMetricsResource, "port", Port, "name", PortName)
	return nil
}

// getNodeEndpointAddresses returns a list of endpoint addresses according to the given list of Windows nodes
func getNodeEndpointAddresses(nodes *v1.NodeList) []v1.EndpointAddress {
	// an empty list to store node IP addresses
	var nodeIPAddress []v1.EndpointAddress
	// loops through nodes
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == "InternalIP" && address.Address != "" {
				// add IP address address to the endpoint address list
				nodeIPAddress = append(nodeIPAddress, v1.EndpointAddress{
					IP:       address.Address,
					Hostname: "",
					NodeName: nil,
					TargetRef: &v1.ObjectReference{
						Kind: "Node",
						Name: node.Name,
					},
				})
				break
			}
		}
	}
	return nodeIPAddress
}

// isEndpointsValid returns true if Endpoints object has entries for all the Windows nodes in the cluster.
// It returns false when any one of the Windows nodes is not present in the subset.
func isEndpointsValid(nodes *v1.NodeList, endpoints *v1.Endpoints) bool {
	// check if number of entries in endpoints object match number of Ready Windows nodes
	if len(endpoints.Subsets) == 0 || len(nodes.Items) != len(endpoints.Subsets[0].Addresses) {
		return false
	}

	for _, node := range nodes.Items {
		nodeFound := false
		for _, address := range endpoints.Subsets[0].Addresses {
			if address.TargetRef.Name == node.Name {
				nodeFound = true
				break
			}
		}
		if !nodeFound {
			return false
		}
	}
	return true
}

// Configure takes care of all the required configuration steps
// for Prometheus monitoring like validating monitoring label
// and creating metrics Endpoints object.
func (c *Config) Configure(ctx context.Context) error {
	// validate if cluster monitoring is enabled in the operator namespace
	enabled, err := c.validate(ctx)
	if err != nil {
		return errors.Wrap(err, "error validating cluster monitoring label")
	}
	// Create Metrics Endpoint object only if monitoring is enabled
	if !enabled {
		return nil
	}
	// In the case of an operator restart, a previous Endpoint object will be deleted and a new one will
	// be created to ensure we have a correct spec.
	err = c.CoreV1().Endpoints(c.namespace).Delete(ctx, WindowsMetricsResource, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return errors.Wrap(err, "error deleting existing metrics Endpoint")
	}
	if err := c.createEndpoint(); err != nil {
		return errors.Wrap(err, "error creating metrics Endpoint")
	}
	return nil
}

// validate will verify if cluster monitoring is enabled in the operator namespace.
// If the label is not present, it will log and send warning events to the user.
func (c *Config) validate(ctx context.Context) (bool, error) {
	// validate if metrics label is added to namespace
	wmcoNamespace, err := c.CoreV1().Namespaces().Get(ctx, c.namespace, metav1.GetOptions{})
	if err != nil {
		return false, errors.Wrap(err, "error getting operator namespace")
	}
	if wmcoNamespace.Labels["openshift.io/cluster-monitoring"] != "true" {
		metricsEnabled = false
		c.recorder.Eventf(wmcoNamespace, v1.EventTypeWarning, "labelValidationFailed",
			"Cluster monitoring openshift.io/cluster-monitoring=true label is not enabled in %s namespace", c.namespace)
		return false, nil
	}
	return true, nil
}

// RemoveStaleResources deletes stale resources that could be left behind during the
// upgrade process due to the renaming of resources between current and previous operator version
func (c *Config) RemoveStaleResources(ctx context.Context) {
	wmcoNamespace, err := c.CoreV1().Namespaces().Get(ctx, c.namespace, metav1.GetOptions{})
	if err != nil {
		log.Error(err, "error getting operator namespace")
	}

	operatorName, err := getOperatorName()
	if err != nil {
		log.Error(err, "error getting operator name")
	}

	// staleResourceName is the metrics object name created for Prometheus monitoring by previous operator versions
	staleResourceName := operatorName + "-metrics"

	// remove stale service objects
	err = c.CoreV1().Services(c.namespace).Delete(ctx, staleResourceName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.recorder.Event(wmcoNamespace, v1.EventTypeWarning, "serviceDeletionFailed",
			"Stale service deletion failure")
		log.Error(err, "error deleting stale service object")
	}

	// remove stale endpoint objects
	err = c.CoreV1().Endpoints(c.namespace).Delete(ctx, staleResourceName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.recorder.Event(wmcoNamespace, v1.EventTypeWarning, "endpointDeletionFailed",
			"Stale endpoint deletion failure")
		log.Error(err, "error deleting stale endpoint object")
	}

	// remove stale serviceMonitor objects
	err = c.ServiceMonitors(c.namespace).Delete(ctx, staleResourceName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		c.recorder.Event(wmcoNamespace, v1.EventTypeWarning, "serviceMonitorDeletionFailed",
			"Stale serviceMonitor deletion failure")
		log.Error(err, "error deleting stale serviceMonitor object")
	}
}

// createEndpoint creates an endpoint object in the operator namespace.
// WMCO is no longer creating a service with a selector therefore no Endpoint
// object is created and WMCO needs to create the Endpoint object.
// We cannot create endpoints as a part of manifests deployment as
// Endpoints resources are not currently OLM-supported for bundle creation.
func (c *Config) createEndpoint() error {
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
		Subsets: nil,
	}
	_, err := c.CoreV1().Endpoints(c.namespace).Create(context.TODO(),
		newEndpoint, metav1.CreateOptions{})
	if err != nil {
		return errors.Wrap(err, "error creating metrics Endpoint")
	}
	return nil
}

// getOperatorName returns the name of the operator
func getOperatorName() (string, error) {
	var operatorNameEnvVar = "OPERATOR_NAME"

	name, found := os.LookupEnv(operatorNameEnvVar)
	if !found {
		return "", fmt.Errorf("%s must be set", operatorNameEnvVar)
	}
	return name, nil
}
