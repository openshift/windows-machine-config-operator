/*
Based on https://github.com/openshift/cluster-machine-approver/tree/master/pkg/controller
Cluster machine approver approves CSR's from machines, hence we cannot use the code from
the package for approving CSR's from BYOH instances which may not have reference to a
machine object
*/

package csr

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"reflect"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	certificatesv1 "k8s.io/api/certificates/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=Approve,resourceNames=kubernetes.io/kube-apiserver-client-kubelet;kubernetes.io/kubelet-serving

// kubeletClientUsages contains the permitted key usages from a kube-apiserver-client-kubelet signer
var kubeletClientUsages = []certificatesv1.KeyUsage{
	certificatesv1.UsageKeyEncipherment,
	certificatesv1.UsageDigitalSignature,
	certificatesv1.UsageClientAuth,
}

// kubeletServerUsages contains the permitted key usages from a kubelet-serving signer
var kubeletServerUsages = []certificatesv1.KeyUsage{
	certificatesv1.UsageKeyEncipherment,
	certificatesv1.UsageDigitalSignature,
	certificatesv1.UsageServerAuth,
}

const (
	nodeGroup          = "system:nodes"
	nodeUserName       = "system:node"
	NodeUserNamePrefix = nodeUserName + ":"
	systemPrefix       = "system:authenticated"
)

// Approver holds the information required to approve a node CSR
type Approver struct {
	// certificateSigningRequest holds the pointer to the CSR request
	*certificatesv1.CertificateSigningRequest
	// k8sclientset holds the kube client
	k8sClientSet *kubernetes.Clientset
	// instanceAddresses holds the list of parsed instance adresses
	instanceAddresses []string
	log               logr.Logger
}

// NewApprover returns a pointer to the Approver
func NewApprover(csr *certificatesv1.CertificateSigningRequest, k8sclientSet *kubernetes.Clientset, instanceAddresses []string, log logr.Logger) (*Approver, error) {
	if csr == nil || len(instanceAddresses) == 0 || k8sclientSet == nil {
		return nil, errors.New("instance address list, CSR request or kubernetes clientSet should not be nil")
	}
	return &Approver{csr, k8sclientSet, instanceAddresses, log}, nil
}

// validateCSRContents returns true if the CSR request contents are valid
func (c *Approver) validateCSRContents() (bool, error) {
	parsedCSR, err := ParseCSR(c.Spec.Request)
	if err != nil {
		return false, errors.Wrapf(err, "error parsing CSR %s", c.Name)
	}

	nodeName := strings.TrimPrefix(parsedCSR.Subject.CommonName, NodeUserNamePrefix)
	if nodeName == "" {
		return false, errors.Errorf("CSR subject name does not contain the required node user prefix %s", c.Name)
	}

	// check node address in CSR matches with instance address
	var hasMatch bool
	for _, address := range c.instanceAddresses {
		if c.areAddressesEqual(address, nodeName) {
			hasMatch = true
			break
		}
	}
	if !hasMatch {
		// CSR is not from a BYOH Windows instance, don't deny it or return error
		// since it might be from a linux node, instead log if returns false here
		return false, nil
	}
	// Kubelet on a node needs two certificates for its normal operation:
	// Client certificate for securely communicating with the Kubernetes API server
	// Server certificate for use by Kubernetes API server to talk back to kubelet
	// Both types are validated based on their contents
	var validated bool
	if c.isNodeClientCert(parsedCSR) {
		// Node client bootstrapper CSR is received before the instance becomes a node
		// hence we should not proceed if a corresponding node already exists
		_, err := c.k8sClientSet.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return false, errors.Wrapf(err, "unable to get node %s", nodeName)
		} else if err == nil {
			return false, errors.Wrapf(err, "%s node already exists, cannot validate CSR", nodeName)
		}
	} else {
		if validated, err = c.validateKubeletServingCSR(parsedCSR); !validated {
			return false, errors.Wrapf(err, "unable to validate kubelet serving CSR %s", c.Name)
		}
	}
	return true, nil
}

// validateKubeletServingCSR validates a kubelet serving CSR for its contents
func (c *Approver) validateKubeletServingCSR(csr *x509.CertificateRequest) (bool, error) {
	if c.CertificateSigningRequest == nil || csr == nil {
		return false, errors.New("CSR or request should not be nil")
	}

	// Check groups, we need at least: system:nodes, system:authenticated
	if len(c.Spec.Groups) < 2 {
		return false, errors.Errorf("CSR %s contains invalid number of groups: %d", c.Name, len(c.Spec.Groups))
	}
	groups := sets.NewString(c.Spec.Groups...)
	if !groups.HasAll(nodeGroup, systemPrefix) {
		return false, errors.Errorf("CSR %s does not contain required groups", c.Name)
	}

	// Check usages include: digital signature, key encipherment and server auth
	if !c.hasUsages(kubeletServerUsages) {
		return false, errors.Errorf("CSR %s does not contain required usages", c.Name)
	}

	var hasOrg bool
	for i := range csr.Subject.Organization {
		if csr.Subject.Organization[i] == nodeGroup {
			hasOrg = true
			break
		}
	}
	if !hasOrg {
		return false, errors.Errorf("CSR %s does not contain required subject organization", c.Name)
	}
	return true, nil
}

// Approve approves a CSR by updating it's status conditions to true if it is a valid CSR
func (c *Approver) Approve() error {
	if validated, err := c.validateCSRContents(); !validated {
		c.log.Info("CSR contents are invalid for approval by WMCO CSR Approver", "CSR", c.Name)
		return errors.Wrapf(err, "could not validate CSR %s contents for approval", c.Name)
	}

	c.Status.Conditions = append(c.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           certificatesv1.CertificateApproved,
		Status:         "True",
		Message:        "This CSR was approved by the WMCO CSR Approver",
		LastUpdateTime: metav1.Now(),
		Reason:         "WMCOApproved",
	})

	if _, err := c.k8sClientSet.CertificatesV1().CertificateSigningRequests().UpdateApproval(context.Background(),
		c.Name, c.CertificateSigningRequest, metav1.UpdateOptions{}); err != nil {
		return errors.Wrapf(err, "could not update conditions for CSR approval %s", c.Name)
	}
	c.log.Info("CSR approved by WMCO CSR Approver", "CSR", c.Name)
	return nil
}

// isNodeClientCert returns true if the CSR is from a  kube-apiserver-client-kubelet signer
// reference: https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/#kubernetes-signers
func (c *Approver) isNodeClientCert(x509cr *x509.CertificateRequest) bool {
	if !reflect.DeepEqual([]string{nodeGroup}, x509cr.Subject.Organization) {
		return false
	}
	if (len(x509cr.DNSNames) > 0) || (len(x509cr.EmailAddresses) > 0) || (len(x509cr.IPAddresses) > 0) {
		return false
	}
	// Check usages include: digital signature, key encipherment and client auth
	if !c.hasUsages(kubeletClientUsages) {
		return false
	}
	if !strings.HasPrefix(x509cr.Subject.CommonName, NodeUserNamePrefix) {
		return false
	}
	return true
}

// hasUsages verifies if the required usages exist in the CSR spec
func (c *Approver) hasUsages(usages []certificatesv1.KeyUsage) bool {
	if len(usages) != len(c.Spec.Usages) {
		return false
	}

	usageMap := map[certificatesv1.KeyUsage]struct{}{}
	for _, u := range usages {
		usageMap[u] = struct{}{}
	}

	for _, u := range c.Spec.Usages {
		if _, ok := usageMap[u]; !ok {
			return false
		}
	}

	return true
}

// getDnsAddresses returns list of dns addresses corresponding to the given IPv4 address
func getDnsAddresses(ipAddr string) []string {
	addr, _ := net.LookupAddr(ipAddr)
	return addr
}

// ParseCSR extracts the CSR from the API object and decodes it.
func ParseCSR(csr []byte) (*x509.CertificateRequest, error) {
	if len(csr) == 0 {
		return nil, fmt.Errorf("CSR request spec should not be empty")
	}
	// extract PEM from request object
	block, _ := pem.Decode(csr)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("PEM block type must be CERTIFICATE REQUEST")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

// areAddressesEqual returns true if the dns address in CSR subject matches with the instance address
// If the address passed is an IP address, we do a reverse lookup to find the host address
func (c *Approver) areAddressesEqual(instanceAdd string, csrAddr string) bool {
	// reverse lookup the host if the address is an IP address
	if parseAddr := net.ParseIP(instanceAdd); parseAddr != nil {
		dnsAddresses, _ := net.LookupAddr(instanceAdd)
		for _, dns := range dnsAddresses {
			if strings.Contains(dns, csrAddr) {
				return true
			}
		} //match directly for host address
	} else if instanceAdd == csrAddr {
		return true
	}
	return false
}
