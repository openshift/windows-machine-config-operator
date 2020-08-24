package clusternetwork

import (
	"context"
	"fmt"
	"net"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ovnKubernetesNetwork = "OVNKubernetes"

// ClusterNetworkConfig interface contains methods to validate network configuration of a cluster
type ClusterNetworkConfig interface {
	Validate() error
	GetServiceCIDR() (string, error)
	VXLANPort() string
}

// networkType holds information for a required network type
type networkType struct {
	// name describes value of the Network Type
	name string
	// operatorClient is the OpenShift operator client, we will use to interact with OpenShift operator objects
	operatorClient operatorv1.OperatorV1Interface
}

// clusterNetworkCfg struct holds the information for the cluster network
type clusterNetworkCfg struct {
	// serviceCIDR holds the value for cluster network service CIDR
	serviceCIDR string
	// vxlanPort is the port to be used for VXLAN communication
	vxlanPort string
}

// ovnKubernetes contains information specific to network type OVNKubernetes
type ovnKubernetes struct {
	networkType
	clusterNetworkConfig *clusterNetworkCfg
}

// NetworkConfigurationFactory is a factory method that returns information specific to network type
func NetworkConfigurationFactory(oclient configclient.Interface, operatorClient operatorv1.OperatorV1Interface) (ClusterNetworkConfig, error) {
	network, err := getNetworkType(oclient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting cluster network type")
	}

	// retrieve serviceCIDR using cluster config required for cni configurations
	serviceCIDR, err := getServiceNetworkCIDR(oclient)
	if err != nil || serviceCIDR == "" {
		return nil, errors.Wrap(err, "error getting service network CIDR")
	}

	// retrieve the VXLAN port using cluster config
	vxlanPort, err := getVXLANPort(operatorClient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting the custom vxlan port")
	}

	clusterNetworkCfg, err := NewClusterNetworkCfg(serviceCIDR, vxlanPort)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting cluster network config")
	}
	switch network {
	case ovnKubernetesNetwork:
		return &ovnKubernetes{
			networkType{
				name:           network,
				operatorClient: operatorClient,
			},
			clusterNetworkCfg,
		}, nil
	default:
		return nil, errors.Errorf("%s : network type not supported", network)
	}
}

// NewClusterNetworkCfg assigns a serviceCIDR value and returns a pointer to the clusterNetworkCfg struct
func NewClusterNetworkCfg(serviceCIDR, vxlanPort string) (*clusterNetworkCfg, error) {
	if serviceCIDR == "" {
		return nil, errors.Errorf("can't instantiate cluster network config" +
			"with empty service CIDR value")
	}
	return &clusterNetworkCfg{
		serviceCIDR: serviceCIDR,
		vxlanPort:   vxlanPort,
	}, nil
}

// GetServiceCIDR returns the serviceCIDR string
func (ovn *ovnKubernetes) GetServiceCIDR() (string, error) {
	return ovn.clusterNetworkConfig.serviceCIDR, nil
}

// GetVXLANPort gets the VXLAN port to be used for VXLAN tunnel establishment
func (ovn *ovnKubernetes) VXLANPort() string {
	return ovn.clusterNetworkConfig.vxlanPort
}

// Validate for OVN Kubernetes checks for network type and hybrid overlay.
func (ovn *ovnKubernetes) Validate() error {
	//check if hybrid overlay is enabled for the cluster
	networkCR, err := ovn.operatorClient.Networks().Get(context.TODO(), "cluster", metav1.GetOptions{})
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
	networkCR, err := oclient.ConfigV1().Networks().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "error getting cluster network object")
	}
	return networkCR.Spec.NetworkType, nil
}

// getServiceNetworkCIDR gets the serviceCIDR using cluster config required for cni configuration
func getServiceNetworkCIDR(oclient configclient.Interface) (string, error) {
	// Get the cluster network object so that we can find the service network
	networkCR, err := oclient.ConfigV1().Networks().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "error getting cluster network object")
	}
	if len(networkCR.Spec.ServiceNetwork) == 0 {
		return "", errors.Wrapf(err, "error getting cluster service CIDR,"+
			"received empty value for service networks")
	}
	serviceCIDR := networkCR.Spec.ServiceNetwork[0]
	if ValidateCIDR(serviceCIDR) != nil {
		return "", errors.Wrapf(err, "invalid cluster service CIDR %s", serviceCIDR)
	}
	return serviceCIDR, nil
}

// getVXLANPort gets the VXLAN port to establish tunnel as a string. The return type doesn't matter as we want to pass
// this argument to a powershell command
func getVXLANPort(operatorClient operatorv1.OperatorV1Interface) (string, error) {
	// Get the cluster network object so that we can find the service network
	networkCR, err := operatorClient.Networks().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "error getting cluster network object")
	}
	var vxlanPort *uint32
	if networkCR.Spec.DefaultNetwork.OVNKubernetesConfig != nil &&
		networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig != nil &&
		networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig.HybridOverlayVXLANPort != nil {
		vxlanPort = networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig.HybridOverlayVXLANPort
		return fmt.Sprint(*vxlanPort), nil
	}
	return "", nil
}

// ValidateCIDR uses the parseCIDR from network package to validate the format of the CIDR
func ValidateCIDR(cidr string) error {
	_, _, err := net.ParseCIDR(cidr)
	if err != nil || cidr == "" {
		return errors.Wrapf(err, "received invalid CIDR value %s", cidr)
	}
	return nil
}
