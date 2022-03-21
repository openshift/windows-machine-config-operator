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

// servicesConfigMapName returns the ConfigMap with the naming scheme:
// windows-services-<WMCOFullVersion>
func servicesConfigMapName() string {
	return fmt.Sprintf("windows-services-%s", version.Get())
}
