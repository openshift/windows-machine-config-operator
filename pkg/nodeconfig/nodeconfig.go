package nodeconfig

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	clientset "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubectl/pkg/drain"
	ctrl "sigs.k8s.io/controller-runtime"
	crclientcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
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
	// PubKeyHashAnnotation corresponds to the public key present on the VM
	PubKeyHashAnnotation = "windowsmachineconfig.openshift.io/pub-key-hash"
)

// nodeConfig holds the information to make the given VM a kubernetes node. As of now, it holds the information
// related to kubeclient and the windowsVM.
type nodeConfig struct {
	// k8sclientset holds the information related to kubernetes clientset
	k8sclientset *kubernetes.Clientset
	// Windows holds the information related to the windows VM
	windows.Windows
	// Node holds the information related to node object
	node *core.Node
	// network holds the network information specific to the node
	network *network
	// publicKeyHash is the hash of the public key present on the VM
	publicKeyHash string
	// clusterServiceCIDR holds the service CIDR for cluster
	clusterServiceCIDR string
	log                logr.Logger
	// additionalAnnotations are extra annotations that should be applied to configured nodes
	additionalAnnotations map[string]string
}

// discoverKubeAPIServerEndpoints discovers the kubernetes api server and internal endpoints URL
func discoverKubeAPIServerEndpoints() (string, string, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return "", "", errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return "", "", errors.Wrap(err, "unable to get client from the given config")
	}

	infra, err := client.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return "", "", errors.Wrap(err, "unable to get cluster infrastructure resource")
	}

	apiServerURL := infra.Status.APIServerURL
	// get API server URL, usually with format https://api.<cluster_name>.<base_domain>:6443
	if apiServerURL == "" {
		return "", "", errors.Wrap(err, "unable to get api server URL from cluster infrastructure")
	}

	apiServerInternalURL := infra.Status.APIServerInternalURL
	// get API server internal URL, usually with format https://api-int.<cluster_name>.<base_domain>:6443
	if apiServerInternalURL == "" {
		return "", "", errors.Wrap(err, "unable to get api server internal URL from cluster infrastructure")
	}
	// Check internal URL prefix
	if !strings.HasPrefix(apiServerInternalURL, "https://api-int.") {
		// TODO: May not need this, make sure this prefix is fixed for infra.Status.APIServerInternalURL
		// 	See https://github.com/openshift/windows-machine-config-operator/pull/66#
		return "", "", errors.Errorf("invalid API server internal URL: %s", apiServerInternalURL)
	}

	return apiServerURL, apiServerInternalURL, nil
}

// NewNodeConfig creates a new instance of nodeConfig to be used by the caller.
// hostName having a value will result in the VM's hostname being changed to the given value.
func NewNodeConfig(clientset *kubernetes.Clientset, clusterServiceCIDR, vxlanPort string,
	instance *instances.InstanceInfo, signer ssh.Signer, additionalAnnotations map[string]string) (*nodeConfig, error) {
	var err error
	if nodeConfigCache.apiServerHostname == "" || nodeConfigCache.apiServerInternalHostname == "" {
		// We couldn't find it in cache. Let's compute it now.
		err := initializeNodeConfigCache()
		if err != nil {
			return nil, errors.Wrap(err, "unable to initialize node config cache")
		}
	}
	if err = cluster.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, errors.Wrap(err, "error receiving valid CIDR value for creating new node config")
	}

	win, err := windows.New(nodeConfigCache.apiServerHostname, nodeConfigCache.apiServerInternalHostname,
		vxlanPort, instance, signer)
	if err != nil {
		return nil, errors.Wrap(err, "error instantiating Windows instance from VM")
	}

	log := ctrl.Log.WithName(fmt.Sprintf("nodeconfig %s", instance.Address))

	return &nodeConfig{
		k8sclientset:          clientset,
		Windows:               win,
		network:               newNetwork(log),
		clusterServiceCIDR:    clusterServiceCIDR,
		publicKeyHash:         CreatePubKeyHashAnnotation(signer.PublicKey()),
		log:                   log,
		additionalAnnotations: additionalAnnotations,
	}, nil
}

// parseHostname returns a valid hostname given the full endpoint URL
//
// For example, given https://api.cluster.openshift.com:6443 the
// hostname api.cluster.openshift.com is returned
func parseHostname(endpointUrlString string) (string, error) {
	endpointUrl, err := url.Parse(endpointUrlString)
	if err != nil {
		return "", errors.Wrapf(err, "unable to parse endpoint URL: %s", endpointUrlString)
	}
	// return hostname
	return endpointUrl.Hostname(), nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	drainHelper := &drain.Helper{Ctx: context.TODO(), Client: nc.k8sclientset}
	// If we find a node  it implies that we are reconfiguring and we should cordon the node
	if err := nc.setNode(true); err == nil {
		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}
	}

	// Perform the basic kubelet configuration using WMCB
	if err := nc.Windows.Configure(); err != nil {
		return errors.Wrap(err, "configuring the Windows VM failed")
	}

	// Perform rest of the configuration with the kubelet running
	err := func() error {
		// populate node object in nodeConfig in the case of a new Windows instance
		if err := nc.setNode(false); err != nil {
			return errors.Wrap(err, "error getting node object")
		}

		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}

		// Ensure we are annotating the node as soon as the Node object is created, so that we can identify which
		// controller should be watching it
		nc.addAdditionalAnnotations()
		nc.addPubKeyHashAnnotation()
		node, err := nc.k8sclientset.CoreV1().Nodes().Update(context.TODO(), nc.node, meta.UpdateOptions{})
		if err != nil {
			return errors.Wrapf(err, "error updating public key hash and additional annotations on node %s",
				nc.node.GetName())
		}
		nc.node = node

		// Now that basic kubelet configuration is complete, configure networking in the node
		if err := nc.configureNetwork(); err != nil {
			return errors.Wrap(err, "configuring node network failed")
		}

		// Now that the node has been fully configured, add the version annotation to signify that the node
		// was successfully configured by this version of WMCO
		// populate node object in nodeConfig once more
		if err := nc.setNode(false); err != nil {
			return errors.Wrap(err, "error getting node object")
		}

		// Version annotation is the indicator that the node was fully configured by this version of WMCO, so it should
		// be added at the end of the process.
		nc.addVersionAnnotation()
		node, err = nc.k8sclientset.CoreV1().Nodes().Update(context.TODO(), nc.node, meta.UpdateOptions{})
		if err != nil {
			return errors.Wrapf(err, "error updating version annotation on node %s", nc.node.GetName())
		}
		nc.node = node

		// Uncordon the node now that it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, false); err != nil {
			return errors.Wrapf(err, "error uncordoning the node %s", nc.node.GetName())
		}
		return nil
	}()

	// Stop the kubelet so that the node is marked NotReady in case of an error in configuration. We are stopping all
	// the required services as they are interdependent and is safer to do so given the node is going to be NotReady.
	if err != nil {
		if err := nc.Windows.EnsureRequiredServicesStopped(); err != nil {
			nc.log.Info("Unable to mark node as NotReady", "error", err)
		}
	}
	return err
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

// addPubKeyHashAnnotation adds the public key annotation to nc.node
func (nc *nodeConfig) addPubKeyHashAnnotation() {
	nc.node.Annotations[PubKeyHashAnnotation] = nc.publicKeyHash
}

// addAdditionalAnnotations merges nc.additionalAnnotations into the annotations on nc.node. If the annotation
// already existed, its value will be overwritten.
func (nc *nodeConfig) addAdditionalAnnotations() {
	if nc.additionalAnnotations == nil {
		return
	}
	for key, value := range nc.additionalAnnotations {
		nc.node.Annotations[key] = value
	}
}

// setNode finds the Node associated with the VM that has been configured, and sets the node field of the
// nodeConfig object. If quickCheck is set, the function does a quicker check for the node which is useful in the node
// reconfiguration case.
func (nc *nodeConfig) setNode(quickCheck bool) error {
	retryInterval := retry.Interval
	retryTimeout := retry.Timeout
	if quickCheck {
		retryInterval = 10 * time.Second
		retryTimeout = 30 * time.Second
	}
	err := wait.Poll(retryInterval, retryTimeout, func() (bool, error) {
		nodes, err := nc.k8sclientset.CoreV1().Nodes().List(context.TODO(),
			meta.ListOptions{LabelSelector: WindowsOSLabel})
		if err != nil {
			nc.log.V(1).Error(err, "node listing failed")
			return false, nil
		}
		if len(nodes.Items) == 0 {
			return false, nil
		}
		// get the node with IP address used to configure it
		for _, node := range nodes.Items {
			for _, nodeAddress := range node.Status.Addresses {
				if nc.Address() == nodeAddress.Address {
					nc.node = &node
					return true, nil
				}
			}
		}
		return false, nil
	})
	return errors.Wrapf(err, "unable to find node with address %s", nc.Address())
}

// waitForNodeAnnotation checks if the node object has the given annotation and waits for retry.Interval seconds and
// returns an error if the annotation does not appear in that time frame.
func (nc *nodeConfig) waitForNodeAnnotation(annotation string) error {
	nodeName := nc.node.GetName()
	var found bool
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		node, err := nc.k8sclientset.CoreV1().Nodes().Get(context.TODO(), nodeName, meta.GetOptions{})
		if err != nil {
			nc.log.V(1).Error(err, "unable to get associated node object")
			return false, nil
		}
		_, found := node.Annotations[annotation]
		if found {
			// update node to avoid staleness
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
		return errors.Wrap(err, "error populating host subnet in node network")
	}
	// populate the CNI config file with the host subnet and the service network CIDR
	configFile, err := nc.network.populateCniConfig(nc.clusterServiceCIDR, payload.CNIConfigTemplatePath)
	if err != nil {
		return errors.Wrapf(err, "error populating CNI config file %s", configFile)
	}
	// configure CNI in the Windows VM
	if err = nc.Windows.ConfigureCNI(configFile); err != nil {
		return errors.Wrapf(err, "error configuring CNI for %s", nc.node.GetName())
	}
	if err = nc.network.cleanupTempConfig(configFile); err != nil {
		nc.log.Error(err, " error deleting temp CNI config", "file",
			configFile)
	}
	return nil
}

// Deconfigure removes the node from the cluster, reverting changes made by the Configure function
func (nc *nodeConfig) Deconfigure() error {
	// Set nc.node to the existing node
	if err := nc.setNode(true); err != nil {
		return err
	}

	// Cordon and drain the Node before we interact with the instance
	drainHelper := &drain.Helper{Ctx: context.TODO(), Client: nc.k8sclientset}
	if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
		return errors.Wrapf(err, "unable to cordon node %s", nc.node.GetName())
	}
	if err := drain.RunNodeDrain(drainHelper, nc.node.GetName()); err != nil {
		return errors.Wrapf(err, "unable to drain node %s", nc.node.GetName())
	}

	// Revert the changes we've made to the instance by removing services and deleting all installed files
	if err := nc.Windows.Deconfigure(); err != nil {
		return errors.Wrap(err, "error deconfiguring instance")
	}

	// Delete the Node object
	err := nc.k8sclientset.CoreV1().Nodes().Delete(context.TODO(), nc.node.GetName(), meta.DeleteOptions{})
	if err != nil {
		return errors.Wrapf(err, "error deleting node %s", nc.node.GetName())
	}
	return nil
}

// CreatePubKeyHashAnnotation returns a formatted string which can be used for a public key annotation on a node.
// The annotation is the sha256 of the public key
func CreatePubKeyHashAnnotation(key ssh.PublicKey) string {
	pubKey := string(ssh.MarshalAuthorizedKey(key))
	trimmedKey := strings.TrimSuffix(pubKey, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmedKey)))
}
