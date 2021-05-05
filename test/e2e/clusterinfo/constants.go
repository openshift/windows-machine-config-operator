package clusterinfo

const (
	MachineOSIDLabel    = "machine.openshift.io/os-id"
	MachineSetLabel     = "machine.openshift.io/cluster-api-machineset"
	MachineRoleLabel    = "machine.openshift.io/cluster-api-machine-role"
	MachineTypeLabel    = "machine.openshift.io/cluster-api-machine-type"
	MachineAPINamespace = "openshift-machine-api"
	UserDataSecretName  = "windows-user-data"
)

// WindowsMachineSetName returns the name of the Windows MachineSet created in the e2e tests depending on if the
// Windows label is set or not
// TODO: Move this function to the providers package as part of https://issues.redhat.com/browse/WINC-608
func WindowsMachineSetName(isWindowsLabelSet bool) string {
	if isWindowsLabelSet {
		// Designate MachineSets that set a Windows label on the Machine with
		// "e2e-wm", to signify they should be configured by the Windows Machine controller
		return "e2e-wm"
	}
	return "e2e"
}
