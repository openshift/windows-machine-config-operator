package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

// wicdRBACTestSuite tests WICD RBAC manifests and certificate functionality
func wicdRBACTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run("WICD CSR Approval", func(t *testing.T) {
		testWICDCSRApproval(t, testCtx)
	})
}

// testWICDCSRApproval tests that WICD CSRs created during node configuration are properly approved
func testWICDCSRApproval(t *testing.T, testCtx *testContext) {
	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")

	// Look for WICD CSRs that should have been created during node configuration
	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			// Find CSRs for this node that match WICD identity pattern
			wicdCSRs, err := findWICDCSRs(testCtx, node.Name)
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
						t.Errorf("WICD CSR %s should be approved by WICD controller with reason 'WICDAutoApproved'", csr.Name)
					}

					// Verify CSR username matches expected WICD identity format
					expectedUsername := "system:wicd-node:" + node.Name
					if csr.Spec.Username != expectedUsername {
						t.Errorf("WICD CSR %s username should be '%s', got '%s'",
							csr.Name, expectedUsername, csr.Spec.Username)
					}

					// Verify CSR groups include WICD group
					hasWICDGroup := false
					for _, group := range csr.Spec.Groups {
						if group == rbac.WICDGroupName {
							hasWICDGroup = true
							break
						}
					}
					if !hasWICDGroup {
						t.Errorf("WICD CSR %s should include group '%s', got groups: %v",
							csr.Name, rbac.WICDGroupName, csr.Spec.Groups)
					}

					t.Logf("WICD CSR %s validated successfully - approved by WICD controller for node %s",
						csr.Name, node.Name)
				})
			}
		})
	}
}

// findWICDCSRs finds CSRs that match WICD certificate identity pattern for the given node
func findWICDCSRs(testCtx *testContext, nodeName string) ([]certificatesv1.CertificateSigningRequest, error) {
	var wicdCSRs []certificatesv1.CertificateSigningRequest

	csrs, err := testCtx.client.K8s.CertificatesV1().CertificateSigningRequests().List(context.TODO(),
		metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	expectedUsername := "system:wicd-node:" + nodeName

	for _, csr := range csrs.Items {
		// Look for CSRs with WICD certificate-based identity
		if csr.Spec.Username == expectedUsername {
			wicdCSRs = append(wicdCSRs, csr)
		}
	}

	return wicdCSRs, nil
}
