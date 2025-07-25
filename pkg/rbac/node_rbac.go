package rbac

import (
	"context"
	"fmt"
	"reflect"

	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureNodeSpecificRBAC creates node-specific RBAC resources with proper resourceNames restriction
func EnsureNodeSpecificRBAC(ctx context.Context, ctrlRuntimeClient client.Client, k8sClient kubernetes.Interface, wmcoNamespace, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("node name cannot be empty")
	}

	// Create node-specific ServiceAccount
	if err := ensureNodeSpecificServiceAccount(ctx, k8sClient, wmcoNamespace, nodeName); err != nil {
		return err
	}

	// Create node-specific ClusterRole with resourceNames restriction
	if err := ensureNodeSpecificClusterRole(ctx, k8sClient, nodeName); err != nil {
		return err
	}

	// Create node-specific ClusterRoleBinding
	if err := ensureNodeSpecificClusterRoleBinding(ctx, k8sClient, wmcoNamespace, nodeName); err != nil {
		return err
	}

	return nil
}

// CleanupNodeSpecificRBAC removes node-specific RBAC resources for a deleted node
func CleanupNodeSpecificRBAC(ctx context.Context, ctrlRuntimeClient client.Client, k8sClient kubernetes.Interface, wmcoNamespace, nodeName string) error {
	// Clean up ServiceAccounts by label (handles both node-name and IP-based ServiceAccounts)
	labelSelector := fmt.Sprintf("wicd.openshift.io/node=%s,wicd.openshift.io/scope=node-specific", nodeName)
	serviceAccounts, err := k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).List(ctx, meta.ListOptions{
		LabelSelector: labelSelector,
	})
	if err == nil {
		for _, sa := range serviceAccounts.Items {
			if err := k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).Delete(ctx, sa.Name, meta.DeleteOptions{}); err != nil {
				if !k8sapierrors.IsNotFound(err) {
					return fmt.Errorf("failed to delete node-specific ServiceAccount %s: %w", sa.Name, err)
				}
			}
		}
	}

	// Clean up ServiceAccount token secrets by label
	secrets, err := k8sClient.CoreV1().Secrets(wmcoNamespace).List(ctx, meta.ListOptions{
		LabelSelector: labelSelector,
	})
	if err == nil {
		for _, secret := range secrets.Items {
			if err := k8sClient.CoreV1().Secrets(wmcoNamespace).Delete(ctx, secret.Name, meta.DeleteOptions{}); err != nil {
				if !k8sapierrors.IsNotFound(err) {
					return fmt.Errorf("failed to delete node-specific ServiceAccount secret %s: %w", secret.Name, err)
				}
			}
		}
	}

	// Also try to clean up by exact name (fallback for node-name based resources)
	serviceAccountName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)
	if err := k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).Delete(ctx, serviceAccountName, meta.DeleteOptions{}); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete node-specific ServiceAccount %s: %w", serviceAccountName, err)
		}
	}

	secretName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)
	if err := k8sClient.CoreV1().Secrets(wmcoNamespace).Delete(ctx, secretName, meta.DeleteOptions{}); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete node-specific ServiceAccount secret %s: %w", secretName, err)
		}
	}

	// Clean up ClusterRole
	roleName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)
	if err := k8sClient.RbacV1().ClusterRoles().Delete(ctx, roleName, meta.DeleteOptions{}); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete node-specific ClusterRole %s: %w", roleName, err)
		}
	}

	// Clean up ClusterRoleBinding
	bindingName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)
	if err := k8sClient.RbacV1().ClusterRoleBindings().Delete(ctx, bindingName, meta.DeleteOptions{}); err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete node-specific ClusterRoleBinding %s: %w", bindingName, err)
		}
	}

	return nil
}

// ensureNodeSpecificServiceAccount creates a ServiceAccount for the specific node
func ensureNodeSpecificServiceAccount(ctx context.Context, k8sClient kubernetes.Interface, wmcoNamespace, nodeName string) error {
	serviceAccountName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)

	existingSA, err := k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).Get(ctx, serviceAccountName, meta.GetOptions{})
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return fmt.Errorf("unable to get ServiceAccount %s: %w", serviceAccountName, err)
	}

	expectedSA := &core.ServiceAccount{
		ObjectMeta: meta.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: wmcoNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "windows-machine-config-operator",
				"app.kubernetes.io/part-of": "wicd",
				"wicd.openshift.io/node":    nodeName,
				"wicd.openshift.io/scope":   "node-specific",
			},
		},
	}

	if err == nil {
		// Check if existing ServiceAccount matches expected configuration
		if reflect.DeepEqual(existingSA.Labels, expectedSA.Labels) {
			return nil
		}

		// Delete existing ServiceAccount to update it
		if err := k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).Delete(ctx, serviceAccountName, meta.DeleteOptions{}); err != nil {
			return fmt.Errorf("unable to delete ServiceAccount %s: %w", serviceAccountName, err)
		}
	}

	// Create new ServiceAccount
	_, err = k8sClient.CoreV1().ServiceAccounts(wmcoNamespace).Create(ctx, expectedSA, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create ServiceAccount %s: %w", serviceAccountName, err)
	}

	return nil
}

// ensureNodeSpecificClusterRole creates a ClusterRole for the specific node with resourceNames restriction
func ensureNodeSpecificClusterRole(ctx context.Context, k8sClient kubernetes.Interface, nodeName string) error {
	roleName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)

	existingCR, err := k8sClient.RbacV1().ClusterRoles().Get(ctx, roleName, meta.GetOptions{})
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return fmt.Errorf("unable to get ClusterRole %s: %w", roleName, err)
	}

	expectedCR := &rbac.ClusterRole{
		ObjectMeta: meta.ObjectMeta{
			Name: roleName,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "windows-machine-config-operator",
				"app.kubernetes.io/part-of": "wicd",
				"wicd.openshift.io/node":    nodeName,
				"wicd.openshift.io/scope":   "node-specific",
			},
		},
		Rules: []rbac.PolicyRule{
			{
				// Allow reading ConfigMaps for bootstrap phase and cleanup
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list"},
			},
			{
				// Allow listing nodes for node discovery (no resourceNames restriction needed)
				APIGroups: []string{""},
				Resources: []string{"nodes"},
				Verbs:     []string{"list"},
			},
			{
				// Allow access only to this specific node
				APIGroups:     []string{""},
				Resources:     []string{"nodes"},
				ResourceNames: []string{nodeName},
				Verbs:         []string{"get", "patch"},
			},
			{
				// Allow status updates only for this specific node
				APIGroups:     []string{""},
				Resources:     []string{"nodes/status"},
				ResourceNames: []string{nodeName},
				Verbs:         []string{"patch", "update"},
			},
		},
	}

	if err == nil {
		// Check if existing ClusterRole matches expected configuration
		if reflect.DeepEqual(existingCR.Rules, expectedCR.Rules) &&
			reflect.DeepEqual(existingCR.Labels, expectedCR.Labels) {
			return nil
		}

		// Delete existing ClusterRole to update it
		if err := k8sClient.RbacV1().ClusterRoles().Delete(ctx, roleName, meta.DeleteOptions{}); err != nil {
			return fmt.Errorf("unable to delete ClusterRole %s: %w", roleName, err)
		}
	}

	// Create new ClusterRole
	_, err = k8sClient.RbacV1().ClusterRoles().Create(ctx, expectedCR, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create ClusterRole %s: %w", roleName, err)
	}

	return nil
}

// ensureNodeSpecificClusterRoleBinding creates a ClusterRoleBinding for the specific node
func ensureNodeSpecificClusterRoleBinding(ctx context.Context, k8sClient kubernetes.Interface, wmcoNamespace, nodeName string) error {
	bindingName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)
	serviceAccountName := fmt.Sprintf("windows-instance-config-daemon-%s", nodeName)

	existingCRB, err := k8sClient.RbacV1().ClusterRoleBindings().Get(ctx, bindingName, meta.GetOptions{})
	if err != nil && !k8sapierrors.IsNotFound(err) {
		return fmt.Errorf("unable to get ClusterRoleBinding %s: %w", bindingName, err)
	}

	expectedCRB := &rbac.ClusterRoleBinding{
		ObjectMeta: meta.ObjectMeta{
			Name: bindingName,
			Labels: map[string]string{
				"app.kubernetes.io/name":    "windows-machine-config-operator",
				"app.kubernetes.io/part-of": "wicd",
				"wicd.openshift.io/node":    nodeName,
				"wicd.openshift.io/scope":   "node-specific",
			},
		},
		RoleRef: rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     "ClusterRole",
			Name:     fmt.Sprintf("windows-instance-config-daemon-%s", nodeName),
		},
		Subjects: []rbac.Subject{{
			Kind:      rbac.ServiceAccountKind,
			Name:      serviceAccountName,
			Namespace: wmcoNamespace,
		}},
	}

	if err == nil {
		// Check if existing ClusterRoleBinding matches expected configuration
		if existingCRB.RoleRef.Name == expectedCRB.RoleRef.Name &&
			reflect.DeepEqual(existingCRB.Subjects, expectedCRB.Subjects) &&
			reflect.DeepEqual(existingCRB.Labels, expectedCRB.Labels) {
			return nil
		}

		// Delete existing ClusterRoleBinding to update it
		if err := k8sClient.RbacV1().ClusterRoleBindings().Delete(ctx, bindingName, meta.DeleteOptions{}); err != nil {
			return fmt.Errorf("unable to delete ClusterRoleBinding %s: %w", bindingName, err)
		}
	}

	// Create new ClusterRoleBinding
	_, err = k8sClient.RbacV1().ClusterRoleBindings().Create(ctx, expectedCRB, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create ClusterRoleBinding %s: %w", bindingName, err)
	}

	return nil
}
