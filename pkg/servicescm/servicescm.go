package servicescm

import (
	"fmt"

	"github.com/openshift/windows-machine-config-operator/version"
)

// ServicesConfigMapName is the name of the ConfigMap detailing service configuration for a specific WMCO version
var ServicesConfigMapName string

// init runs once, initializing global variables
func init() {
	ServicesConfigMapName = servicesConfigMapName()
}

// NodeCmdArg is a variable in a Windows command whose value is sourced from the node object associated with a instance
type NodeCmdArg struct {
	// Name is the variable name as it appears in commands
	Name string `json:"name"`
	// NodeObjectJsonPath is the JSON path of a field within an instance's Node object
	NodeObjectJsonPath string `json:"nodeObjectJsonPath"`
}

// PowershellCmdArg is a variable in a Windows command whose value is determined from the output of a PowerShell script
type PowershellCmdArg struct {
	// Name is the variable name as it appears in commands
	Name string `json:"name"`
	// Path is the location of the PowerShell script to be run
	Path string `json:"path"`
}

// Service represents the configuration spec of a Windows service
type Service struct {
	// Name is the name of the Windows service
	Name string `json:"name"`
	// Command is command that will be executed. This could potentially include strings whose values will be derived
	// from NodeVariablesInCommand and PowershellVariablesInCommand.
	Command string `json:"path"`
	// Before a command is run on a Windows instance, all node and PowerShell variables will be replaced by their values
	NodeVariablesInCommand       []NodeCmdArg       `json:"nodeVariablesInCommand,omitempty"`
	PowershellVariablesInCommand []PowershellCmdArg `json:"powershellVariablesInCommand,omitempty"`
	// Dependencies is a list of service names that this service is dependent on
	Dependencies []string `json:"dependencies,omitempty"`
	// Bootstrap is a boolean flag indicating whether this service should be handled as part of node bootstrapping
	Bootstrap bool `json:"bootstrap"`
	// Priority is a non-negative integer that will be used to order the creation of the services.
	// Priority 0 is created first
	Priority uint `json:"priority"`
}

// FileInfo contains the path and checksum of a file copied to an instance by WMCO
type FileInfo struct {
	// Path is the filepath of a file on an instance
	Path string `json:"path"`
	// Checksum is used to validate that a file has not been changed
	Checksum string `json:"checksum"`
}

// servicesConfigMapName returns the ConfigMap with the naming scheme:
// windows-services-<WMCOFullVersion>
func servicesConfigMapName() string {
	return fmt.Sprintf("windows-services-%s", version.Get())
}
