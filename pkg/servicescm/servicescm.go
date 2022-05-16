package servicescm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

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

// newData returns a new 'data' object with the given services and files. Validates given object contents on creation.
func newData(services *[]Service, files *[]FileInfo) (*data, error) {
	cmData := &data{*services, *files}
	if err := cmData.validate(); err != nil {
		return nil, errors.Wrap(err, "unable to create services ConfigMap data object")
	}
	return cmData, nil
}

// Generate returns the specifications for the Windows Service ConfigMap expected by WMCO
func Generate(name, namespace string) (*core.ConfigMap, error) {
	return GenerateWithData(name, namespace, requiredServices, requiredFiles)
}

// GenerateWithData creates an immutable service ConfigMap which provides WICD with the specifications
// for each Windows service that must be created on a Windows instance.
func GenerateWithData(name, namespace string, services *[]Service, files *[]FileInfo) (*core.ConfigMap, error) {
	immutable := true
	servicesConfigMap := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Immutable: &immutable,
		Data:      make(map[string]string),
	}

	cmData, err := newData(services, files)
	if err != nil {
		return nil, err
	}

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

// Parse converts ConfigMap data into the objects representing a Windows services ConfigMap schema
// Returns error if the given data is invalid in structure
func Parse(dataFromCM map[string]string) (*data, error) {
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

	return newData(services, files)
}

// MarshallAndEncode returns a base64 encoded JSON representation of the data
// TODO: Remove this when the WICD controller has permissions to watch ConfigMaps
func (cmData *data) MarshallAndEncode() (string, error) {
	marshalled, err := json.Marshal(cmData)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(marshalled), nil
}

// DecodeAndUnmarshall takes in a JSON marshalled base64 encoded string and returns the decoded data
// TODO: Remove this when the WICD controller has permissions to watch ConfigMaps
func DecodeAndUnmarshall(base64Encoded string) (*data, error) {
	marshalledData, err := base64.StdEncoding.DecodeString(base64Encoded)
	if err != nil {
		return nil, errors.Wrap(err, "unable to decode ConfigMap data")
	}
	data := data{}
	err = json.Unmarshal(marshalledData, &data)
	return &data, err
}

// validate ensures the given object represents a valid services ConfigMap, ensuring bootstrap services are defined to
// always start before controller services.
func (cmData *data) validate() error {
	if err := validateDependencies(cmData.Services); err != nil {
		return err
	}
	return validatePriorities(cmData.Services)
}

// ValidateRequiredContent ensures that the given slices are comprised of all the required services/files and only these
func (cmData *data) ValidateRequiredContent() error {
	// Validate services
	if len(cmData.Services) != len(*requiredServices) {
		return errors.New("Unexpected number of services")
	}
	for _, requiredSvc := range *requiredServices {
		if !requiredSvc.isPresentAndCorrect(cmData.Services) {
			return errors.Errorf("Required service %s is not present with expected configuration", requiredSvc.Name)
		}
	}
	// Validate files
	if len(cmData.Files) != len(*requiredFiles) {
		return errors.New("Unexpected number of files")
	}
	for _, requiredFile := range *requiredFiles {
		if !requiredFile.isPresentAndCorrect(cmData.Files) {
			return errors.Errorf("Required file %s is not present as expected", requiredFile.Path)
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

// validateDependencies ensures that no bootstrap service depends on a non-bootstrap service
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
		if bootstrapSvc.hasDependency(nonBootstrapServices) {
			return errors.Errorf("bootstrap service %s cannot depend on non-bootstrap service", bootstrapSvc.Name)
		}
	}
	return nil
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

// validatePriorities ensures that each service that has the bootstrap flag set as true has a higher priority than all
// non-bootstrap services. There should be no overlap in the priorities of bootstrap services and controller services.
func validatePriorities(services []Service) error {
	// sort services in ascending priority, bootstrap services towards the front of slice
	sort.Slice(services, func(i, j int) bool {
		return services[i].Priority < services[j].Priority
	})

	// ensure no bootstrap service appears after a controller service in the ordered list
	nonBootstrapSeen := false
	lastBootstrapPriority := uint(0)
	for _, svc := range services {
		if svc.Bootstrap {
			if nonBootstrapSeen {
				return errors.Errorf("bootstrap service %s priority must be higher than all controller services",
					svc.Name)
			}
			lastBootstrapPriority = svc.Priority
		} else {
			// corner case if two adjacent bootstrap and controller services have the same priority
			if svc.Priority == lastBootstrapPriority {
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
