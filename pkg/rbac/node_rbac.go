package rbac

const (
	// WICDGroupName is the group name for all WICD certificate users
	WICDGroupName = "system:wicd-nodes"
	// WICDUserPrefix is the prefix for WICD certificate usernames
	WICDUserPrefix = "system:wicd-node"
	// WICDClusterRoleName is the name of the ClusterRole for WICD group
	WICDClusterRoleName = "system-wicd-nodes"
	// WICDClusterRoleBindingName is the name of the ClusterRoleBinding for WICD group
	WICDClusterRoleBindingName = "system-wicd-nodes"
)
