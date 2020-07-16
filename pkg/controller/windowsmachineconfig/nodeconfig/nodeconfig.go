package nodeconfig

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	clientset "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/openshift/windows-machine-config-operator/pkg/clusternetwork"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/windows"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	certificates "k8s.io/api/certificates/v1beta1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	kretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	// bootstrapCSR is the CSR name associated with a worker node that just got bootstrapped.
	bootstrapCSR = "system:serviceaccount:openshift-machine-config-operator:node-bootstrapper"
	// HybridOverlaySubnet is an annotation applied by the cluster network operator which is used by the hybrid overlay
	HybridOverlaySubnet = "k8s.ovn.org/hybrid-overlay-node-subnet"
	// HybridOverlayMac is an annotation applied by the hybrid-overlay
	HybridOverlayMac = "k8s.ovn.org/hybrid-overlay-distributed-router-gateway-mac"
	// WindowsOSLabel is the label that is applied by WMCB to identify the Windows nodes bootstrapped via WMCB
	WindowsOSLabel = "node.openshift.io/os_id=Windows"
	// WorkerLabel is the label that needs to be applied to the Windows node to make it worker node
	WorkerLabel = "node-role.kubernetes.io/worker"
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
// TODO: WINC-429 will replace WindowsVM with a Machine
func NewNodeConfig(clientset *kubernetes.Clientset, windowsVM types.WindowsVM, clusterServiceCIDR string,
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

	win, err := windows.New(windowsVM, nodeConfigCache.workerIgnitionEndPoint, signer)
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
	if err := nc.handleCSRs(); err != nil {
		return errors.Wrap(err, "handling CSR for the given node failed")
	}

	// populate node object in nodeConfig
	if err := nc.getNode(); err != nil {
		return errors.Wrapf(err, "error getting node object for VM %s", nc.ID())
	}
	// Apply worker labels
	if err := nc.applyWorkerLabel(); err != nil {
		return errors.Wrap(err, "failed applying worker label")
	}
	// Now that basic kubelet configuration is complete, configure networking in the node
	if err := nc.configureNetwork(); err != nil {
		return errors.Wrap(err, "configuring node network failed")
	}
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

// applyWorkerLabel applies the worker label to the Windows Node we created.
func (nc *nodeConfig) applyWorkerLabel() error {
	if _, found := nc.node.Labels[WorkerLabel]; found {
		log.V(1).Info("worker label %s already present on node %s", WorkerLabel, nc.node.GetName())
		return nil
	}
	nc.node.Labels[WorkerLabel] = ""
	node, err := nc.k8sclientset.CoreV1().Nodes().Update(context.TODO(), nc.node, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(err, "error applying worker label to node object")
	}
	nc.node = node
	return nil
}

// HandleCSRs handles the approval of bootstrap and node CSRs
func (nc *nodeConfig) handleCSRs() error {
	// Handle the bootstrap CSR
	err := nc.handleCSR(bootstrapCSR)
	if err != nil {
		return errors.Wrap(err, "unable to handle bootstrap CSR")
	}

	// TODO: Handle the node CSR
	// 		Note: for the product we want to get the node name from the instance information
	//		jira story: https://issues.redhat.com/browse/WINC-271
	err = nc.handleCSR("system:node:")
	if err != nil {
		return errors.Wrap(err, "unable to handle node CSR")
	}
	return nil
}

// findCSR finds the CSR that contains the requestor filter
func (nc *nodeConfig) findCSR(requestor string) (*certificates.CertificateSigningRequest, error) {
	var foundCSR *certificates.CertificateSigningRequest
	// Find the CSR
	for retries := 0; retries < retry.Count; retries++ {
		csrs, err := nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("unable to get CSR list: %v", err)
		}
		if csrs == nil {
			time.Sleep(retry.Interval)
			continue
		}

		for _, csr := range csrs.Items {
			if !strings.Contains(csr.Spec.Username, requestor) {
				continue
			}
			var handledCSR bool
			for _, c := range csr.Status.Conditions {
				if c.Type == certificates.CertificateApproved || c.Type == certificates.CertificateDenied {
					handledCSR = true
					break
				}
			}
			if handledCSR {
				continue
			}
			foundCSR = &csr
			break
		}

		if foundCSR != nil {
			break
		}
		time.Sleep(retry.Interval)
	}

	if foundCSR == nil {
		return nil, errors.Errorf("unable to find CSR with requestor %s", requestor)
	}
	return foundCSR, nil
}

// approve approves the given CSR if it has not already been approved
// Based on https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/certificates/certificates.go#L237
func (nc *nodeConfig) approve(csr *certificates.CertificateSigningRequest) error {
	// Check if the certificate has already been approved
	for _, c := range csr.Status.Conditions {
		if c.Type == certificates.CertificateApproved {
			return nil
		}
	}

	// Approve the CSR
	return kretry.RetryOnConflict(kretry.DefaultRetry, func() error {
		// Ensure we get the current version
		csr, err := nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().Get(
			context.TODO(),
			csr.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Add the approval status condition
		csr.Status.Conditions = append(csr.Status.Conditions, certificates.CertificateSigningRequestCondition{
			Type:           certificates.CertificateApproved,
			Reason:         "WMCBe2eTestRunnerApprove",
			Message:        "This CSR was approved by WMCO runner",
			LastUpdateTime: metav1.Now(),
		})

		_, err = nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().UpdateApproval(context.TODO(), csr, metav1.UpdateOptions{})
		return err
	})
}

// handleCSR finds the CSR based on the requestor filter and approves it
func (nc *nodeConfig) handleCSR(requestorFilter string) error {
	csr, err := nc.findCSR(requestorFilter)
	if err != nil {
		return errors.Wrapf(err, "error finding CSR for %s", requestorFilter)
	}

	if err = nc.approve(csr); err != nil {
		return errors.Wrapf(err, "error approving CSR for %s", requestorFilter)
	}

	return nil
}

// getNode returns a pointer to the node object associated with the instance id provided
func (nc *nodeConfig) getNode() error {
	nodes, err := nc.k8sclientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: WindowsOSLabel})
	if err != nil {
		return errors.Wrap(err, "could not get list of nodes")
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no nodes found")
	}
	// get the node with given instance id
	instanceID := nc.ID()
	for _, node := range nodes.Items {
		if instanceID == getInstanceIDfromProviderID(node.Spec.ProviderID) {
			nc.node = &node
			return nil
		}
	}
	return errors.Errorf("unable to find node for instanceID %s", instanceID)
}

// waitForNodeAnnotation checks if the node object has the given annotation and waits for retry.Interval seconds and
// returns an error if the annotation does not appear in that time frame.
func (nc *nodeConfig) waitForNodeAnnotation(annotation string) error {
	nodeName := nc.node.GetName()
	var found bool
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		node, err := nc.k8sclientset.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil {
			return false, errors.Wrapf(err, "error getting node %s", nodeName)
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
