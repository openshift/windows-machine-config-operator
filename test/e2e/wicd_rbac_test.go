package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrlruntimecfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

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

	// Test static RBAC manifests exist
	t.Run("Static RBAC Manifests", func(t *testing.T) {
		testStaticRBACManifests(t, testCtx)
	})

	// Test webhook validation and cross-node access enforcement
	t.Run("Webhook Enforcement", func(t *testing.T) {
		testWebhookEnforcement(t, testCtx, windowsNodes.Items)
	})

	// Test webhook service deployment
	t.Run("Webhook Service", func(t *testing.T) {
		testWebhookService(t, testCtx)
	})
}

// testStaticRBACManifests verifies that our static RBAC manifests exist and are correct
func testStaticRBACManifests(t *testing.T, testCtx *testContext) {
	ctx := context.TODO()

	// Test 1: Bootstrap ServiceAccount ClusterRole exists
	bootstrapCR, err := testCtx.client.K8s.RbacV1().ClusterRoles().Get(ctx, "windows-instance-config-daemon", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Bootstrap ClusterRole 'windows-instance-config-daemon' should exist: %v", err)
	}

	// Verify bootstrap permissions are minimal
	hasCSRCreate := false
	hasConfigMapRead := false
	for _, rule := range bootstrapCR.Rules {
		for _, resource := range rule.Resources {
			if resource == "certificatesigningrequests" {
				for _, verb := range rule.Verbs {
					if verb == "create" {
						hasCSRCreate = true
					}
				}
			}
			if resource == "configmaps" {
				for _, verb := range rule.Verbs {
					if verb == "get" || verb == "list" {
						hasConfigMapRead = true
					}
				}
			}
		}
	}
	if !hasCSRCreate {
		t.Error("Bootstrap ClusterRole should allow CSR creation")
	}
	if !hasConfigMapRead {
		t.Error("Bootstrap ClusterRole should allow ConfigMap read")
	}

	// Test 2: Certificate-based ClusterRole exists
	certCR, err := testCtx.client.K8s.RbacV1().ClusterRoles().Get(ctx, rbac.WICDClusterRoleName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Certificate ClusterRole '%s' should exist: %v", rbac.WICDClusterRoleName, err)
	}

	// Verify certificate permissions are enhanced
	hasNodePatch := false
	for _, rule := range certCR.Rules {
		for _, resource := range rule.Resources {
			if resource == "nodes" {
				for _, verb := range rule.Verbs {
					if verb == "patch" {
						hasNodePatch = true
					}
				}
			}
		}
	}
	if !hasNodePatch {
		t.Error("Certificate ClusterRole should allow node patching")
	}

	// Test 3: Certificate-based ClusterRoleBinding exists
	certCRB, err := testCtx.client.K8s.RbacV1().ClusterRoleBindings().Get(ctx, rbac.WICDClusterRoleBindingName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Certificate ClusterRoleBinding '%s' should exist: %v", rbac.WICDClusterRoleBindingName, err)
	}

	// Verify it binds to the correct group
	hasCorrectGroup := false
	for _, subject := range certCRB.Subjects {
		if subject.Kind == rbacv1.GroupKind && subject.Name == rbac.WICDGroupName {
			hasCorrectGroup = true
		}
	}
	if !hasCorrectGroup {
		t.Errorf("Certificate ClusterRoleBinding should bind to group '%s'", rbac.WICDGroupName)
	}

	// All static RBAC manifests validated successfully
}

// TestWICDNodeAccessRestriction tests that WICD can only write to its own node
func TestWICDNodeAccessRestriction(t *testing.T) {
	// 1. CSR controller validates certificates are only issued to legitimate nodes
	// 2. Certificate provides identity (system:wicd-node:nodename)
	// 3. RBAC gives broad permissions to the certificate group
	// 4. Admission webhook enforces node-specific access + allowed operations only

	// Run the comprehensive WICD RBAC test suite
	wicdRBACTestSuite(t)
}

// testWebhookEnforcement tests cross-node access restrictions and webhook behavior
func testWebhookEnforcement(t *testing.T, testCtx *testContext, windowsNodes []core.Node) {
	if len(windowsNodes) < 2 {
		t.Skip("Need at least 2 Windows nodes for cross-node access testing")
		return
	}

	nodeA := &windowsNodes[0]
	nodeB := &windowsNodes[1]

	// Test webhook deployment and basic functionality
	t.Run("WebhookDeploymentValidation", func(t *testing.T) {
		testWebhookService(t, testCtx)
	})

	// Test webhook blocking behavior with unauthorized actions
	t.Run("WebhookBlockingValidation", func(t *testing.T) {
		testWebhookBlockingValidation(t, testCtx, nodeA, nodeB)
	})

}

// createImpersonationClient creates a client that impersonates a specific user and groups
// This simulates certificate-based authentication for testing webhook behavior
func createImpersonationClient(testCtx *testContext, username string, groups []string) (*kubernetes.Clientset, error) {
	restConfig, err := ctrlruntimecfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get rest config: %w", err)
	}

	// Create config with impersonation headers
	config := rest.CopyConfig(restConfig)
	config.Impersonate = rest.ImpersonationConfig{
		UserName: username,
		Groups:   groups,
	}

	return kubernetes.NewForConfig(config)
}

// createWICDServiceAccountClient creates a client using WICD ServiceAccount token
func createWICDServiceAccountClient(testCtx *testContext) (*kubernetes.Clientset, error) {
	serviceAccountName := "windows-instance-config-daemon"

	tokenRequest := &auth.TokenRequest{
		Spec: auth.TokenRequestSpec{},
	}

	token, err := testCtx.client.K8s.CoreV1().ServiceAccounts(wmcoNamespace).CreateToken(context.TODO(),
		serviceAccountName, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get bearer token for service account %s: %w", serviceAccountName, err)
	}

	restConfig, err := ctrlruntimecfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get rest config: %w", err)
	}

	config := &rest.Config{
		Host: restConfig.Host,
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:     restConfig.TLSClientConfig.CAFile,
			CAData:     restConfig.TLSClientConfig.CAData,
			ServerName: restConfig.TLSClientConfig.ServerName,
			Insecure:   restConfig.TLSClientConfig.Insecure,
		},
		BearerToken: token.Status.Token,
	}

	return kubernetes.NewForConfig(config)
}

// createWMCOServiceAccountClient creates a client using WMCO ServiceAccount token
func createWMCOServiceAccountClient(testCtx *testContext) (*kubernetes.Clientset, error) {
	serviceAccountName := "windows-machine-config-operator"

	tokenRequest := &auth.TokenRequest{
		Spec: auth.TokenRequestSpec{},
	}

	token, err := testCtx.client.K8s.CoreV1().ServiceAccounts(wmcoNamespace).CreateToken(context.TODO(),
		serviceAccountName, tokenRequest, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not get bearer token for service account %s: %w", serviceAccountName, err)
	}

	restConfig, err := ctrlruntimecfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get rest config: %w", err)
	}

	config := &rest.Config{
		Host: restConfig.Host,
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:     restConfig.TLSClientConfig.CAFile,
			CAData:     restConfig.TLSClientConfig.CAData,
			ServerName: restConfig.TLSClientConfig.ServerName,
			Insecure:   restConfig.TLSClientConfig.Insecure,
		},
		BearerToken: token.Status.Token,
	}

	return kubernetes.NewForConfig(config)
}

// testWebhookBlockingValidation tests that webhook blocks unauthorized actions
func testWebhookBlockingValidation(t *testing.T, testCtx *testContext, targetNode *core.Node, otherNode *core.Node) {
	node, err := testCtx.client.K8s.CoreV1().Nodes().Get(context.TODO(), targetNode.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get node: %v", err)
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	// NEGATIVE TEST: Cross-node access test using legitimate WICD user and real WICD annotation
	// This tests the core webhook functionality: node-specific access control
	// - Target: targetNode (nodeA)
	// - User: WICD identity for otherNode (nodeB)
	// - Annotation: Real WICD annotation that should be allowed on own node but blocked on other nodes
	// - Expected: Webhook blocks cross-node access even with valid RBAC permissions
	crossNodeWicdUser := fmt.Sprintf("system:wicd-node:%s", otherNode.Name)
	crossNodeClient, err := createImpersonationClient(testCtx, crossNodeWicdUser, []string{rbac.WICDGroupName})
	if err != nil {
		t.Skipf("Impersonation not available in test environment: %v", err)
		return
	}

	// Use a real WICD annotation that should be allowed on own node but blocked cross-node
	blockedAnnotation := "windowsmachineconfig.openshift.io/reboot-required"
	node.Annotations[blockedAnnotation] = "true"

	_, err = patchNodeAnnotations(crossNodeClient, targetNode.Name, node.Annotations)
	if err == nil {
		// FAILURE: Webhook should have blocked cross-node access
		t.Errorf("Expected webhook to block cross-node access from %s to %s, but patch succeeded", otherNode.Name, targetNode.Name)
		t.Errorf("This indicates webhook is not functioning (likely TLS certificate issues)")
		// Clean up the annotation since the test failed
		delete(node.Annotations, blockedAnnotation)
		patchNodeAnnotations(testCtx.client.K8s, targetNode.Name, node.Annotations)
	} else if kerrors.IsForbidden(err) && strings.Contains(err.Error(), "admission webhook") {
		t.Logf("Webhook correctly blocked cross-node access from %s to %s: %v", otherNode.Name, targetNode.Name, err)
	} else {
		t.Errorf("Expected webhook denial for cross-node access, but got different error: %v", err)
	}

	wmcoSAClient, err := createWMCOServiceAccountClient(testCtx)
	if err != nil {
		t.Fatalf("Failed to create WMCO ServiceAccount client: %v", err)
	}

	wmcoAnnotation := "windowsmachineconfig.openshift.io/desired-version"
	node.Annotations[wmcoAnnotation] = "test-version"

	_, err = patchNodeAnnotations(wmcoSAClient, targetNode.Name, node.Annotations)
	if err != nil {
		t.Errorf("WMCO ServiceAccount should be allowed to set WICD annotations, but got error: %v", err)
	}

	delete(node.Annotations, wmcoAnnotation)
	patchNodeAnnotations(testCtx.client.K8s, targetNode.Name, node.Annotations)
}

// patchNodeAnnotations patches a node's annotations using the PATCH API
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

// testWebhookService tests webhook service deployment and configuration
func testWebhookService(t *testing.T, testCtx *testContext) {
	ctx := context.TODO()

	// Verify webhook service exists and is properly configured
	service, err := testCtx.client.K8s.CoreV1().Services("openshift-windows-machine-config-operator").Get(ctx, "windows-machine-config-operator-webhook-service", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("WICD webhook service should exist: %v", err)
	}

	// Verify service has serving certificate annotation
	if service.Annotations["service.beta.openshift.io/serving-cert-secret-name"] == "" {
		t.Error("Webhook service should have serving certificate annotation")
	}

	// Verify service targets correct port
	hasWebhookPort := false
	for _, port := range service.Spec.Ports {
		if port.Name == "webhook-server" && port.Port == 443 && port.TargetPort.IntVal == 9443 {
			hasWebhookPort = true
			break
		}
	}
	if !hasWebhookPort {
		t.Error("Webhook service should expose port 443 targeting 9443")
	}

}
