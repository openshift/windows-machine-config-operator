package nodeconfig

import (
	"context"
	"net/url"
	"strings"

	clientset "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/windows-machine-config-operator/pkg/clusternetwork"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/windows"
	"github.com/openshift/windows-machine-config-operator/version"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	// HybridOverlaySubnet is an annotation applied by the cluster network operator which is used by the hybrid overlay
	HybridOverlaySubnet = "k8s.ovn.org/hybrid-overlay-node-subnet"
	// HybridOverlayMac is an annotation applied by the hybrid-overlay
	HybridOverlayMac = "k8s.ovn.org/hybrid-overlay-distributed-router-gateway-mac"
	// WindowsOSLabel is the label that is applied by WMCB to identify the Windows nodes bootstrapped via WMCB
	WindowsOSLabel = "node.openshift.io/os_id=Windows"
	// WorkerLabel is the label that needs to be applied to the Windows node to make it worker node
	WorkerLabel = "node-role.kubernetes.io/worker"
	// VersionAnnotation indicates the version of WMCO that configured the node
	VersionAnnotation = "windowsmachineconfig.openshift.io/version"
)

// nodeConfig holds the information to make the given VM a kubernetes node. As of now, it holds the information
// related to kubeclient and the windowsVM.
type nodeConfig struct {
	// k8sclientset holds the information related to kubernetes clientset
	k8sclientset *kubernetes.Clientset
	// Windows holds the information related to the windows VM
	windows.Windows
	// Node holds the information related to node object
	node *v1.Node
	// network holds the network information specific to the node
	network *network
	// clusterServiceCIDR holds the service CIDR for cluster
	clusterServiceCIDR string
}

// discoverKubeAPIServerEndpoint discovers the kubernetes api server endpoint from the
func discoverKubeAPIServerEndpoint() (string, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return "", errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return "", errors.Wrap(err, "unable to get client from the given config")
	}

	host, err := client.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "unable to get cluster infrastructure resource")
	}
	// get API server internal url of format https://api-int.abc.devcluster.openshift.com:6443
	if host.Status.APIServerInternalURL == "" {
		return "", errors.Wrap(err, "could not get host name for the kubernetes api server")
	}
	return host.Status.APIServerInternalURL, nil
}

// NewNodeConfig creates a new instance of nodeConfig to be used by the caller.
func NewNodeConfig(clientset *kubernetes.Clientset, ipAddress, providerName, instanceID, clusterServiceCIDR, vxlanPort string,
	signer ssh.Signer) (*nodeConfig, error) {
	var err error
	if nodeConfigCache.workerIgnitionEndPoint == "" {
		var kubeAPIServerEndpoint string
		// We couldn't find it in cache. Let's compute it now.
		kubeAPIServerEndpoint, err = discoverKubeAPIServerEndpoint()
		if err != nil {
			return nil, errors.Wrap(err, "unable to find kube api server endpoint")
		}
		clusterAddress, err := getClusterAddr(kubeAPIServerEndpoint)
		if err != nil {
			return nil, errors.Wrap(err, "error getting cluster address")
		}
		workerIgnitionEndpoint := "https://" + clusterAddress + ":22623/config/worker"
		nodeConfigCache.workerIgnitionEndPoint = workerIgnitionEndpoint
	}
	if err = clusternetwork.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, errors.Wrap(err, "error receiving valid CIDR value for "+
			"creating new node config")
	}

	win, err := windows.New(ipAddress, providerName, instanceID, nodeConfigCache.workerIgnitionEndPoint, vxlanPort, signer)
	if err != nil {
		return nil, errors.Wrap(err, "error instantiating Windows instance from VM")
	}

	return &nodeConfig{k8sclientset: clientset, Windows: win, network: newNetwork(),
		clusterServiceCIDR: clusterServiceCIDR}, nil
}

// getClusterAddr gets the cluster address associated with given kubernetes APIServerEndpoint.
// For example: https://api-int.abc.devcluster.openshift.com:6443 gets translated to
// api-int.abc.devcluster.openshift.com
// TODO: Think if this needs to be removed as this is too restrictive. Imagine apiserver behind
// 		a loadbalancer.
// 		Jira story: https://issues.redhat.com/browse/WINC-398
func getClusterAddr(kubeAPIServerEndpoint string) (string, error) {
	clusterEndPoint, err := url.Parse(kubeAPIServerEndpoint)
	if err != nil {
		return "", errors.Wrap(err, "unable to parse the kubernetes API server endpoint")
	}
	hostName := clusterEndPoint.Hostname()

	// Check if hostname is valid
	if !strings.HasPrefix(hostName, "api-int.") {
		return "", errors.New("invalid API server url format found: expected hostname to start with `api-int.`")
	}
	return hostName, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	if err := nc.Windows.Configure(); err != nil {
		return errors.Wrap(err, "configuring the Windows VM failed")
	}
	// populate node object in nodeConfig
	if err := nc.setNode(); err != nil {
		return errors.Wrapf(err, "error getting node object for VM %s", nc.ID())
	}
	// Now that basic kubelet configuration is complete, configure networking in the node
	if err := nc.configureNetwork(); err != nil {
		return errors.Wrap(err, "configuring node network failed")
	}

	// Now that the node has been fully configured, add the version annotation to signify that the node
	// was successfully configured by this version of WMCO
	// populate node object in nodeConfig once more
	if err := nc.setNode(); err != nil {
		return errors.Wrapf(err, "error getting node object for VM %s", nc.ID())
	}
	nc.addVersionAnnotation()
	node, err := nc.k8sclientset.CoreV1().Nodes().Update(context.TODO(), nc.node, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "error updating node labels and annotations")
	}
	nc.node = node

	return nil
}

// configureNetwork configures k8s networking in the node
// we are assuming that the WindowsVM and node objects are valid
func (nc *nodeConfig) configureNetwork() error {
	// Wait until the node object has the hybrid overlay subnet annotation. Otherwise the hybrid-overlay will fail to
	// start
	if err := nc.waitForNodeAnnotation(HybridOverlaySubnet); err != nil {
		return errors.Wrapf(err, "error waiting for %s node annotation for %s", HybridOverlaySubnet,
			nc.node.GetName())
	}

	// NOTE: Investigate if we need to introduce a interface wrt to the VM's networking configuration. This will
	// become more clear with the outcome of https://issues.redhat.com/browse/WINC-343

	// Configure the hybrid overlay in the Windows VM
	if err := nc.Windows.ConfigureHybridOverlay(nc.node.GetName()); err != nil {
		return errors.Wrapf(err, "error configuring hybrid overlay for %s", nc.node.GetName())
	}

	// Wait until the node object has the hybrid overlay MAC annotation. This is required for the CNI configuration to
	// start.
	if err := nc.waitForNodeAnnotation(HybridOverlayMac); err != nil {
		return errors.Wrapf(err, "error waiting for %s node annotation for %s", HybridOverlayMac,
			nc.node.GetName())
	}

	// Configure CNI in the Windows VM
	if err := nc.configureCNI(); err != nil {
		return errors.Wrapf(err, "error configuring CNI for %s", nc.node.GetName())
	}
	// Start the kube-proxy service
	if err := nc.Windows.ConfigureKubeProxy(nc.node.GetName(), nc.node.Annotations[HybridOverlaySubnet]); err != nil {
		return errors.Wrapf(err, "error starting kube-proxy for %s", nc.node.GetName())
	}
	return nil
}

// addVersionAnnotation adds the version annotation to nc.node
func (nc *nodeConfig) addVersionAnnotation() {
	nc.node.Annotations[VersionAnnotation] = version.Get()
}

// setNode identifies the node from the instanceID provided and sets the node object in the nodeconfig.
func (nc *nodeConfig) setNode() error {
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		nodes, err := nc.k8sclientset.CoreV1().Nodes().List(context.TODO(),
			metav1.ListOptions{LabelSelector: WindowsOSLabel})
		if err != nil {
			log.V(1).Info("node object listing failed for", "Machine ID", nc.ID(), "error", err)
			return false, nil
		}
		if len(nodes.Items) == 0 {
			log.V(1).Info("got empty node list for", "Machine ID", nc.ID(), "error", err)
			return false, nil
		}
		// get the node with given instance id
		for _, node := range nodes.Items {
			if nc.ID() == getInstanceIDfromProviderID(node.Spec.ProviderID) {
				nc.node = &node
				return true, nil
			}
		}
		return false, nil
	})
	return errors.Wrapf(err, "unable to find node for instanceID %s", nc.ID())
}

// waitForNodeAnnotation checks if the node object has the given annotation and waits for retry.Interval seconds and
// returns an error if the annotation does not appear in that time frame.
func (nc *nodeConfig) waitForNodeAnnotation(annotation string) error {
	nodeName := nc.node.GetName()
	var found bool
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		node, err := nc.k8sclientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			log.V(1).Info("error getting node object associated with", "Machine ID", nc.ID(), "error", err)
			return false, nil
		}
		_, found := node.Annotations[annotation]
		if found {
			//update node to avoid staleness
			nc.node = node
			return true, nil
		}
		return false, nil
	})

	if !found {
		return errors.Wrapf(err, "timeout waiting for %s node annotation", annotation)
	}
	return nil
}

// configureCNI populates the CNI config template and sends the config file location
// for completing CNI configuration in the windows VM
func (nc *nodeConfig) configureCNI() error {
	// set the hostSubnet value in the network struct
	if err := nc.network.setHostSubnet(nc.node.Annotations[HybridOverlaySubnet]); err != nil {
		return errors.Wrapf(err, "error populating host subnet in node network")
	}
	// populate the CNI config file with the host subnet and the service network CIDR
	configFile, err := nc.network.populateCniConfig(nc.clusterServiceCIDR, wkl.CNIConfigTemplatePath)
	if err != nil {
		return errors.Wrapf(err, "error populating CNI config file %s", configFile)
	}
	// configure CNI in the Windows VM
	if err = nc.Windows.ConfigureCNI(configFile); err != nil {
		return errors.Wrapf(err, "error configuring CNI for %s", nc.node.GetName())
	}
	if err = nc.network.cleanupTempConfig(configFile); err != nil {
		log.Error(err, " could not delete temp CNI config file", "configFile",
			configFile)
	}
	return nil
}

// getInstanceIDfromProviderID gets the instanceID of VM for a given cloud provider ID
// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
func getInstanceIDfromProviderID(providerID string) string {
	providerTokens := strings.Split(providerID, "/")
	return providerTokens[len(providerTokens)-1]
}
