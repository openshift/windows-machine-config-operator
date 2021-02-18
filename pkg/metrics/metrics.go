package metrics

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/pkg/errors"
	monclient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
)

var (
	log = logf.Log.WithName("metrics")
	// metricsEnabled specifies if metrics are enabled in the current cluster
	metricsEnabled = true
	// windowsMetricsResource is the name of an object created for Windows metrics
	windowsMetricsResource = ""
)

const (
	// metricsPortName specifies the portname used for Prometheus monitoring
	PortName = "metrics"
	// Host is the host address used by Windows metrics
	Host = "0.0.0.0"
	// Port is the port number on which windows-exporter is exposed.
	Port int32 = 9182
)

// PrometheusNodeConfig holds the information required to configure Prometheus, so that it can scrape metrics from the
// given endpoint address
type PrometheusNodeConfig struct {
	// k8sclientset is a handle that allows us to interact with the Kubernetes API.
	k8sclientset *kubernetes.Clientset
	// namespace is the namespace in which metrics endpoints object is created
	namespace string
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
func NewPrometheusNodeConfig(clientset *kubernetes.Clientset) (*PrometheusNodeConfig, error) {
	windowsMetricsEndpointsNamespace, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		return nil, err
	}

	return &PrometheusNodeConfig{
		k8sclientset: clientset,
		namespace:    windowsMetricsEndpointsNamespace,
	}, err
}

// Validate will create the Services and Service Monitors that allows the operator to export the metrics by using
// the Prometheus operator
func Validate(ctx context.Context, cfg *rest.Config, namespace string) error {
	oclient, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "could not create config clientset")
	}

	// Validate if metrics label is added to namespace
	wincNamespace, err := oclient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if wincNamespace.Labels["openshift.io/cluster-monitoring"] != "true" {
		metricsEnabled = false
	}

	// Check if metrics service exists
	serviceList, err := oclient.CoreV1().Services(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=windows-exporter"})
	if err != nil || len(serviceList.Items) == 0 {
		metricsEnabled = false
		return errors.Wrap(err, "could not get metrics Service")
	}

	// the name for the metrics resources is set during creation of metrics service and is equivalent to the service name
	windowsMetricsResource = serviceList.Items[0].Name

	// Create a monitoring client to interact with the ServiceMonitor object
	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "could not create monitoring client")
	}
	// Check if Service Monitor exists.
	_, err = mclient.ServiceMonitors(namespace).Get(ctx, windowsMetricsResource, metav1.GetOptions{})
	if err != nil {
		metricsEnabled = false
		return errors.Wrap(err, "could not get metrics Service")
	}

	return nil
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
		Patch(context.TODO(), windowsMetricsResource, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
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
		windowsMetricsResource, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "could not get metrics endpoints %v", windowsMetricsResource)
	}

	if !isEndpointsValid(nodes, endpoints) {
		// check if we can get list of endpoint addresses
		windowsIPList := getNodeEndpointAddresses(nodes)
		// sync metrics endpoints object with the current list of addresses
		if err := pc.syncMetricsEndpoint(windowsIPList); err != nil {
			return errors.Wrap(err, "error updating endpoints object with list of endpoint addresses")
		}
	}
	log.Info("Prometheus configured", "endpoints", windowsMetricsResource, "port", Port, "name", PortName)
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

// updateServiceMonitors patches the metrics Service Monitor to include required fields to display node graphs on the
// OpenShift console. Console graph queries require metrics endpoint target name to be node name, however
// windows_exporter returns node IP. We replace the target name by adding `replace` action field to the ServiceMonitor
// object that replaces node IP to node name as the metrics endpoint target.
func updateServiceMonitors(cfg *rest.Config, namespace string) error {

	patchData := fmt.Sprintf("[{\"op\": \"replace\", \"path\": \"/spec/endpoints/0\", "+
		"\"value\":{\"path\": \"/%s\",\"port\": \"%s\",\"relabelings\": [{\"action\": \"replace\", \"regex\": \"(.*)\", "+
		"\"replacement\": \"$1\", \"sourceLabels\": [\"__meta_kubernetes_endpoint_address_target_name\"],"+
		"\"targetLabel\": \"instance\"}]}}]", PortName, PortName)

	mclient, err := monclient.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "error creating monitoring client")
	}
	_, err = mclient.ServiceMonitors(namespace).Patch(context.TODO(), windowsMetricsResource, types.JSONPatchType, []byte(patchData),
		metav1.PatchOptions{})
	if err != nil {
		return errors.Wrap(err, "unable to patch service monitor")
	}
	return nil
}
