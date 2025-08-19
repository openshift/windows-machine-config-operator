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

// wicdRBACTestSuite tests that WICD can only write to its own node
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

	// Test static RBAC manifests exist and are correct
	t.Run("Static RBAC Manifests", func(t *testing.T) {
		testStaticRBACManifests(t, testCtx)
	})

	// Test webhook service deployment
	t.Run("Webhook Service", func(t *testing.T) {
		testWebhookService(t, testCtx)
	})

	// Test own node vs cross-node access restrictions
	if len(windowsNodes.Items) >= 2 {
		t.Run("Node Access Restrictions", func(t *testing.T) {
			testNodeAccessRestrictions(t, testCtx, windowsNodes.Items)
		})
	} else {
		t.Skip("Need at least 2 Windows nodes for cross-node access testing")
	}
}

// testStaticRBACManifests verifies that our static RBAC manifests exist and are correct
func testStaticRBACManifests(t *testing.T, testCtx *testContext) {
	ctx := context.TODO()

	// Test 1: Bootstrap ServiceAccount ClusterRole exists
	bootstrapCR, err := testCtx.client.K8s.RbacV1().ClusterRoles().Get(ctx, "windows-instance-config-daemon", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Bootstrap ClusterRole 'windows-instance-config-daemon' should exist: %v", err)
	}

	// Verify bootstrap permissions are minimal (CSR creation, ConfigMap read)
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

	// Verify certificate permissions include node patching
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
}

// testWebhookService tests webhook service deployment and configuration
func testWebhookService(t *testing.T, testCtx *testContext) {
	ctx := context.TODO()

	// Verify webhook service exists and is properly configured
	service, err := testCtx.client.K8s.CoreV1().Services(wmcoNamespace).Get(ctx, "windows-machine-config-operator-webhook-service", metav1.GetOptions{})
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

// testNodeAccessRestrictions tests that WICD can access its own node but not other nodes
func testNodeAccessRestrictions(t *testing.T, testCtx *testContext, windowsNodes []core.Node) {
	if len(windowsNodes) < 2 {
		t.Skip("Need at least 2 Windows nodes for cross-node access testing")
		return
	}

	nodeA := &windowsNodes[0]
	nodeB := &windowsNodes[1]

	// Test 1: Own node access - WICD for nodeA should be able to patch nodeA
	nodeAWicdUser := fmt.Sprintf("system:wicd-node:%s", nodeA.Name)
	nodeAClient, err := createImpersonationClient(testCtx, nodeAWicdUser, []string{rbac.WICDGroupName})
	if err != nil {
		t.Skipf("Impersonation not available in test environment: %v", err)
		return
	}

	// Test that WICD can read its own node
	_, err = nodeAClient.CoreV1().Nodes().Get(context.TODO(), nodeA.Name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("WICD should be able to read its own node %s: %v", nodeA.Name, err)
	}

	// Test that WICD can patch its own node
	patchData := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"test.wicd.own.node": "allowed",
			},
		},
	}
	patchBytes, _ := json.Marshal(patchData)
	_, err = nodeAClient.CoreV1().Nodes().Patch(context.TODO(), nodeA.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		t.Errorf("WICD should be able to patch its own node %s: %v", nodeA.Name, err)
	}

	// Clean up test annotation
	delete(patchData["metadata"].(map[string]interface{})["annotations"].(map[string]string), "test.wicd.own.node")
	patchBytes, _ = json.Marshal(patchData)
	testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), nodeA.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})

	// Test 2: Cross-node access - WICD for nodeA should NOT be able to patch nodeB
	patchData = map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				"test.wicd.cross.node": "blocked",
			},
		},
	}
	patchBytes, _ = json.Marshal(patchData)
	_, err = nodeAClient.CoreV1().Nodes().Patch(context.TODO(), nodeB.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err == nil {
		t.Errorf("WICD should NOT be able to patch other node %s from %s", nodeB.Name, nodeA.Name)
		// Clean up if somehow the patch succeeded
		delete(patchData["metadata"].(map[string]interface{})["annotations"].(map[string]string), "test.wicd.cross.node")
		patchBytes, _ = json.Marshal(patchData)
		testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), nodeB.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	} else if !kerrors.IsForbidden(err) || !strings.Contains(err.Error(), "admission webhook") {
		t.Errorf("Expected webhook denial for cross-node access, got: %v", err)
	} else {
		t.Logf("Webhook correctly blocked cross-node access from %s to %s", nodeA.Name, nodeB.Name)
	}

	// Test 3: WMCO should be allowed to patch any node (for cluster operations)
	wmcoClient, err := createWMCOServiceAccountClient(testCtx)
	if err != nil {
		t.Fatalf("Failed to create WMCO ServiceAccount client: %v", err)
	}

	wmcoAnnotation := "windowsmachineconfig.openshift.io/test-wmco-access"
	patchData = map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				wmcoAnnotation: "allowed",
			},
		},
	}
	patchBytes, _ = json.Marshal(patchData)
	_, err = wmcoClient.CoreV1().Nodes().Patch(context.TODO(), nodeA.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		t.Errorf("WMCO should be able to patch any node for cluster operations: %v", err)
	}

	// Clean up WMCO test annotation
	delete(patchData["metadata"].(map[string]interface{})["annotations"].(map[string]string), wmcoAnnotation)
	patchBytes, _ = json.Marshal(patchData)
	testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), nodeA.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
}

// createImpersonationClient creates a client that impersonates a specific user and groups
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
