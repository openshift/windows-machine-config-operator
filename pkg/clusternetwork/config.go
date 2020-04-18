package clusternetwork

import (
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ovnKubernetesNetwork = "OVNKubernetes"

// ClusterNetworkConfig interface contains methods to validate network configuration of a cluster
type ClusterNetworkConfig interface {
	Validate() error
}

// networkType information for a required network type
type networkType struct {
	// name describes value of the Network Type
	name string
	// oclient is the OpenShift config client, we will use to interact with the OpenShift API
	oclient configclient.Interface
	// operatorClient is the OpenShift operator client, we will use to interact with OpenShift operator objects
	operatorClient operatorv1.OperatorV1Interface
}

// ovnKubernetes contains information specific to network type OVNKubernetes
type ovnKubernetes struct {
	networkType
}

// NetworkConfigurationFactory is a factory method that returns information specific to network type
func NetworkConfigurationFactory(oclient configclient.Interface, operatorClient operatorv1.OperatorV1Interface) (ClusterNetworkConfig, error) {
	network, err := getNetworkType(oclient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting cluster network type")
	}
	switch network {
	case ovnKubernetesNetwork:
		return &ovnKubernetes{
			networkType{
				name:           network,
				oclient:        oclient,
				operatorClient: operatorClient,
			},
		}, nil
	default:
		return nil, errors.Errorf("%s : network type not supported", network)
	}
}

// Validate for OVN Kubernetes checks for network type and hybrid overlay.
func (ovn *ovnKubernetes) Validate() error {
	//check if hybrid overlay is enabled for the cluster
	networkCR, err := ovn.operatorClient.Networks().Get("cluster", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(err, "error getting cluster network.operator object")
	}

	defaultNetwork := networkCR.Spec.DefaultNetwork
	if defaultNetwork.OVNKubernetesConfig == nil || defaultNetwork.OVNKubernetesConfig.HybridOverlayConfig == nil {
		return errors.New("cluster is not configured for OVN hybrid networking")
	}

	if len(networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig.HybridClusterNetwork) == 0 {
		return errors.New("invalid OVN hybrid networking configuration")
	}
	return nil
}

// getNetworkType returns network type of the cluster
func getNetworkType(oclient configclient.Interface) (string, error) {
	// Get the cluster network object so that we can find the network type
	networkCR, err := oclient.ConfigV1().Networks().Get("cluster", metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "error getting cluster network object")
	}
	return networkCR.Spec.NetworkType, nil
}
