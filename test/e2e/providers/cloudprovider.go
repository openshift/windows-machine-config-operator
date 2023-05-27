package providers

import (
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	core "k8s.io/api/core/v1"
	client "k8s.io/client-go/kubernetes"

	oc "github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	awsProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/aws"
	azureProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/azure"
	gcpProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/gcp"
	noneProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/none"
	vSphereProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/vsphere"
	"github.com/openshift/windows-machine-config-operator/test/e2e/windows"
)

type CloudProvider interface {
	// GenerateMachineSet generates provider specific Windows Server version MachineSet with the given replicas and
	// the ignore label if the boolean is set
	GenerateMachineSet(bool, int32, windows.ServerVersion) (*mapi.MachineSet, error)
	// GetType returns the cloud provider type ex: AWS, Azure etc
	GetType() config.PlatformType
	// StorageSupport indicates if we support Windows storage on this provider
	StorageSupport() bool
	// CreatePVC creates a new PersistentVolumeClaim that can be used by a workload. The PVC will be created with
	// the given client, in the given namespace.
	CreatePVC(client.Interface, string) (*core.PersistentVolumeClaim, error)
}

// NewCloudProvider returns a CloudProvider interface or an error
func NewCloudProvider() (CloudProvider, error) {
	openshift, err := oc.GetOpenShift()
	if err != nil {
		return nil, fmt.Errorf("getting OpenShift client failed: %w", err)
	}
	infra, err := openshift.GetInfrastructure()
	if err != nil {
		return nil, fmt.Errorf("getting cloud provider type: %w", err)
	}
	switch provider := infra.Status.PlatformStatus.Type; provider {
	case config.AWSPlatformType:
		return awsProvider.New(openshift, &infra.Status)
	case config.AzurePlatformType:
		return azureProvider.New(openshift, &infra.Status), nil
	case config.GCPPlatformType:
		return gcpProvider.New(openshift, &infra.Status), nil
	case config.VSpherePlatformType:
		return vSphereProvider.New(openshift, &infra.Status)
	case config.NonePlatformType:
		return noneProvider.New(openshift)
	default:
		return nil, fmt.Errorf("the '%v' cloud provider is not supported", provider)
	}
}
