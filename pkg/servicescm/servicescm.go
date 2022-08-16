package servicescm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/version"
	"github.com/pkg/errors"
)

const (
	// NamePrefix is the prefix of all Windows services ConfigMap names
	NamePrefix = "windows-services-"
	// CMDataAnnotation is a Node annotation whose value is the base64 encoded data of current version's service CM
	// TODO: Remove this when the WICD controller has permissions to watch ConfigMaps
	CMDataAnnotation = "windowsmachineconfig.openshift.io/cmdata"
	// servicesKey is a required key in the services ConfigMap. The value for this key is a Service object JSON array.
	servicesKey = "services"
	// filesKey is a required key in the services ConfigMap. The value for this key is a FileInfo object JSON array.
	filesKey = "files"
)

var (
	// Name is the full name of the Windows services ConfigMap, detailing the service config for a specific WMCO version
	Name string
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

// Data represents the Data field of a `windows-services` ConfigMap resource, which is all the required information to
// configure a Windows instance as a Node
type Data struct {
	// Services contains information required to start all required Windows services with proper arguments and order
	Services []Service `json:"services"`
	// Files contains the path and checksum of all the files copied to a Windows VM by WMCO
	Files []FileInfo `json:"files"`
}

// NewData returns a new 'Data' object with the given services and files. Validates given object contents on creation.
func NewData(services *[]Service, files *[]FileInfo) (*Data, error) {
	cmData := &Data{*services, *files}
	if err := cmData.validate(); err != nil {
		return nil, errors.Wrap(err, "unable to create services ConfigMap data object")
	}
	return cmData, nil
}

// List returns a list of all windows-services ConfigMaps in the given namespace
func List(c client.Client, ctx context.Context, namespace string) ([]core.ConfigMap, error) {
	watchNamespaceCMs := &core.ConfigMapList{}
	if err := c.List(ctx, watchNamespaceCMs, &client.ListOptions{Namespace: namespace}); err != nil {
		return nil, err
	}
	servicesConfigMaps := []core.ConfigMap{}
	for _, cm := range watchNamespaceCMs.Items {
		if strings.HasPrefix(cm.Name, NamePrefix) {
			servicesConfigMaps = append(servicesConfigMaps, cm)
		}
	}
	return servicesConfigMaps, nil
}

// Generate creates an immutable service ConfigMap which provides WICD with the specifications
// for each Windows service that must be created on a Windows instance.
func Generate(name, namespace string, data *Data) (*core.ConfigMap, error) {
	immutable := true
	servicesConfigMap := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Immutable: &immutable,
		Data:      make(map[string]string),
	}

	jsonServices, err := json.Marshal(data.Services)
	if err != nil {
		return nil, err
	}
	servicesConfigMap.Data[servicesKey] = string(jsonServices)

	jsonFiles, err := json.Marshal(data.Files)
	if err != nil {
		return nil, err
	}
	servicesConfigMap.Data[filesKey] = string(jsonFiles)

	return servicesConfigMap, nil
}

// Parse converts ConfigMap data into the objects representing a Windows services ConfigMap schema
// Returns error if the given data is invalid in structure
func Parse(dataFromCM map[string]string) (*Data, error) {
	if len(dataFromCM) != 2 {
		return nil, errors.New("services ConfigMap should have exactly 2 keys")
	}

	value, ok := dataFromCM[servicesKey]
	if !ok {
		return nil, errors.Errorf("expected key %s does not exist", servicesKey)
	}
	services := &[]Service{}
	if err := json.Unmarshal([]byte(value), services); err != nil {
		return nil, err
	}

	value, ok = dataFromCM[filesKey]
	if !ok {
		return nil, errors.Errorf("expected key %s does not exist", filesKey)
	}
	files := &[]FileInfo{}
	if err := json.Unmarshal([]byte(value), files); err != nil {
		return nil, err
	}

	return NewData(services, files)
}

// GetBootstrapServices filters the cmData object's services list and returns only the bootstrap services
func (cmData *Data) GetBootstrapServices() []Service {
	bootstrapSvcs := []Service{}
	for _, svc := range cmData.Services {
		if !svc.Bootstrap {
			// services are pre-sorted by priority, with all bootstrap services ordered towards the front of the slice
			break
		}
		bootstrapSvcs = append(bootstrapSvcs, svc)
	}
	return bootstrapSvcs
}

// validate ensures the given object represents a valid services ConfigMap, ensuring bootstrap services are defined to
// always start before controller services.
func (cmData *Data) validate() error {
	if err := validateDependencies(cmData.Services); err != nil {
		return err
	}
	return validatePriorities(cmData.Services)
}

// ValidateExpectedContent ensures that the given slices are comprised of all the expected services/files and only these
func (cmData *Data) ValidateExpectedContent(expected *Data) error {
	// Validate services
	if len(cmData.Services) != len(expected.Services) {
		return errors.New("Unexpected number of services")
	}
	for _, expectedSvc := range expected.Services {
		if !expectedSvc.isPresentAndCorrect(cmData.Services) {
			return errors.Errorf("Required service %s is not present with expected configuration", expectedSvc.Name)
		}
	}
	// Validate files
	if len(cmData.Files) != len(expected.Files) {
		return errors.New("Unexpected number of files")
	}
	for _, expectedFile := range expected.Files {
		if !expectedFile.isPresentAndCorrect(cmData.Files) {
			return errors.Errorf("Required file %s is not present as expected", expectedFile.Path)
		}
	}
	return nil
}

// isPresentAndCorrect checks if the required service exists as expected within the given services slice
func (s Service) isPresentAndCorrect(services []Service) bool {
	for _, service := range services {
		if reflect.DeepEqual(s, service) {
			return true
		}
	}
	return false
}

// isPresentAndCorrect checks if the required file exists as expected within the given files slice
// TODO: When we move to go1.18, consolodate this helper with the above Service.isPresentAndCorrect using generics
func (f FileInfo) isPresentAndCorrect(files []FileInfo) bool {
	for _, file := range files {
		if reflect.DeepEqual(f, file) {
			return true
		}
	}
	return false
}

// validateDependencies ensures that no bootstrap service depends on a non-bootstrap service or node object
// and ensures there is no cyclical dependency chain
func validateDependencies(services []Service) error {
	bootstrapServices := []Service{}
	nonBootstrapServices := []Service{}
	for _, svc := range services {
		if svc.Bootstrap {
			bootstrapServices = append(bootstrapServices, svc)
		} else {
			nonBootstrapServices = append(nonBootstrapServices, svc)
		}
	}

	for _, bootstrapSvc := range bootstrapServices {
		if len(bootstrapSvc.NodeVariablesInCommand) > 0 {
			return errors.Errorf("bootstrap service %s cannot require node variables in command", bootstrapSvc.Name)
		}
		if bootstrapSvc.hasDependency(nonBootstrapServices) {
			return errors.Errorf("bootstrap service %s cannot depend on non-bootstrap service", bootstrapSvc.Name)
		}
	}

	return validateCycles(services)
}

// hasDependency checks if a service is dependent on any services in the given slice
func (s *Service) hasDependency(possibleDependencies []Service) bool {
	for _, dependency := range s.Dependencies {
		for _, possibleDependency := range possibleDependencies {
			if dependency == possibleDependency.Name {
				return true
			}
		}
	}
	return false
}

// validateCycles detects cycles between any of the given services by traversing the resulting dependency graph.
// Wrapper for Service.hasCycle to handle disconnected graphs
func validateCycles(services []Service) error {
	// Convert list to map for fast lookup of service object by its name
	servicesMap := make(map[string]*Service)
	for _, svc := range services {
		servicesMap[svc.Name] = &svc
	}

	// state is a map that keeps track of whether a service's dependency chain has been explored for cycles:
	// 1. if a service does not have an entry in the map, it has not been processed yet
	// 2. if a service has an entry in the map with value "true", it is currently being processed
	// 3. if a service has an entry in the map with value "false", it has already been fully processed in the past
	state := make(map[string]bool)
	for _, svc := range services {
		// Check if helper has already been called on this service to prevent duplicate calls
		if _, seen := state[svc.Name]; !seen {
			if svc.hasCycle(servicesMap, state) {
				return errors.Errorf("invalid cyclical chain in %s service's dependencies", svc.Name)
			}
		}
	}
	return nil
}

// hasCycle uses depth-first traversal to check for cycles in the service dependency graph, using s as the source node
func (s *Service) hasCycle(servicesMap map[string]*Service, state map[string]bool) bool {
	// Mark this service as visited and in the current traversal path
	state[s.Name] = true

	for _, dependencyName := range s.Dependencies {
		if inCurrentPath, seen := state[dependencyName]; seen && inCurrentPath {
			// Cycle detected if service that is still being processed is seen again in the same dependency path
			return true
		}
		if _, seen := state[dependencyName]; !seen {
			// Only explore a dependency service if it's also managed by the services ConfigMap. Continue otherwise
			if dependencyService, ok := servicesMap[dependencyName]; ok {
				return dependencyService.hasCycle(servicesMap, state)
			}
		}
	}
	// Backtracking step, remove this service from current traversal path by marking it as fully processed
	state[s.Name] = false
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
	nonBootstrapSeen := false
	lastBootstrapPriority := -1
	for _, svc := range services {
		if svc.Bootstrap {
			if nonBootstrapSeen {
				return errors.Errorf("bootstrap service %s priority must be higher than all controller services",
					svc.Name)
			}
			lastBootstrapPriority = int(svc.Priority)
		} else {
			// corner case if two adjacent bootstrap and controller services have the same priority
			if int(svc.Priority) == lastBootstrapPriority {
				return errors.Errorf("controller service %s priority must not overlap with any bootstrap service",
					svc.Name)
			}
			nonBootstrapSeen = true
		}
	}
	return nil
}

// getName returns the name of the ConfigMap, using the following naming convention:
// windows-services-<WMCOFullVersion>
func getName() string {
	return fmt.Sprintf("%s%s", NamePrefix, version.Get())
}
