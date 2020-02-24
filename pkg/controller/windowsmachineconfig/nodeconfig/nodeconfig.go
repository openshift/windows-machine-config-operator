package nodeconfig

import (
	"fmt"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/windows"
	"github.com/pkg/errors"
	certificates "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// RetryCount is the amount of times we will retry an api operation
	RetryCount = 20
	// RetryInterval is the interval of time until we retry after a failure
	RetryInterval = 5 * time.Second
	// bootstrapCSR is the CSR name associated with a worker node that just got bootstrapped.
	bootstrapCSR = "system:serviceaccount:openshift-machine-config-operator:node-bootstrapper"
)

// nodeConfig holds the information to make the given VM a kubernetes node. As of now, it holds the information
// related to kubeclient and the windowsVM.
type nodeConfig struct {
	// k8sclientset holds the information related to kubernetes clientset
	k8sclientset *kubernetes.Clientset
	// Windows holds the information related to the windows VM
	*windows.Windows
}

// NewNodeConfig creates a new instance of nodeConfig to be used by the caller.
func NewNodeConfig(clientset *kubernetes.Clientset, windowsVM types.WindowsVM) *nodeConfig {
	return &nodeConfig{k8sclientset: clientset, Windows: windows.New(windowsVM)}
}

var log = logf.Log.WithName("nodeconfig")

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	if err := nc.Windows.Configure(); err != nil {
		return errors.Wrap(err, "configuring the Windows VM failed")
	}
	if err := nc.handleCSRs(); err != nil {
		return errors.Wrap(err, "handling CSR for the given node failed")
	}
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
	for retries := 0; retries < RetryCount; retries++ {
		csrs, err := nc.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().List(metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("unable to get CSR list: %v", err)
		}
		if csrs == nil {
			time.Sleep(RetryInterval)
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
		time.Sleep(RetryInterval)
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
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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
