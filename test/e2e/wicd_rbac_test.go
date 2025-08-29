package e2e

import (
	"context"
	"testing"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

// wicdRBACTestSuite tests WICD RBAC manifests and certificate functionality
func wicdRBACTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	if err != nil {
		t.Fatalf("Failed to create test context: %v", err)
	}

	// Get all Windows nodes to determine if RBAC is relevant
	windowsNodes, err := testCtx.client.K8s.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "kubernetes.io/os=windows",
	})
	if err != nil {
		t.Fatalf("Failed to list Windows nodes: %v", err)
	}

	if len(windowsNodes.Items) == 0 {
		t.Skip("No Windows nodes found, skipping WICD RBAC tests")
		return
	}

	// Test WICD CSR approval functionality
	t.Run("WICD CSR Approval", func(t *testing.T) {
		testWICDCSRApproval(t, testCtx, windowsNodes.Items)
	})
}

// testWICDCSRApproval tests that WICD CSRs created during node configuration are properly approved
func testWICDCSRApproval(t *testing.T, testCtx *testContext, windowsNodes []corev1.Node) {
	// Look for WICD CSRs that should have been created during node configuration
	for _, node := range windowsNodes {
		t.Run(node.Name, func(t *testing.T) {
			// Find CSRs for this node that match WICD identity pattern
			wicdCSRs, err := findWICDCSRs(testCtx, node.Name)
			if err != nil {
				t.Fatalf("Failed to search for WICD CSRs for node %s: %v", node.Name, err)
			}

			if len(wicdCSRs) == 0 {
				t.Logf("No WICD CSRs found for node %s - may not be using certificate-based auth yet", node.Name)
				return
			}

			t.Logf("Found %d WICD CSR(s) for node %s", len(wicdCSRs), node.Name)

			// Test each WICD CSR
			for _, csr := range wicdCSRs {
				t.Run(csr.Name, func(t *testing.T) {
					// Test 1: Verify CSR is approved
					if !isCSRApproved(&csr) {
						t.Errorf("WICD CSR %s should be approved", csr.Name)
						return
					}

					// Test 2: Verify it was approved by WICD controller with correct reason
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

					// Test 3: Verify CSR username matches expected WICD identity format
					expectedUsername := "system:wicd-node:" + node.Name
					if csr.Spec.Username != expectedUsername {
						t.Errorf("WICD CSR %s username should be '%s', got '%s'",
							csr.Name, expectedUsername, csr.Spec.Username)
					}

					// Test 4: Verify CSR groups include WICD group
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

// isCSRApproved checks if a CSR has been approved
func isCSRApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved &&
			condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
