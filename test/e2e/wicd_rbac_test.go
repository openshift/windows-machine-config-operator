package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrlruntimecfg "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// wicdRBACTestSuite tests WICD RBAC functionality
func wicdRBACTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	if err != nil {
		t.Fatalf("Failed to create test context: %v", err)
	}

	// Get all Windows nodes to determine which tests to run
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

	if len(windowsNodes.Items) < 2 {
		t.Skip("Test requires at least 2 Windows nodes for comprehensive RBAC validation")
		return
	}

	// Run the comprehensive node access restriction test
	t.Run("Node Access Restriction", TestWICDNodeAccessRestriction)
}

// TestWICDNodeAccessRestriction tests that WICD can only write to its own node
func TestWICDNodeAccessRestriction(t *testing.T) {
	testCtx, err := NewTestContext()
	if err != nil {
		t.Fatalf("Failed to create test context: %v", err)
	}

	// Get all Windows nodes
	windowsNodes, err := testCtx.client.K8s.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "kubernetes.io/os=windows",
	})
	if err != nil {
		t.Fatalf("Failed to list Windows nodes: %v", err)
	}

	if len(windowsNodes.Items) < 2 {
		t.Skip("Test requires at least 2 Windows nodes")
	}

	// Test each node's RBAC restrictions
	for _, node := range windowsNodes.Items {
		t.Run(fmt.Sprintf("Node-%s", node.Name), func(t *testing.T) {
			testNodeRBACRestriction(t, testCtx, &node, windowsNodes.Items)
		})

		// Test all nodes to thoroughly validate RBAC isolation between nodes
		// This ensures Node A WICD can only access Node A, Node B WICD can only access Node B, etc.
	}
}

func testNodeRBACRestriction(t *testing.T, testCtx *testContext, targetNode *core.Node, allNodes []core.Node) {
	// Create a test client using the WICD ServiceAccount for this node
	wicdClient, err := createWICDClient(testCtx, targetNode.Name)
	if err != nil {
		t.Fatalf("Failed to create WICD client for node %s: %v", targetNode.Name, err)
	}

	// Test 1: WICD should be able to update its own node
	t.Run("CanUpdateOwnNode", func(t *testing.T) {
		// Try to add a test annotation to its own node
		testAnnotation := "test.wmco.io/rbac-test"
		testValue := "allowed"

		// Get fresh copy of the node
		node, err := wicdClient.CoreV1().Nodes().Get(context.TODO(), targetNode.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Failed to get own node: %v", err)
		}

		// Add test annotation
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[testAnnotation] = testValue

		// Patch the node (uses PATCH API which RBAC allows, not UPDATE)
		_, err = patchNodeAnnotations(wicdClient, targetNode.Name, node.Annotations)
		if err != nil {
			t.Errorf("WICD should be able to update its own node %s, but got error: %v", targetNode.Name, err)
		}

		// Clean up the test annotation
		delete(node.Annotations, testAnnotation)
		patchNodeAnnotations(wicdClient, targetNode.Name, node.Annotations)
	})

	// Test 2: WICD should NOT be able to update other nodes
	t.Run("CannotUpdateOtherNodes", func(t *testing.T) {
		for _, otherNode := range allNodes {
			if otherNode.Name == targetNode.Name {
				continue // Skip own node
			}

			// Try to get other node (should fail or succeed based on RBAC)
			node, err := wicdClient.CoreV1().Nodes().Get(context.TODO(), otherNode.Name, metav1.GetOptions{})
			if err != nil {
				// If we can't even get the node, that's good - RBAC is working
				if kerrors.IsForbidden(err) {
					t.Logf("Good: WICD correctly forbidden from accessing node %s: %v", otherNode.Name, err)
					continue
				}
				t.Errorf("Unexpected error accessing node %s: %v", otherNode.Name, err)
				continue
			}

			// If we can get the node, try to update it (this should fail)
			testAnnotation := "test.wmco.io/rbac-test"
			testValue := "should-fail"

			if node.Annotations == nil {
				node.Annotations = make(map[string]string)
			}
			originalValue := node.Annotations[testAnnotation]
			node.Annotations[testAnnotation] = testValue

			_, err = patchNodeAnnotations(wicdClient, otherNode.Name, node.Annotations)
			if err == nil {
				t.Errorf("WICD should NOT be able to update other node %s, but update succeeded", otherNode.Name)
				// Clean up if update somehow succeeded
				node.Annotations[testAnnotation] = originalValue
				if originalValue == "" {
					delete(node.Annotations, testAnnotation)
				}
				patchNodeAnnotations(wicdClient, otherNode.Name, node.Annotations)
			} else if kerrors.IsForbidden(err) {
				t.Logf("Good: WICD correctly forbidden from updating node %s: %v", otherNode.Name, err)
			} else {
				t.Errorf("Expected Forbidden error when updating node %s, but got: %v", otherNode.Name, err)
			}
		}
	})
}

func createWICDClient(testCtx *testContext, nodeName string) (*kubernetes.Clientset, error) {
	// Get the ServiceAccount token for WICD using the modern CreateToken API
	// Use node-specific ServiceAccount based on the node name
	serviceAccountName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)

	// Debug: Print which ServiceAccount we're trying to use
	fmt.Printf("DEBUG: Attempting to use ServiceAccount: %s for node: %s\n", serviceAccountName, nodeName)

	// Create a token request for the WICD ServiceAccount
	tokenRequest := &auth.TokenRequest{
		Spec: auth.TokenRequestSpec{},
	}

	token, err := testCtx.client.K8s.CoreV1().ServiceAccounts(wmcoNamespace).CreateToken(context.TODO(),
		serviceAccountName, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get bearer token for service account %s: %w", serviceAccountName, err)
	}

	// Debug: Confirm token was created successfully
	fmt.Printf("DEBUG: Successfully created token for ServiceAccount: %s\n", serviceAccountName)

	// Get the rest config
	restConfig, err := ctrlruntimecfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get rest config: %w", err)
	}

	// Create a new client using this token - IMPORTANT: Create clean TLS config without client certificates
	// This ensures bearer token authentication is used instead of client certificate authentication
	config := &rest.Config{
		Host: restConfig.Host,
		TLSClientConfig: rest.TLSClientConfig{
			// Only copy CA data for server verification, NOT client certificates
			CAFile:     restConfig.TLSClientConfig.CAFile,
			CAData:     restConfig.TLSClientConfig.CAData,
			ServerName: restConfig.TLSClientConfig.ServerName,
			Insecure:   restConfig.TLSClientConfig.Insecure,
			// Explicitly exclude client cert fields to force bearer token auth:
			// CertFile, CertData, KeyFile, KeyData are intentionally omitted
		},
		BearerToken: token.Status.Token,
	}

	return kubernetes.NewForConfig(config)
}

// patchNodeAnnotations patches a node's annotations using the PATCH API (which RBAC allows)
// instead of UPDATE API (which requires broader permissions)
func patchNodeAnnotations(client kubernetes.Interface, nodeName string, annotations map[string]string) (*core.Node, error) {
	// Create a patch that only updates annotations
	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": annotations,
		},
	}

	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal patch data: %w", err)
	}

	return client.CoreV1().Nodes().Patch(context.TODO(), nodeName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
}
