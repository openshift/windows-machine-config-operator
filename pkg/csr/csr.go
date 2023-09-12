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
	"golang.org/x/crypto/ssh"
	certificates "k8s.io/api/certificates/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client-kubelet;kubernetes.io/kubelet-serving

const (
	nodeGroup          = "system:nodes"
	nodeUserName       = "system:node"
	NodeUserNamePrefix = nodeUserName + ":"
	systemPrefix       = "system:authenticated"
)

var (
	// kubeletClientUsages contains the permitted key usages from a kube-apiserver-client-kubelet signer
	kubeletClientUsages = []certificates.KeyUsage{
		certificates.UsageKeyEncipherment,
		certificates.UsageDigitalSignature,
		certificates.UsageClientAuth,
	}
	// kubeletClientUsagesNoRSA contains the permitted client usages when kubelet is given a non-RSA key
	kubeletClientUsagesNoRSA = []certificates.KeyUsage{
		certificates.UsageDigitalSignature,
		certificates.UsageClientAuth,
	}
	// kubeletServerUsages contains the permitted key usages from a kubelet-serving signer
	kubeletServerUsages = []certificates.KeyUsage{
		certificates.UsageKeyEncipherment,
		certificates.UsageDigitalSignature,
		certificates.UsageServerAuth,
	}
	// kubeletServerUsagesNoRSA contains the permitted server usages when kubelet is given a non-RSA key
	kubeletServerUsagesNoRSA = []certificates.KeyUsage{
		certificates.UsageDigitalSignature,
		certificates.UsageServerAuth,
	}
)

// Approver holds the information required to approve a node CSR
type Approver struct {
	// client is the cache client
	client client.Client
	// k8sclientset holds the kube client needed for updating CSR approval conditions
	k8sclientset *kubernetes.Clientset
	// csr holds the pointer to the CSR request
	csr      *certificates.CertificateSigningRequest
	log      logr.Logger
	recorder record.EventRecorder
	// namespace is the namespace in which CSR's are present
	namespace string
}

// NewApprover returns a pointer to the Approver
func NewApprover(client client.Client, clientSet *kubernetes.Clientset, csr *certificates.CertificateSigningRequest,
	log logr.Logger, recorder record.EventRecorder, watchNamespace string) (*Approver, error) {
	if client == nil || csr == nil || clientSet == nil {
		return nil, fmt.Errorf("kubernetes client, clientSet or CSR should not be nil")
	}
	return &Approver{client,
		clientSet,
		csr,
		log,
		recorder,
		watchNamespace}, nil
}

// Approve approves a CSR by updating its status conditions to true if it is a valid CSR
func (a *Approver) Approve() error {
	if a.k8sclientset == nil {
		return fmt.Errorf("kubernetes clientSet should not be nil")
	}

	if valid, err := a.validateCSRContents(); !valid && err != nil {
		// if the validation fails and error returned is nil, returns nil
		return fmt.Errorf("could not validate contents for approval of CSR: %s: %w", a.csr.Name, err)

	}

	a.csr.Status.Conditions = append(a.csr.Status.Conditions, certificates.CertificateSigningRequestCondition{
		Type:           certificates.CertificateApproved,
		Status:         "True",
		Message:        "This CSR was approved by the WMCO certificate Approver.",
		LastUpdateTime: meta.Now(),
		Reason:         "WMCOApprove",
	})

	if _, err := a.k8sclientset.CertificatesV1().CertificateSigningRequests().UpdateApproval(context.Background(),
		a.csr.Name, a.csr, meta.UpdateOptions{}); err != nil {
		// have to return err itself here (not wrapped inside another error) so it can be identified as a conflict
		return err
	}
	a.log.Info("CSR approved", "CSR", a.csr.Name)
	return nil
}

// validateCSRContents returns true if the CSR request contents are valid.
// If the CSR is not from a BYOH Windows instance, it returns false with no error.
// If there is an error during validation, it returns false with the error.
func (a *Approver) validateCSRContents() (bool, error) {
	parsedCSR, err := ParseCSR(a.csr.Spec.Request)
	if err != nil {
		return false, fmt.Errorf("error parsing CSR: %s: %w", a.csr.Name, err)
	}

	nodeName := strings.TrimPrefix(parsedCSR.Subject.CommonName, NodeUserNamePrefix)
	if nodeName == "" {
		return false, fmt.Errorf("CSR %s subject name does not contain the required node user prefix: %s",
			a.csr.Name, NodeUserNamePrefix)
	}

	// lookup the node name against the instance configMap addresses/host names
	valid, err := a.validateNodeName(nodeName)
	if err != nil {
		return false, fmt.Errorf("error validating node name %s for CSR: %s: %w", nodeName, a.csr.Name, err)
	}
	// CSR is not from a BYOH Windows instance, don't return error to avoid requeue, instead log if it is invalid
	// as it might be from a linux node.
	if !valid {
		a.log.Info("CSR contents are invalid for approval by WMCO", "CSR", a.csr.Name)
		return false, nil
	}
	// Kubelet on a node needs two certificates for its normal operation:
	// Client certificate for securely communicating with the Kubernetes API server
	// Server certificate for use by Kubernetes API server to talk back to kubelet
	// Both types are validated based on their contents
	if a.isNodeClientCert(parsedCSR) {
		// Node client bootstrapper CSR is received before the instance becomes a node
		// hence we should not proceed if a corresponding node already exists
		node := &core.Node{}
		err := a.client.Get(context.TODO(), kubeTypes.NamespacedName{Namespace: a.namespace,
			Name: nodeName}, node)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("unable to get node %s: %w", nodeName, err)
		} else if err == nil {
			return false, fmt.Errorf("%s node already exists, cannot validate CSR: %s", nodeName, a.csr.Name)
		}
	} else {
		if err := a.validateKubeletServingCSR(parsedCSR); err != nil {
			return false, fmt.Errorf("unable to validate kubelet serving CSR: %s: %w", a.csr.Name, err)
		}
	}
	return true, nil
}

// validateNodeName returns true if the node name passed here matches either the
// actual host name of the VM'S or the reverse lookup of the instance addresses
// present in the configMap.
func (a *Approver) validateNodeName(nodeName string) (bool, error) {
	// Get the list of instances that are expected to be Nodes
	windowsInstances, err := wiparser.GetInstances(a.client, a.namespace)
	if err != nil {
		return false, fmt.Errorf("unable to retrieve Windows instances: %w", err)
	}
	// check if the node name matches the lookup of any of the instance addresses
	hasEntry, err := matchesDNS(nodeName, windowsInstances)
	if err != nil {
		return false, fmt.Errorf("unable to map node name to the addresses of Windows instances: %w", err)
	}
	if hasEntry {
		return true, nil
	}
	return a.validateWithHostName(nodeName, windowsInstances)
}

// validateWithHostName returns true if the node name given matches the host name for any of the instances
// provided in the instance list. If a match is found, it also validates if the node name complies with the DNS
// RFC1123 naming convention for internet hosts.
func (a *Approver) validateWithHostName(nodeName string, windowsInstances []*instance.Info) (bool, error) {
	// Create a new signer using the private key secret
	instanceSigner, err := signer.Create(kubeTypes.NamespacedName{Namespace: a.namespace,
		Name: secrets.PrivateKeySecret}, a.client)
	if err != nil {
		return false, fmt.Errorf("unable to create signer from private key secret: %w", err)
	}
	// check if the node name matches any of the instances host names
	hasEntry, err := matchesHostname(nodeName, windowsInstances, instanceSigner)
	if err != nil {
		return false, fmt.Errorf("unable to map node name to the host names of Windows instances: %w", err)
	}
	if !hasEntry {
		// CSR is not from a BYOH instance
		return false, nil
	}
	// validate node name for DNS RFC1123 naming conventions
	// ref: https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-subdomain-names
	if errs := validation.IsDNS1123Subdomain(nodeName); len(errs) > 0 {
		a.recorder.Eventf(a.csr, core.EventTypeWarning, "NodeNameValidationFailed",
			"node name %s does not comply with naming rules defined in RFC1123: "+
				"Requirements for internet hosts", nodeName)
		return false, fmt.Errorf("node name %s should comply with naming rules defined in RFC1123: "+
			"Requirements for internet hosts", nodeName)
	}
	return true, nil
}

// validateKubeletServingCSR validates a kubelet serving CSR for its contents
func (a *Approver) validateKubeletServingCSR(parsedCsr *x509.CertificateRequest) error {
	if a.csr == nil || parsedCsr == nil {
		return fmt.Errorf("CSR or request should not be nil")
	}
	// Check groups, we need at least: system:nodes, system:authenticated
	if len(a.csr.Spec.Groups) < 2 {
		return fmt.Errorf("CSR %s contains invalid number of groups: %d", a.csr.Name,
			len(a.csr.Spec.Groups))
	}
	groups := sets.NewString(a.csr.Spec.Groups...)
	if !groups.HasAll(nodeGroup, systemPrefix) {
		return fmt.Errorf("CSR %s does not contain required groups", a.csr.Name)
	}

	// Check usages, the list can include: digital signature, key encipherment and server auth
	if !hasUsages(a.csr, kubeletServerUsages) && !hasUsages(a.csr, kubeletServerUsagesNoRSA) {
		return fmt.Errorf("CSR %s does not contain required usages", a.csr.Name)
	}

	var hasOrg bool
	for i := range parsedCsr.Subject.Organization {
		if parsedCsr.Subject.Organization[i] == nodeGroup {
			hasOrg = true
			break
		}
	}
	if !hasOrg {
		return fmt.Errorf("CSR %s does not contain required subject organization", a.csr.Name)
	}
	return nil
}

// isNodeClientCert returns true if the CSR is from a  kube-apiserver-client-kubelet signer
// reference: https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/#kubernetes-signers
func (a *Approver) isNodeClientCert(x509cr *x509.CertificateRequest) bool {
	if !reflect.DeepEqual([]string{nodeGroup}, x509cr.Subject.Organization) {
		return false
	}
	if (len(x509cr.DNSNames) > 0) || (len(x509cr.EmailAddresses) > 0) || (len(x509cr.IPAddresses) > 0) {
		return false
	}
	// Check usages, the list can include: digital signature, key encipherment and client auth
	if !hasUsages(a.csr, kubeletClientUsages) && !hasUsages(a.csr, kubeletClientUsagesNoRSA) {
		return false
	}
	return true
}

// hasUsages verifies if the required usages exist in the CSR spec
func hasUsages(csr *certificates.CertificateSigningRequest, usages []certificates.KeyUsage) bool {
	if csr == nil || len(csr.Spec.Usages) < 2 {
		return false
	}
	usageMap := map[certificates.KeyUsage]struct{}{}
	for _, u := range usages {
		usageMap[u] = struct{}{}
	}

	for _, u := range csr.Spec.Usages {
		if _, ok := usageMap[u]; !ok {
			return false
		}
	}
	return true
}

// matchesHostname returns true if given node name matches with host name of any of the instances present
// in the given instance list
func matchesHostname(nodeName string, windowsInstances []*instance.Info,
	instanceSigner ssh.Signer) (bool, error) {
	for _, instanceInfo := range windowsInstances {
		hostName, err := findHostName(instanceInfo, instanceSigner)
		if err != nil {
			return false, fmt.Errorf("unable to find host name for instance with address %s: %w",
				instanceInfo.Address, err)
		}
		// check if the instance host name matches node name
		if strings.Contains(hostName, nodeName) {
			return true, nil
		}
	}
	return false, nil
}

// findHostName returns the actual host name of the instance by running the 'hostname' command
func findHostName(instanceInfo *instance.Info, instanceSigner ssh.Signer) (string, error) {
	// We don't need to pass most args here as we just need to be able to run commands on the instance.
	win, err := windows.New("", instanceInfo, instanceSigner, nil)
	if err != nil {
		return "", fmt.Errorf("error instantiating Windows instance: %w", err)
	}
	// get the instance host name  by running hostname command on remote VM
	hostName, err := win.Run("hostname", true)
	if err != nil {
		return "", fmt.Errorf("error getting the host name, with stdout %s: %w", hostName, err)
	}
	return hostName, nil
}

// matchesDNS returns true if the node name passed matches with the instance address of any of the instances present
// in the given instance list. If the address found is an IP address, we do a reverse lookup for the DNS address.
func matchesDNS(nodeName string, windowsInstances []*instance.Info) (bool, error) {
	for _, instanceInfo := range windowsInstances {
		// reverse lookup the instance if the address is an IP address
		if parseAddr := net.ParseIP(instanceInfo.Address); parseAddr != nil {
			dnsAddresses, err := net.LookupAddr(instanceInfo.Address)
			if err != nil {
				return false, fmt.Errorf("failed to lookup DNS for IP %s: %w", instanceInfo.Address, err)
			}
			for _, dns := range dnsAddresses {
				if strings.Contains(dns, nodeName) {
					return true, nil
				}
			}
		} else { // direct match if it is a DNS address
			if strings.Contains(instanceInfo.Address, nodeName) {
				return true, nil
			}
		}
	}
	return false, nil
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
