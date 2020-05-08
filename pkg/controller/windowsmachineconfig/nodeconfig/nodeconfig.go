package nodeconfig

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/windows"
	"github.com/pkg/errors"
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
	*windows.Windows
	// Node holds the information related to node object
	node *v1.Node
}

// discoverKubeAPIServerEndpoint discovers the kubernetes api server endpoint from the
func discoverKubeAPIServerEndpoint() (string, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return "", errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}
	if len(cfg.Host) == 0 {
		return "", errors.Wrap(err, "could not get host name for the kubernetes api server")
	}
	return cfg.Host, nil
}

// NewNodeConfig creates a new instance of nodeConfig to be used by the caller.
func NewNodeConfig(clientset *kubernetes.Clientset, windowsVM types.WindowsVM) (*nodeConfig, error) {
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
		workerIgnitionEndpoint := "https://api-int." + clusterAddress + ":22623/config/worker"
		nodeConfigCache.workerIgnitionEndPoint = workerIgnitionEndpoint
	}
	return &nodeConfig{k8sclientset: clientset, Windows: windows.New(windowsVM, nodeConfigCache.workerIgnitionEndPoint)}, nil
}

// getClusterAddr gets the cluster address associated with given kubernetes APIServerEndpoint.
// For example: https://api.abc.devcluster.openshift.com:6443 gets translated to
// abc.devcluster.openshift.com
// TODO: Think if this needs to be removed as this is too restrictive. Imagine apiserver behind
// 		a loadbalancer.
// 		Jira story: https://issues.redhat.com/browse/WINC-398
func getClusterAddr(kubeAPIServerEndpoint string) (string, error) {
	clusterEndPoint, err := url.Parse(kubeAPIServerEndpoint)
	if err != nil {
		return "", errors.Wrap(err, "unable to parse the kubernetes API server endpoint")
	}
	hostName := clusterEndPoint.Hostname()
	subdomainToBeReplaced := "api."

	if !strings.HasPrefix(hostName, "api.") {
		return "", errors.New("invalid API server url format found: expected hostname to start with `api.`")
	}
	// Replace `api.` with empty string for the first occurrence.
	clusterAddress := strings.Replace(hostName, subdomainToBeReplaced, "", 1)
	return clusterAddress, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	var err error
	// Validate WindowsVM
	if err := nc.Windows.Validate(); err != nil {
		return errors.Wrap(err, "error validating Windows VM")
	}

	if err := nc.Windows.Configure(); err != nil {
		return errors.Wrap(err, "configuring the Windows VM failed")
	}
	if err := nc.handleCSRs(); err != nil {
		return errors.Wrap(err, "handling CSR for the given node failed")
	}

	// populate node object in nodeConfig
	err = nc.getNode()
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("error getting node object for VM %s",
			nc.GetCredentials().GetInstanceId()))
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
		return errors.Wrap(err, fmt.Sprintf("error waiting for %s node annotation for %s", HybridOverlaySubnet,
			nc.node.GetName()))
	}

	// NOTE: Investigate if we need to introduce a interface wrt to the VM's networking configuration. This will
	// become more clear with the outcome of https://issues.redhat.com/browse/WINC-343

	// Configure the hybrid overlay in the Windows VM
	if err := nc.Windows.ConfigureHybridOverlay(nc.node.GetName()); err != nil {
		return errors.Wrap(err, fmt.Sprintf("error configuring hybrid overlay for %s", nc.node.GetName()))
	}

	// Wait until the node object has the hybrid overlay MAC annotation. This is required for the CNI configuration to
	// start.
	if err := nc.waitForNodeAnnotation(HybridOverlayMac); err != nil {
		return errors.Wrap(err, fmt.Sprintf("error waiting for %s node annotation for %s", HybridOverlayMac,
			nc.node.GetName()))
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
	node, err := nc.k8sclientset.CoreV1().Nodes().Update(nc.node)
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
		csrs, err := nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().List(metav1.ListOptions{})
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

		_, err = nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().UpdateApproval(csr)
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
	nodes, err := nc.k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: WindowsOSLabel})
	if err != nil {
		return errors.Wrap(err, "could not get list of nodes")
	}
	if len(nodes.Items) == 0 {
		return fmt.Errorf("no nodes found")
	}
	// get the node with given instance id
	instanceID := nc.GetCredentials().GetInstanceId()
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
		node, err := nc.k8sclientset.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
		if err != nil {
			return false, errors.Wrap(err, fmt.Sprintf("error getting node %s", nodeName))
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
		return errors.Wrap(err, fmt.Sprintf("timeout waiting for %s node annotation", annotation))
	}
	return nil
}

// getInstanceIDfromProviderID gets the instanceID of VM for a given cloud provider ID
// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
func getInstanceIDfromProviderID(providerID string) string {
	providerTokens := strings.Split(providerID, "/")
	return providerTokens[len(providerTokens)-1]
}
