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
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	csrvalidation "github.com/openshift/windows-machine-config-operator/pkg/csr/validation"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests/approval,verbs=update
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=certificatesigningrequests,verbs=get;list;watch
//+kubebuilder:rbac:groups="certificates.k8s.io",resources=signers,verbs=approve,resourceNames=kubernetes.io/kube-apiserver-client-kubelet;kubernetes.io/kubelet-serving

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
	// clientValidator validates kubelet client certificates
	clientValidator *csrvalidation.CSRValidator
	// servingValidator validates kubelet serving certificates
	servingValidator *csrvalidation.CSRValidator
}

// NewApprover returns a pointer to the Approver
func NewApprover(client client.Client, clientSet *kubernetes.Clientset, csr *certificates.CertificateSigningRequest,
	log logr.Logger, recorder record.EventRecorder, watchNamespace string) (*Approver, error) {
	if client == nil || csr == nil || clientSet == nil {
		return nil, fmt.Errorf("kubernetes client, clientSet or CSR should not be nil")
	}

	// Create validators for kubelet types
	clientValidator := csrvalidation.NewCSRValidator(client, csrvalidation.KubeletClientCertType)
	servingValidator := csrvalidation.NewCSRValidator(client, csrvalidation.KubeletServingCertType)

	return &Approver{
		client:           client,
		k8sclientset:     clientSet,
		csr:              csr,
		log:              log,
		recorder:         recorder,
		namespace:        watchNamespace,
		clientValidator:  clientValidator,
		servingValidator: servingValidator,
	}, nil
}

// Approve determines if a CSR should be approved by WMCO, and if so, approves it by updating its status. This function
// is a NOOP if the CSR should not be approved.
func (a *Approver) Approve(ctx context.Context) error {
	if a.k8sclientset == nil {
		return fmt.Errorf("kubernetes clientSet should not be nil")
	}

	validForApproval, err := a.validateCSRContents(ctx)
	if err != nil {
		return fmt.Errorf("error determining if CSR %s should be approved: %w", a.csr.Name, err)
	}
	if !validForApproval {
		return nil
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
func (a *Approver) validateCSRContents(ctx context.Context) (bool, error) {
	parsedCSR, err := csrvalidation.ParseCSR(a.csr.Spec.Request)
	if err != nil {
		return false, fmt.Errorf("error parsing CSR %s: %w", a.csr.Name, err)
	}

	if !strings.HasPrefix(parsedCSR.Subject.CommonName, csrvalidation.NodeUserNamePrefix) {
		return false, nil
	}

	if a.isNodeClientCert(parsedCSR) {
		return a.validateKubeletClientCSR(ctx)
	}
	return a.validateKubeletServingCSR(ctx)
}

// validateKubeletClientCSR validates kubelet client certificates
func (a *Approver) validateKubeletClientCSR(ctx context.Context) (bool, error) {
	nodeName, err := a.clientValidator.GetNodeNameFromCSR(a.csr)
	if err != nil {
		return false, fmt.Errorf("error extracting node name from CSR %s: %w", a.csr.Name, err)
	}

	valid, err := a.validateNodeName(ctx, nodeName)
	if err != nil {
		return false, fmt.Errorf("error validating node name %s for CSR: %s: %w", nodeName, a.csr.Name, err)
	}
	// CSR is not from a BYOH Windows instance, don't return error to avoid requeue, instead log if it is invalid
	// as it might be from a linux node.
	if !valid {
		a.log.Info("CSR contents are invalid for approval by WMCO", "CSR", a.csr.Name)
		return false, nil
	}

	// Node client bootstrapper CSR is received before the instance becomes a node
	// hence we should not proceed if a corresponding node already exists
	node := &core.Node{}
	err = a.client.Get(ctx, kubeTypes.NamespacedName{Namespace: a.namespace, Name: nodeName}, node)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("unable to get node %s: %w", nodeName, err)
	} else if err == nil {
		a.log.Info("node already exists, cannot validate CSR", "node", nodeName, "CSR", a.csr.Name)
		return false, nil
	}

	if err := a.clientValidator.ValidateCSR(ctx, a.csr); err != nil {
		return false, fmt.Errorf("kubelet client CSR validation failed: %w", err)
	}

	return true, nil
}

// validateKubeletServingCSR validates kubelet serving certificates
func (a *Approver) validateKubeletServingCSR(ctx context.Context) (bool, error) {
	nodeName, err := a.servingValidator.GetNodeNameFromCSR(a.csr)
	if err != nil {
		return false, fmt.Errorf("error extracting node name from CSR %s: %w", a.csr.Name, err)
	}

	// lookup the node name against the instance configMap addresses/host names
	valid, err := a.validateNodeName(ctx, nodeName)
	if err != nil {
		return false, fmt.Errorf("error validating node name %s for CSR: %s: %w", nodeName, a.csr.Name, err)
	}
	// CSR is not from a BYOH Windows instance, don't return error to avoid requeue, instead log if it is invalid
	// as it might be from a linux node.
	if !valid {
		a.log.Info("CSR contents are invalid for approval by WMCO", "CSR", a.csr.Name)
		return false, nil
	}

	if err := a.servingValidator.ValidateCSR(ctx, a.csr); err != nil {
		return false, fmt.Errorf("kubelet serving CSR validation failed: %w", err)
	}
	return true, nil
}

// validateNodeName returns true if the node name passed here matches either the
// actual host name of the VM'S or the reverse lookup of the instance addresses
// present in the configMap.
func (a *Approver) validateNodeName(ctx context.Context, nodeName string) (bool, error) {
	// Get the list of instances that are expected to be Nodes
	windowsInstances, err := wiparser.GetInstances(ctx, a.client, a.namespace)
	if err != nil {
		return false, fmt.Errorf("unable to retrieve Windows instances: %w", err)
	}
	// check if the node name matches the lookup of any of the instance addresses
	hasEntry, err := matchesDNS(nodeName, windowsInstances)
	if err != nil {
		a.log.Info("error occurred with reverse DNS lookup, falling back to hostname validation", "error", err)
	} else if hasEntry {
		return true, nil
	}
	return a.validateWithHostName(ctx, nodeName, windowsInstances)
}

// validateWithHostName returns true if the node name given matches the host name for any of the instances
// provided in the instance list. If a match is found, it also validates if the node name complies with the DNS
// RFC1123 naming convention for internet hosts.
func (a *Approver) validateWithHostName(ctx context.Context, nodeName string, windowsInstances []*instance.Info) (bool, error) {
	// Create a new signer using the private key secret
	instanceSigner, err := signer.Create(ctx, kubeTypes.NamespacedName{Namespace: a.namespace,
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
	defer win.Close()
	// get the instance host name  by running hostname command on remote VM
	return win.GetHostname()
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

// isNodeClientCert returns true if the CSR is from a kube-apiserver-client-kubelet signer.
// Client certificates have no DNS names, email addresses, or IP addresses (no SAN).
// Reference: https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/#kubernetes-signers
func (a *Approver) isNodeClientCert(x509cr *x509.CertificateRequest) bool {
	if !reflect.DeepEqual([]string{csrvalidation.NodeGroup}, x509cr.Subject.Organization) {
		return false
	}
	if (len(x509cr.DNSNames) > 0) || (len(x509cr.EmailAddresses) > 0) || (len(x509cr.IPAddresses) > 0) {
		return false
	}
	return true
}
