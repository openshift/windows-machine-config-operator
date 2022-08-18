package providers

import (
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/pkg/errors"

	oc "github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	awsProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/aws"
	azureProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/azure"
	noneProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/none"
	vSphereProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/vsphere"
)

type CloudProvider interface {
	GenerateMachineSet(bool, int32) (*mapi.MachineSet, error)
	// GetType returns the cloud provider type ex: AWS, Azure etc
	GetType() config.PlatformType
}

// NewCloudProvider returns a CloudProvider interface or an error
func NewCloudProvider() (CloudProvider, error) {
	openshift, err := oc.GetOpenShift()
	if err != nil {
		return nil, errors.Wrap(err, "Getting OpenShift client failed")
	}
	infra, err := openshift.GetInfrastructure()
	if err != nil {
		return nil, errors.Wrap(err, "Getting cloud provider type")
	}
	switch provider := infra.Status.PlatformStatus.Type; provider {
	case config.AWSPlatformType:
		return awsProvider.New(openshift, &infra.Status)
	case config.AzurePlatformType:
		return azureProvider.New(openshift, &infra.Status), nil
	case config.VSpherePlatformType:
		return vSphereProvider.New(openshift, &infra.Status)
	case config.NonePlatformType:
		return noneProvider.New(openshift)
	default:
		return nil, fmt.Errorf("the '%v' cloud provider is not supported", provider)
	}
}
