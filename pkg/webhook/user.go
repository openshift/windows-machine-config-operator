package webhook

import (
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/rbac"
)

// wicdNodeIdentity extracts the node name from a WICD certificate-based user identity.
// Returns (nodeName, true) if the user is a WICD node, ("", false) otherwise.
//
// Expected username format: "system:wicd-node:nodename"
// Based on ovn-kubernetes/go-controller/pkg/ovnwebhook/user.go:ovnkubeNodeIdentity
func wicdNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if !strings.HasPrefix(user.Username, rbac.WICDUserPrefix+":") {
		return "", false
	}

	// Extract node name from certificate identity
	// "system:wicd-node:ip-10-0-61-109.us-west-1.compute.internal" -> "ip-10-0-61-109.us-west-1.compute.internal"
	nodeName := strings.TrimPrefix(user.Username, rbac.WICDUserPrefix+":")
	return nodeName, true
}

// isWMCOServiceAccount checks if the user is the WMCO ServiceAccount.
// WMCO ServiceAccount gets broad annotation access for Windows node management.
func isWMCOServiceAccount(user authenticationv1.UserInfo) bool {
	return user.Username == "system:serviceaccount:openshift-windows-machine-config-operator:windows-machine-config-operator"
}

// isWICDServiceAccount checks if the user is the WICD ServiceAccount (bootstrap authentication).
// WICD ServiceAccount gets restricted access to WICD-specific annotations only.
func isWICDServiceAccount(user authenticationv1.UserInfo) bool {
	return user.Username == "system:serviceaccount:openshift-windows-machine-config-operator:windows-instance-config-daemon"
}
