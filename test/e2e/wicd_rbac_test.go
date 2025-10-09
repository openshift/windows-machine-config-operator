package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/csr/validation"
	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

// testWICDCSRApproval tests that WICD CSRs created during node configuration are properly approved
func (tc *testContext) testWICDCSRApproval(t *testing.T) {
	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")

	// Look for WICD CSRs that should have been created during node configuration
	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			wicdCSRs, err := findWICDCSRs(tc, node.Name)
			if err != nil {
				t.Fatalf("Failed to search for WICD CSRs for node %s: %v", node.Name, err)
			}

			if len(wicdCSRs) == 0 {
				t.Logf("No WICD CSRs found for node %s", node.Name)
				return
			}
			t.Logf("Found %d WICD CSR(s) for node %s", len(wicdCSRs), node.Name)
			for _, csr := range wicdCSRs {
				t.Run(csr.Name, func(t *testing.T) {
					// Verify it was approved by WICD controller with correct reason
					approvedByWICD := false
					for _, condition := range csr.Status.Conditions {
						if condition.Type == certificatesv1.CertificateApproved &&
							condition.Reason == "WICDAutoApproved" {
							approvedByWICD = true
							break
						}
					}
					if !approvedByWICD {
						t.Errorf("WICD CSR %s should be approved by WICD controller with reason 'WICDAutoApproved'",
							csr.Name)
					}

					// Verify CSR username matches expected WICD identity format
					expectedCertUsername := "system:wicd-node:" + node.Name
					expectedSAUsername := fmt.Sprintf("system:serviceaccount:%s:%s", wmcoNamespace, windows.WicdServiceName)

					if csr.Spec.Username != expectedCertUsername && csr.Spec.Username != expectedSAUsername {
						t.Errorf("WICD CSR %s username should be '%s' or '%s', got '%s'",
							csr.Name, expectedCertUsername, expectedSAUsername, csr.Spec.Username)
					}

					if csr.Spec.Username == expectedCertUsername {
						// Certificate-based CSRs should have WICD group
						hasWICDGroup := false
						for _, group := range csr.Spec.Groups {
							if group == rbac.WICDGroupName {
								hasWICDGroup = true
								break
							}
						}
						if !hasWICDGroup {
							t.Errorf("WICD certificate CSR %s should include group '%s', got groups: %v",
								csr.Name, rbac.WICDGroupName, csr.Spec.Groups)
						}
					}
					t.Logf("WICD CSR %s validated successfully - approved by WICD controller for node %s",
						csr.Name, node.Name)
				})
			}
		})
	}
}

// findWICDCSRs finds CSRs that match WICD identity patterns (ServiceAccount or certificate-based) for the given node
func findWICDCSRs(testCtx *testContext, nodeName string) ([]certificatesv1.CertificateSigningRequest, error) {
	var wicdCSRs []certificatesv1.CertificateSigningRequest
	csrs, err := testCtx.client.K8s.CertificatesV1().CertificateSigningRequests().List(context.TODO(),
		metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	expectedCertUsername := "system:wicd-node:" + nodeName
	expectedSAUsername := fmt.Sprintf("system:serviceaccount:%s:%s", wmcoNamespace, windows.WicdServiceName)
	for _, csr := range csrs.Items {
		// Check if CSR matches either WICD identity (certificate-based or ServiceAccount)
		if csr.Spec.Username == expectedCertUsername || csr.Spec.Username == expectedSAUsername {
			if isValidWICDCSR(&csr, nodeName) {
				wicdCSRs = append(wicdCSRs, csr)
			}
		}
	}

	return wicdCSRs, nil
}

// isValidWICDCSR validates that a CSR is actually from WICD by checking the certificate content
func isValidWICDCSR(csr *certificatesv1.CertificateSigningRequest, nodeName string) bool {
	// Parse the CSR to validate its content
	parsedCSR, err := validation.ParseCSR(csr.Spec.Request)
	if err != nil {
		return false
	}

	// Check if the Common Name matches the expected WICD certificate identity
	expectedCommonName := fmt.Sprintf("%s:%s", rbac.WICDUserPrefix, nodeName)
	if parsedCSR.Subject.CommonName != expectedCommonName {
		return false
	}

	// Check if the Organization includes the WICD group
	for _, org := range parsedCSR.Subject.Organization {
		if org == rbac.WICDGroupName {
			return true
		}
	}

	return false
}
