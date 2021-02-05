package cluster

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	oconfig "github.com/openshift/api/config/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	operatorv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/pkg/errors"
	"golang.org/x/mod/semver"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	ovnKubernetesNetwork = "OVNKubernetes"
	// baseK8sVersion specifies the base k8s version supported by the operator. (For eg. All versions in the format
	// 1.20.x are supported for baseK8sVersion 1.20)
	baseK8sVersion = "v1.20"
)

// Network interface contains methods to interact with cluster network objects
type Network interface {
	Validate() error
	GetServiceCIDR() (string, error)
	VXLANPort() string
}

// Config interface contains methods to expose cluster config related information
type Config interface {
	// Validate checks if the cluster configurations are as required.
	Validate() error
	// Platform returns cloud provider on which OpenShift is running
	Platform() oconfig.PlatformType
	// Network returns network configuration for the OpenShift cluster
	Network() Network
}

// networkType holds information for a required network type
type networkType struct {
	// name describes value of the Network Type
	name string
	// operatorClient is the OpenShift operator client, we will use to interact with OpenShift operator objects
	operatorClient operatorv1.OperatorV1Interface
}

// config encapsulates cluster configuration
type config struct {
	// oclient is the OpenShift config client that will be used to interact with the OpenShift API
	oclient configclient.Interface
	// operatorClient is the OpenShift operator client that that will be used to interact with operator APIs
	operatorClient operatorv1.OperatorV1Interface
	// network is the interface containing information on cluster network
	network Network
	// platform indicates the cloud on which OpenShift cluster is running
	// TODO: Remove this once we figure out how to be provider agnostic
	platform oconfig.PlatformType
}

func (c *config) Platform() oconfig.PlatformType {
	return c.platform
}

func (c *config) Network() Network {
	return c.network
}

// NewConfig returns a Config struct pertaining to the cluster configuration
func NewConfig(restConfig *rest.Config) (Config, error) {
	// get OpenShift API config client.
	oclient, err := configclient.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "could not create config clientset")
	}

	// get OpenShift API operator client
	operatorClient, err := operatorv1.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "could not create operator clientset")
	}

	// get cluster network configurations
	network, err := networkConfigurationFactory(oclient, operatorClient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting cluster network")
	}

	// get the platform type here
	infra, err := oclient.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "error getting cluster network")
	}
	platformStatus := infra.Status.PlatformStatus
	if platformStatus == nil {
		return nil, errors.New("error getting infrastructure status")
	}
	if len(platformStatus.Type) == 0 {
		return nil, errors.New("error getting platform type")
	}
	return &config{
		oclient:        oclient,
		operatorClient: operatorClient,
		network:        network,
		platform:       platformStatus.Type,
	}, nil
}

// validateK8sVersion checks for valid k8s version in the cluster. It returns an error for all versions that are not in
// range of given base version(x.y.z) and x.y+1.z version.
func (c *config) validateK8sVersion() error {
	versionInfo, err := c.oclient.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrap(err, "error retrieving server version ")
	}
	// split the version in the form Major.Minor. For e.g v1.18.0-rc.1 -> v1.18
	clusterBaseVersion := semver.MajorMinor(versionInfo.GitVersion)
	// Convert base version to float and add 1 to Minor version
	baseVersion, err := strconv.ParseFloat(strings.TrimPrefix(baseK8sVersion, "v"), 64)
	if err != nil {
		return errors.Wrapf(err, "error converting %s k8s version to float", baseK8sVersion)
	}
	maxK8sVersion := fmt.Sprintf("v%.2f", baseVersion+0.01)

	// validate cluster version is in the range of baseK8sVersion and maxK8sVersion
	if semver.Compare(clusterBaseVersion, baseK8sVersion) >= 0 && semver.Compare(clusterBaseVersion, maxK8sVersion) <= 0 {
		return nil
	}

	return errors.Errorf("Unsupported server version: %v. Supported versions are %v.x to %v.x", versionInfo.GitVersion,
		baseK8sVersion, maxK8sVersion)
}

// Validate method checks if the cluster configurations are as required. It throws an error if the configuration could not
// be validated.
func (c *config) Validate() error {
	err := c.validateK8sVersion()
	if err != nil {
		return errors.Wrap(err, "error validating k8s version")
	}
	if err = c.network.Validate(); err != nil {
		return errors.Wrap(err, "error validating network configuration")
	}
	return nil
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

// networkConfigurationFactory is a factory method that returns information specific to network type
func networkConfigurationFactory(oclient configclient.Interface, operatorClient operatorv1.OperatorV1Interface) (Network, error) {
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
	networkCR, err := ovn.operatorClient.Networks().Get(context.TODO(), "cluster", meta.GetOptions{})
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
	networkCR, err := oclient.ConfigV1().Networks().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "error getting cluster network object")
	}
	return networkCR.Spec.NetworkType, nil
}

// getServiceNetworkCIDR gets the serviceCIDR using cluster config required for cni configuration
func getServiceNetworkCIDR(oclient configclient.Interface) (string, error) {
	// Get the cluster network object so that we can find the service network
	networkCR, err := oclient.ConfigV1().Networks().Get(context.TODO(), "cluster", meta.GetOptions{})
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
	networkCR, err := operatorClient.Networks().Get(context.TODO(), "cluster", meta.GetOptions{})
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
