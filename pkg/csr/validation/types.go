package validation

import (
	certificatesv1 "k8s.io/api/certificates/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

const (
	// Node certificate constants
	nodeGroup          = "system:nodes"
	nodeUserName       = "system:node"
	NodeUserNamePrefix = nodeUserName
	systemPrefix       = "system:authenticated"
)

var (
	// KubeletClientCertType defines validation rules for kubelet client certificates
	KubeletClientCertType = CertificateType{
		Name:       "kubelet-client",
		UserPrefix: NodeUserNamePrefix,
		GroupName:  nodeGroup,
		AllowedUsages: []certificatesv1.KeyUsage{
			certificatesv1.UsageKeyEncipherment,
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageClientAuth,
		},
		AllowedUsagesNoRSA: []certificatesv1.KeyUsage{
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageClientAuth,
		},
		RequiredGroups:     []string{}, // Client certs don't require specific groups in CSR spec
		ValidateNodeExists: false,      // Node doesn't exist yet during bootstrapping
		AllowDNSNames:      false,
		AllowIPAddresses:   false,
	}

	// KubeletServingCertType defines validation rules for kubelet serving certificates
	KubeletServingCertType = CertificateType{
		Name:       "kubelet-serving",
		UserPrefix: NodeUserNamePrefix,
		GroupName:  nodeGroup,
		AllowedUsages: []certificatesv1.KeyUsage{
			certificatesv1.UsageKeyEncipherment,
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageServerAuth,
		},
		AllowedUsagesNoRSA: []certificatesv1.KeyUsage{
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageServerAuth,
		},
		RequiredGroups:     []string{nodeGroup, systemPrefix}, // Serving certs require specific groups
		ValidateNodeExists: true,                              // Node must exist for serving certs
		AllowDNSNames:      true,                              // Serving certs can have DNS names
		AllowIPAddresses:   true,                              // Serving certs can have IP addresses
	}

	// WICDCertType defines validation rules for WICD certificates
	WICDCertType = CertificateType{
		Name:       "wicd",
		UserPrefix: rbac.WICDUserPrefix,
		GroupName:  rbac.WICDGroupName,
		AllowedUsages: []certificatesv1.KeyUsage{
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageClientAuth,
			certificatesv1.UsageKeyEncipherment,
		},
		AllowedUsagesNoRSA: []certificatesv1.KeyUsage{
			certificatesv1.UsageDigitalSignature,
			certificatesv1.UsageClientAuth,
		},
		RequiredGroups:     []string{}, // WICD CSRs come from service account, groups are set by K8s
		ValidateNodeExists: true,       // Node must exist for WICD certs
		AllowDNSNames:      false,      // WICD certs don't need DNS names
		AllowIPAddresses:   false,      // WICD certs don't need IP addresses
	}
)
