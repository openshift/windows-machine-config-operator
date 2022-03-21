package servicescm

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pkg/errors"

	"github.com/openshift/windows-machine-config-operator/version"
)

// ServicesConfigMapName is the name of the ConfigMap detailing service configuration for a specific WMCO version
var ServicesConfigMapName string

// init runs once, initializing global variables
func init() {
	ServicesConfigMapName = servicesConfigMapName()
}

const (
	// servicesKey is a required key in the services ConfigMap. The value for this key is a Service object JSON array.
	servicesKey = "services"
	// filesKey is a required key in the services ConfigMap. The value for this key is a FileInfo object JSON array.
	filesKey = "files"
)

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

// servicesCMData represents the Data field of a `windows-services` ConfigMap resource
type servicesCMData struct {
	// Services contains all data required to configure the required Windows services to configure an instance as a Node
	Services []Service `json:"services"`
	// Files contains the path and checksum of all the files copied to a Windows VM by WMCO
	Files []FileInfo `json:"files"`
}

// NewServicesConfigMapData returns a new servicesCMData. Validates given object contents on creation.
func NewServicesConfigMapData(services *[]Service, files *[]FileInfo) (*servicesCMData, error) {
	cmData := &servicesCMData{*services, *files}
	if err := cmData.validate(); err != nil {
		return nil, errors.Wrap(err, "unable to create services ConfigMap data object")
	}
	return cmData, nil
}

// Parse converts ConfigMap data into the objects representing a Windows services ConfigMap schema
// Returns error if the given data is invalid in structure or contents.
func Parse(data map[string]string) (*servicesCMData, error) {
	if len(data) != 2 {
		return nil, errors.New("services ConfigMap should have exactly 2 keys")
	}

	services := &[]Service{}
	files := &[]FileInfo{}

	value, ok := data[servicesKey]
	if !ok {
		return nil, errors.Errorf("expected `%s` key to exist", servicesKey)
	}
	if err := json.Unmarshal([]byte(value), services); err != nil {
		return nil, err
	}

	value, ok = data[filesKey]
	if !ok {
		return nil, errors.Errorf("expected `%s` key to exist", filesKey)
	}
	if err := json.Unmarshal([]byte(value), files); err != nil {
		return nil, err
	}

	return NewServicesConfigMapData(services, files)
}

// validate ensures the given object represents a valid services ConfigMap.
// The validation ensures bootstrap services always start before controller services according to WICD's expected schema
func (cmData *servicesCMData) validate() error {
	if err := validateDependencies(cmData.Services); err != nil {
		return err
	}
	return validatePriorities(cmData.Services)
}

// validateDependencies ensures that no bootstrap service depends on a non-bootstrap service
func validateDependencies(services []Service) error {
	boostrapServices := []Service{}
	nonBoostrapServices := []Service{}
	for _, svc := range services {
		if svc.Bootstrap {
			boostrapServices = append(boostrapServices, svc)
		} else {
			nonBoostrapServices = append(nonBoostrapServices, svc)
		}
	}

	for _, bootstrapSvc := range boostrapServices {
		if hasDependency(bootstrapSvc, nonBoostrapServices) {
			return errors.Errorf("bootstrap service %s cannot depend on non-boostrap service", bootstrapSvc.Name)
		}
	}
	return nil
}

// hasDependency checks if a service is dependent on any services in the given slice
func hasDependency(s Service, possibleDependencies []Service) bool {
	for _, dependency := range s.Dependencies {
		for _, possibleDependency := range possibleDependencies {
			if dependency == possibleDependency.Name {
				return true
			}
		}
	}
	return false
}

// validatePriorities ensures that each service that has the bootstrap flag set as true has a higher priority than all
// non-bootstrap services. There should be no overlap in the priorities of bootstrap services and controller services.
func validatePriorities(services []Service) error {
	// sort services in ascending priority, bootstrap services towards the front of slice
	sort.Slice(services, func(i, j int) bool {
		return services[i].Priority < services[j].Priority
	})

	// ensure no bootstrap service appears after a controller service in the ordered list
	nonBoostrapSeen := false
	lastBoostrapPriority := uint(0)
	for _, svc := range services {
		if svc.Bootstrap {
			if nonBoostrapSeen {
				return errors.Errorf("bootstrap service %s priority must be higher than all controller services",
					svc.Name)
			}
			lastBoostrapPriority = svc.Priority
		} else {
			// corner case if two adjacent bootstrap and controller services have the same priority
			if svc.Priority == lastBoostrapPriority {
				return errors.Errorf("controller service %s priority must not overlap with any bootstrap service",
					svc.Name)
			}
			nonBoostrapSeen = true
		}
	}
	return nil
}

// servicesConfigMapName returns the ConfigMap with the naming scheme:
// windows-services-<WMCOFullVersion>
func servicesConfigMapName() string {
	return fmt.Sprintf("windows-services-%s", version.Get())
}
