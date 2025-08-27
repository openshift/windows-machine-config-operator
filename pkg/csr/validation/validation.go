package validation

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// kubeletClientUsages contains the permitted key usages from a kube-apiserver-client-kubelet signer
	kubeletClientUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
	}
	// kubeletClientUsagesNoRSA contains the permitted client usages when kubelet is given a non-RSA key
	kubeletClientUsagesNoRSA = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
	}
	// kubeletServerUsages contains the permitted key usages from a kubelet-serving signer
	kubeletServerUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageKeyEncipherment,
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageServerAuth,
	}
	// kubeletServerUsagesNoRSA contains the permitted server usages when kubelet is given a non-RSA key
	kubeletServerUsagesNoRSA = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageServerAuth,
	}
	// wicdClientUsages contains the required usages for WICD client certificates
	wicdClientUsages = []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
		certificatesv1.UsageKeyEncipherment,
	}
)

// CertificateType defines the type of certificate being validated
type CertificateType struct {
	// Name is used for logging and error messages
	Name string
	// UserPrefix is the expected prefix for the certificate common name (e.g., "system:node", "system:wicd-node")
	UserPrefix string
	// GroupName is the expected organization in the certificate (e.g., "system:nodes", "system:wicd-nodes")
	GroupName string
	// RequiredGroups are the required groups in the CSR spec (for kubelet serving certificates)
	RequiredGroups []string
	// ValidateNodeExists indicates whether to verify the node exists before approval
	ValidateNodeExists bool
	// AllowDNSNames indicates whether DNS names are allowed in the certificate
	AllowDNSNames bool
	// AllowIPAddresses indicates whether IP addresses are allowed in the certificate
	AllowIPAddresses bool
}

// CSRValidator provides common CSR validation functionality
type CSRValidator struct {
	client   client.Client
	certType CertificateType
}

// NewCSRValidator creates a new CSR validator for the given certificate type
func NewCSRValidator(client client.Client, certType CertificateType) *CSRValidator {
	return &CSRValidator{
		client:   client,
		certType: certType,
	}
}

// ParseCSR extracts the CSR from the API object and decodes it
func ParseCSR(csrData []byte) (*x509.CertificateRequest, error) {
	if len(csrData) == 0 {
		return nil, fmt.Errorf("CSR request spec should not be empty")
	}
	// extract PEM from request object
	block, _ := pem.Decode(csrData)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("PEM block type must be CERTIFICATE REQUEST")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

// ValidateCSR validates a CSR according to the certificate type rules
func (v *CSRValidator) ValidateCSR(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) error {
	// Parse the certificate request
	parsedCSR, err := ParseCSR(csr.Spec.Request)
	if err != nil {
		return fmt.Errorf("error parsing CSR %s: %w", csr.Name, err)
	}

	nodeName := strings.TrimPrefix(parsedCSR.Subject.CommonName, v.certType.UserPrefix+":")
	if nodeName == "" || nodeName == parsedCSR.Subject.CommonName {
		return fmt.Errorf("CSR %s subject name does not contain the required prefix: %s",
			csr.Name, v.certType.UserPrefix+":")
	}

	if err := v.validateCertificateContent(parsedCSR); err != nil {
		return fmt.Errorf("certificate content validation failed for CSR %s: %w", csr.Name, err)
	}

	// Validate CSR spec (groups, usages)
	if err := v.validateCSRSpec(csr, parsedCSR); err != nil {
		return fmt.Errorf("CSR spec validation failed for CSR %s: %w", csr.Name, err)
	}

	// Validate node exists if required for this type of CSR
	if v.certType.ValidateNodeExists {
		if err := v.validateNodeExists(ctx, nodeName); err != nil {
			return fmt.Errorf("node validation failed for CSR %s: %w", csr.Name, err)
		}
	}
	return nil
}

// validateCertificateContent validates the parsed certificate request content
func (v *CSRValidator) validateCertificateContent(parsedCSR *x509.CertificateRequest) error {
	// Validate organization
	hasRequiredOrg := false
	for _, org := range parsedCSR.Subject.Organization {
		if org == v.certType.GroupName {
			hasRequiredOrg = true
			break
		}
	}
	if !hasRequiredOrg {
		return fmt.Errorf("certificate request missing required organization: %s", v.certType.GroupName)
	}

	// Validate DNS names and IP addresses based on certificate type
	if !v.certType.AllowDNSNames && len(parsedCSR.DNSNames) > 0 {
		return fmt.Errorf("DNS names not allowed for %s certificates", v.certType.Name)
	}
	if !v.certType.AllowIPAddresses && len(parsedCSR.IPAddresses) > 0 {
		return fmt.Errorf("IP addresses not allowed for %s certificates", v.certType.Name)
	}
	return nil
}

// validateCSRSpec validates the CSR spec (groups, usages)
func (v *CSRValidator) validateCSRSpec(csr *certificatesv1.CertificateSigningRequest, parsedCSR *x509.CertificateRequest) error {
	// Validate required groups if specified
	if len(v.certType.RequiredGroups) > 0 {
		if len(csr.Spec.Groups) < len(v.certType.RequiredGroups) {
			return fmt.Errorf("CSR contains invalid number of groups: %d, expected at least %d",
				len(csr.Spec.Groups), len(v.certType.RequiredGroups))
		}
		groups := sets.NewString(csr.Spec.Groups...)
		if !groups.HasAll(v.certType.RequiredGroups...) {
			return fmt.Errorf("CSR does not contain required groups: %v", v.certType.RequiredGroups)
		}
	}

	// Validate key usages
	if !v.hasValidUsages(csr) {
		return fmt.Errorf("CSR does not contain valid usages for %s certificates", v.certType.Name)
	}

	return nil
}

// hasValidUsages verifies if the CSR has valid key usages for this certificate type
func (v *CSRValidator) hasValidUsages(csr *certificatesv1.CertificateSigningRequest) bool {
	switch v.certType.Name {
	case "kubelet-client":
		return hasUsages(csr, kubeletClientUsages) || hasUsages(csr, kubeletClientUsagesNoRSA)
	case "kubelet-serving":
		return hasUsages(csr, kubeletServerUsages) || hasUsages(csr, kubeletServerUsagesNoRSA)
	case "wicd":
		return hasUsages(csr, wicdClientUsages)
	default:
		return false
	}
}

// validateNodeExists checks if the node exists in the cluster
func (v *CSRValidator) validateNodeExists(ctx context.Context, nodeName string) error {
	node := &corev1.Node{}
	if err := v.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("node %s does not exist", nodeName)
		}
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	return nil
}

// GetNodeNameFromCSR extracts the node name from a CSR's common name
func (v *CSRValidator) GetNodeNameFromCSR(csr *certificatesv1.CertificateSigningRequest) (string, error) {
	parsedCSR, err := ParseCSR(csr.Spec.Request)
	if err != nil {
		return "", fmt.Errorf("error parsing CSR: %w", err)
	}

	nodeName := strings.TrimPrefix(parsedCSR.Subject.CommonName, v.certType.UserPrefix+":")
	if nodeName == "" || nodeName == parsedCSR.Subject.CommonName {
		return "", fmt.Errorf("invalid common name format: %s", parsedCSR.Subject.CommonName)
	}

	return nodeName, nil
}

// IsCorrectCertificateType checks if a CSR is for this certificate type
func (v *CSRValidator) IsCorrectCertificateType(csr *certificatesv1.CertificateSigningRequest) bool {
	parsedCSR, err := ParseCSR(csr.Spec.Request)
	if err != nil {
		return false
	}

	// Check if Common Name starts with the expected prefix
	if !strings.HasPrefix(parsedCSR.Subject.CommonName, v.certType.UserPrefix+":") {
		return false
	}

	// Check if Organization includes the expected group
	for _, org := range parsedCSR.Subject.Organization {
		if org == v.certType.GroupName {
			return true
		}
	}

	return false
}

// hasUsages verifies if the required usages exist in the CSR spec
func hasUsages(csr *certificatesv1.CertificateSigningRequest, usages []certificatesv1.KeyUsage) bool {
	if csr == nil || len(csr.Spec.Usages) < 2 {
		return false
	}
	usageMap := map[certificatesv1.KeyUsage]struct{}{}
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
