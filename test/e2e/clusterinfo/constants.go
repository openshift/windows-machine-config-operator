package clusterinfo

const (
	MachineOSIDLabel = "machine.openshift.io/os-id"
	MachineSetLabel  = "machine.openshift.io/cluster-api-machineset"
	MachineRoleLabel = "machine.openshift.io/cluster-api-machine-role"
	MachineTypeLabel = "machine.openshift.io/cluster-api-machine-type"
	// MachineE2ELabel signifies that the Machine was created as part of the WMCO e2e tests
	MachineE2ELabel     = "e2e-wmco"
	MachineAPINamespace = "openshift-machine-api"
	UserDataSecretName  = "windows-user-data"
)
