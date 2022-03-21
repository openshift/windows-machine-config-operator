package servicescm

import (
	"fmt"

	"github.com/openshift/windows-machine-config-operator/version"
)

// NamePrefix is the prefix of all Windows services ConfigMap names
const NamePrefix = "windows-services-"

// Name is the full name of the Windows services ConfigMap, detailing the service config for a specific WMCO version
var Name string

// init runs once, initializing global variables
func init() {
	Name = getName()
}

// getName returns the name of the ConfigMap, using the following naming convention:
// windows-services-<WMCOFullVersion>
func getName() string {
	return fmt.Sprintf("%s%s", NamePrefix, version.Get())
}
