package servicescm

import (
	"encoding/json"
	"fmt"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/version"
)

const (
	// NamePrefix is the prefix of all Windows services ConfigMap names
	NamePrefix = "windows-services-"
	// servicesKey is a required key in the services ConfigMap. The value for this key is a Service object JSON array.
	servicesKey = "services"
	// filesKey is a required key in the services ConfigMap. The value for this key is a FileInfo object JSON array.
	filesKey = "files"
)

var (
	// Name is the full name of the Windows services ConfigMap, detailing the service config for a specific WMCO version
	Name string
	//TODO: Fill in required content as services and files are added to the ConfigMap definition
	// requiredServices is the source of truth for expected service configuration on a Windows Node
	requiredServices = &[]Service{}
	// requiredFiles is the source of truth for files that are expected to exist on a Windows Node
	requiredFiles = &[]FileInfo{}
)

// init runs once, initializing global variables
func init() {
	Name = getName()
}

// NodeCmdArg describes a Windows command variable and how its value can be populated
type NodeCmdArg struct {
	// Name is the variable name as it appears in commands
	Name string `json:"name"`
	// NodeObjectJsonPath is the JSON path of a field within an instance's Node object.
	// The value of this field is the value of the variable
	NodeObjectJsonPath string `json:"nodeObjectJsonPath"`
}

// PowershellCmdArg describes a PowerShell variable and how its value can be populated
type PowershellCmdArg struct {
	// Name is the variable name as it appears in commands
	Name string `json:"name"`
	// Path is the location of a PowerShell script whose output is the value of the variable
	Path string `json:"path"`
}

// Service represents the configuration spec of a Windows service
type Service struct {
	// Name is the name of the Windows service
	Name string `json:"name"`
	// Command is the command that will launch the Windows service. This could potentially include strings whose values
	// will be derived from NodeVariablesInCommand and PowershellVariablesInCommand.
	// Before the command is run on an instance, all node and PowerShell variables will be replaced by their values
	Command string `json:"path"`
	// NodeVariablesInCommand holds all variables in the service command whose values are sourced from a node object
	NodeVariablesInCommand []NodeCmdArg `json:"nodeVariablesInCommand,omitempty"`
	// PowershellVariablesInCommand holds all variables in the command whose values are sourced from a PowerShell script
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
	// Checksum is the checksum of the file specified at Path. It is used to validate that a file has not been changed
	Checksum string `json:"checksum"`
}

// data represents the Data field of a `windows-services` ConfigMap resource, which is all the required information to
// configure a Windows instance as a Node
type data struct {
	// Services contains information required to start all required Windows services with proper arguments and order
	Services []Service `json:"services"`
	// Files contains the path and checksum of all the files copied to a Windows VM by WMCO
	Files []FileInfo `json:"files"`
}

// newData returns a new 'data' object with the given services and files.
func newData(services *[]Service, files *[]FileInfo) *data {
	cmData := &data{*services, *files}
	return cmData
}

// Generate creates an immutable service ConfigMap which provides WICD with the specifications
// for each Windows service that must be created on a Windows instance.
func Generate(name, namespace string) (*core.ConfigMap, error) {
	immutable := true
	servicesConfigMap := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Immutable: &immutable,
		Data:      make(map[string]string),
	}

	cmData := newData(requiredServices, requiredFiles)

	jsonServices, err := json.Marshal(cmData.Services)
	if err != nil {
		return nil, err
	}
	servicesConfigMap.Data[servicesKey] = string(jsonServices)

	jsonFiles, err := json.Marshal(cmData.Files)
	if err != nil {
		return nil, err
	}
	servicesConfigMap.Data[filesKey] = string(jsonFiles)

	return servicesConfigMap, nil
}

// getName returns the name of the ConfigMap, using the following naming convention:
// windows-services-<WMCOFullVersion>
func getName() string {
	return fmt.Sprintf("%s%s", NamePrefix, version.Get())
}
