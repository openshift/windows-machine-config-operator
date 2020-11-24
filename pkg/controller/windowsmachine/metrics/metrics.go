package metrics

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/operator-framework/operator-sdk/pkg/metrics"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	k8sclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/nodeconfig"
)

var (
	log = logf.Log.WithName("metrics")
	// metricsEnabled specifies if metrics are enabled in the current cluster
	metricsEnabled = true
)

const (
	// windowsMetricsEndpoints is the name of the Endpoints object for Windows metrics
	windowsMetricsEndpoints = "windows-machine-config-operator-metrics"
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

// Add will create the Services and Service Monitors that allows the operator to export the metrics by using
// the Prometheus operator
func Add(ctx context.Context, cfg *rest.Config, namespace string) error {
	// Add to the below struct any other metrics ports you want to expose.
	servicePorts := []v1.ServicePort{
		{Port: Port, Name: PortName, Protocol: v1.ProtocolTCP, TargetPort: intstr.IntOrString{Type: intstr.Int, IntVal: Port}},
	}

	// Create Service object to expose the metrics port(s).
	service, err := metrics.CreateMetricsService(ctx, cfg, servicePorts)
	if err != nil {
		return errors.Wrap(err, "could not create metrics Service")
	}

	// CreateServiceMonitors will automatically create the prometheus-operator ServiceMonitor resources
	// necessary to configure Prometheus to scrape metrics from this operator.
	services := []*v1.Service{service}
	_, err = metrics.CreateServiceMonitors(cfg, namespace, services)
	if err != nil {
		log.Error(err, "could not create ServiceMonitor object")
		// If this operator is deployed to a cluster without the prometheus-operator running, it will return
		// ErrServiceMonitorNotPresent, which can be used to safely skip ServiceMonitor creation.
		if err == metrics.ErrServiceMonitorNotPresent {
			metricsEnabled = false
			return errors.Wrap(err, "install prometheus-operator in your cluster to create ServiceMonitor objects")

		}
	}

	oclient, err := k8sclient.NewForConfig(cfg)
	if err != nil {
		return errors.Wrap(err, "could not create config clientset")
	}
	// When a selector is present in a headless service i.e. spec.ClusterIP=None, Kubernetes manages the
	// list of endpoints reverting all the changes made by the operator. Remove selector from Metrics Service to avoid
	// reverting changes in the Endpoints object.
	patchData := fmt.Sprintf(`{"spec":{"selector": null }}`)
	service, err = oclient.CoreV1().Services(namespace).Patch(ctx, service.Name, types.MergePatchType,
		[]byte(patchData), metav1.PatchOptions{})
	if err != nil {
		return errors.Wrap(err, "could not remove selector from metrics service")
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
		Patch(context.TODO(), windowsMetricsEndpoints, types.JSONPatchType, patchDataBytes, metav1.PatchOptions{})
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
		windowsMetricsEndpoints, metav1.GetOptions{})
	if err != nil {
		return errors.Wrapf(err, "could not get metrics endpoints %v", windowsMetricsEndpoints)
	}

	if !isEndpointsValid(nodes, endpoints) {
		// check if we can get list of endpoint addresses
		windowsIPList := getNodeEndpointAddresses(nodes)
		// sync metrics endpoints object with the current list of addresses
		if err := pc.syncMetricsEndpoint(windowsIPList); err != nil {
			return errors.Wrap(err, "error updating endpoints object with list of endpoint addresses")
		}
	}
	log.Info("Prometheus configured", "endpoints", windowsMetricsEndpoints, "port", Port, "name", PortName)
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
