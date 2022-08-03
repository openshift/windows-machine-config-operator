package nodeconfig

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	clientset "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	cloudproviderapi "k8s.io/cloud-provider/api"
	cloudnodeutil "k8s.io/cloud-provider/node/helpers"
	"k8s.io/kubectl/pkg/drain"
	ctrl "sigs.k8s.io/controller-runtime"
	crclientcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
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
	// PubKeyHashAnnotation corresponds to the public key present on the VM
	PubKeyHashAnnotation = "windowsmachineconfig.openshift.io/pub-key-hash"
	// KubeletClientCAFilename is the name of the CA certificate file required by kubelet to interact
	// with the kube-apiserver client
	KubeletClientCAFilename = "kubelet-ca.crt"
	// DesiredVersionAnnotation is a Node annotation, indicating the Service ConfigMap that should be used to configure it
	DesiredVersionAnnotation = "windowsmachineconfig.openshift.io/desired-version"
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
	// additionalLabels are extra labels that should be applied to configured nodes
	additionalLabels map[string]string
	// platformType holds the name of the platform where cluster is deployed
	platformType configv1.PlatformType
}

// ErrWriter is a wrapper to enable error-level logging inside kubectl drainer implementation
type ErrWriter struct {
	log logr.Logger
}

func (ew ErrWriter) Write(p []byte) (n int, err error) {
	// log error
	ew.log.Error(err, string(p))
	return len(p), nil
}

// OutWriter is a wrapper to enable info-level logging inside kubectl drainer implementation
type OutWriter struct {
	log logr.Logger
}

func (ow OutWriter) Write(p []byte) (n int, err error) {
	// log info
	ow.log.Info(string(p))
	return len(p), nil
}

// discoverKubeAPIServerEndpoint discovers the kubernetes api server endpoint
func discoverKubeAPIServerEndpoint() (string, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return "", errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return "", errors.Wrap(err, "unable to get client from the given config")
	}

	host, err := client.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
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
// hostName having a value will result in the VM's hostname being changed to the given value.
func NewNodeConfig(clientset *kubernetes.Clientset, clusterServiceCIDR, vxlanPort string,
	instanceInfo *instance.Info, signer ssh.Signer, additionalLabels,
	additionalAnnotations map[string]string, platformType configv1.PlatformType) (*nodeConfig, error) {
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
	if err = cluster.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, errors.Wrap(err, "error receiving valid CIDR value for "+
			"creating new node config")
	}

	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting cluster DNS from service CIDR: %s", clusterServiceCIDR)
	}

	log := ctrl.Log.WithName(fmt.Sprintf("nc %s", instanceInfo.Address))
	win, err := windows.New(nodeConfigCache.workerIgnitionEndPoint, clusterDNS, vxlanPort,
		instanceInfo, signer, string(platformType))
	if err != nil {
		return nil, errors.Wrap(err, "error instantiating Windows instance from VM")
	}

	return &nodeConfig{k8sclientset: clientset, Windows: win, network: newNetwork(log), platformType: platformType,
		clusterServiceCIDR: clusterServiceCIDR, publicKeyHash: CreatePubKeyHashAnnotation(signer.PublicKey()),
		log: log, additionalLabels: additionalLabels, additionalAnnotations: additionalAnnotations}, nil
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
		return "", fmt.Errorf("invalid API server url %s: expected hostname to start with `api-int.`", hostName)
	}
	return hostName, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	drainHelper := nc.newDrainHelper()
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

		// Ensure we are labeling and annotating the node as soon as the Node object is created, so that we can identify
		// which controller should be watching it
		annotationsToApply := map[string]string{PubKeyHashAnnotation: nc.publicKeyHash}
		for key, value := range nc.additionalAnnotations {
			annotationsToApply[key] = value
		}
		if err := nc.applyLabelsAndAnnotations(nc.additionalLabels, annotationsToApply); err != nil {
			return errors.Wrapf(err, "error updating public key hash and additional annotations on node %s",
				nc.node.GetName())
		}

		ownedByCCM, err := isCloudControllerOwnedByCCM()
		if err != nil {
			return errors.Wrap(err, "unable to check if cloud controller owned by cloud controller manager")
		}

		// Configuring cloud node manager for Azure platform with CCM support
		if ownedByCCM && nc.platformType == configv1.AzurePlatformType {
			if err := nc.Windows.ConfigureAzureCloudNodeManager(nc.node.GetName()); err != nil {
				return errors.Wrap(err, "configuring azure cloud node manager failed")
			}

			// If we deploy on Azure with CCM support, we have to explicitly remove the cloud taint, because cloud node manager
			// running on the node can't do it itself. The taint should be removed only when the related CSR
			// has been approved.
			cloudTaint := &core.Taint{
				Key:    cloudproviderapi.TaintExternalCloudProvider,
				Effect: core.TaintEffectNoSchedule,
			}
			if err := cloudnodeutil.RemoveTaintOffNode(nc.k8sclientset, nc.node.GetName(), nc.node, cloudTaint); err != nil {
				return errors.Wrapf(err, "error excluding cloud taint on node %s", nc.node.GetName())
			}
		}
		if err := nc.configureWICD(); err != nil {
			return errors.Wrap(err, "configuring WICD failed")
		}
		// Set the desired version annotation, communicating to WICD which Windows services configmap to use
		if err := nc.applyLabelsAndAnnotations(nil, map[string]string{DesiredVersionAnnotation: version.Get()}); err != nil {
			return errors.Wrapf(err, "error updating desired version annotation on node %s", nc.node.GetName())
		}

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
		if err := nc.applyLabelsAndAnnotations(nil, map[string]string{metadata.VersionAnnotation: version.Get()}); err != nil {
			return errors.Wrapf(err, "error updating version annotation on node %s", nc.node.GetName())
		}

		// Uncordon the node now that it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, false); err != nil {
			return errors.Wrapf(err, "error uncordoning the node %s", nc.node.GetName())
		}

		nc.log.Info("instance has been configured as a worker node", "version",
			nc.node.Annotations[metadata.VersionAnnotation])
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

// applyLabelsAndAnnotations applies all the given labels and annotations and updates the Node object in NodeConfig
func (nc *nodeConfig) applyLabelsAndAnnotations(labels, annotations map[string]string) error {
	patchData, err := metadata.GenerateAddPatch(labels, annotations)
	if err != nil {
		return err
	}
	node, err := nc.k8sclientset.CoreV1().Nodes().Patch(context.TODO(), nc.node.GetName(), kubeTypes.JSONPatchType,
		patchData, meta.PatchOptions{})
	if err != nil {
		return errors.Wrapf(err, "unable to apply patch data %s", patchData)
	}
	nc.node = node
	return nil
}

// isCloudControllerOwnedByCCM checks if Cloud Controllers are managed by Cloud Controller Manager (CCM)
// instead of Kube Controller Manager.
func isCloudControllerOwnedByCCM() (bool, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return false, errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return false, errors.Wrap(err, "unable to get client from the given config")
	}

	return cluster.IsCloudControllerOwnedByCCM(client)
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

	// Wait until the node object has the hybrid overlay MAC annotation. This indicates that hybrid-overlay is running
	// successfully, and is required for the CNI configuration to start.
	if err := nc.waitForNodeAnnotation(HybridOverlayMac); err != nil {
		return errors.Wrapf(err, "error waiting for %s node annotation for %s", HybridOverlayMac,
			nc.node.GetName())
	}
	// Running the hybrid-overlay causes network reconfiguration in the Windows VM which results in the ssh connection
	// being closed, and the client is not smart enough to reconnect.
	if err := nc.Windows.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing VM after running hybrid-overlay")
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

	instanceAddress := nc.GetIPv4Address()
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
		if node := nodeutil.FindByAddress(instanceAddress, nodes); node != nil {
			nc.node = node
			return true, nil
		}
		return false, nil
	})
	return errors.Wrapf(err, "unable to find node with address %s", instanceAddress)
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
	// populate the CNI config file with the host subnet, service network CIDR and IP address of the Windows VM
	configFile, err := nc.network.populateCniConfig(nc.clusterServiceCIDR, nc.GetIPv4Address(), payload.CNIConfigTemplatePath)
	if err != nil {
		return errors.Wrapf(err, "error populating CNI config file %s", configFile)
	}
	// Copy CNI config file
	if err = nc.Windows.EnsureCNIConfig(configFile); err != nil {
		return errors.Wrapf(err, "error ensuring CNI config file for %s", nc.node.GetName())
	}
	if err = nc.network.cleanupTempConfig(configFile); err != nil {
		nc.log.Error(err, " error deleting temp CNI config", "file",
			configFile)
	}
	return nil
}

// newDrainHelper returns new drain.Helper instance
func (nc *nodeConfig) newDrainHelper() *drain.Helper {
	return &drain.Helper{
		Ctx:    context.TODO(),
		Client: nc.k8sclientset,
		ErrOut: &ErrWriter{nc.log},
		Out:    &OutWriter{nc.log},
	}
}

// Deconfigure removes the node from the cluster, reverting changes made by the Configure function
func (nc *nodeConfig) Deconfigure() error {
	// Set nc.node to the existing node
	if err := nc.setNode(true); err != nil {
		return err
	}

	// Cordon and drain the Node before we interact with the instance
	drainHelper := nc.newDrainHelper()
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

	// Clear the version annotation from the node object to indicate the node is not configured
	patchData, err := metadata.GenerateRemovePatch([]string{}, []string{metadata.VersionAnnotation})
	if err != nil {
		return errors.Wrapf(err, "error creating version annotation remove request")
	}
	_, err = nc.k8sclientset.CoreV1().Nodes().Patch(context.TODO(), nc.node.GetName(), kubeTypes.JSONPatchType,
		patchData, meta.PatchOptions{})
	if err != nil {
		return errors.Wrapf(err, "error removing version annotation from node %s", nc.node.GetName())
	}

	nc.log.Info("instance has been deconfigured", "node", nc.node.GetName())
	return nil
}

// UpdateKubeletClientCA updates the kubelet client CA certificate file in the Windows node. No service restart or
// reboot required, kubelet detects the changes in the file system and use the new CA certificate. The file is replaced
// if and only if it does not exist or there is a checksum mismatch.
func (nc *nodeConfig) UpdateKubeletClientCA(contents []byte) error {
	// check CA bundle contents
	if len(contents) == 0 {
		// nothing do to, return
		return nil
	}
	err := nc.Windows.EnsureFileContent(contents, KubeletClientCAFilename, windows.GetK8sDir())
	if err != nil {
		return err
	}
	return nil
}

// configureWICD configures and ensures WICD is running
func (nc *nodeConfig) configureWICD() error {
	tokenSecretPrefix := "windows-instance-config-daemon-token-"
	secrets, err := nc.k8sclientset.CoreV1().Secrets("openshift-windows-machine-config-operator").
		List(context.TODO(), meta.ListOptions{})
	if err != nil {
		return errors.Wrap(err, "error listing secrets")
	}
	var filteredSecrets []core.Secret
	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, tokenSecretPrefix) {
			filteredSecrets = append(filteredSecrets, secret)
		}
	}
	if len(filteredSecrets) != 1 {
		return fmt.Errorf("expected 1 secret with '%s' prefix, found %d", tokenSecretPrefix, len(filteredSecrets))
	}
	saCA := filteredSecrets[0].Data["ca.crt"]
	if len(saCA) == 0 {
		return errors.New("ServiceAccount ca.crt value empty")
	}
	saToken := filteredSecrets[0].Data["token"]
	if len(saToken) == 0 {
		return errors.New("ServiceAccount token value empty")
	}
	return nc.Windows.ConfigureWICD(nodeConfigCache.apiServerEndpoint, saCA, saToken)
}

// CreatePubKeyHashAnnotation returns a formatted string which can be used for a public key annotation on a node.
// The annotation is the sha256 of the public key
func CreatePubKeyHashAnnotation(key ssh.PublicKey) string {
	pubKey := string(ssh.MarshalAuthorizedKey(key))
	trimmedKey := strings.TrimSuffix(pubKey, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmedKey)))
}
