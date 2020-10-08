package providers

import (
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/pkg/errors"

	oc "github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	awsProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/aws"
	azureProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/azure"
)

type CloudProvider interface {
	GenerateMachineSet(bool, int32) (*mapi.MachineSet, error)
}

// NewCloudProvider returns a CloudProvider interface or an error
func NewCloudProvider(hasCustomVXLANPort bool) (CloudProvider, error) {
	openshift, err := oc.GetOpenShift()
	if err != nil {
		return nil, errors.Wrap(err, "Getting OpenShift client failed")
	}
	platformStatus, err := openshift.GetCloudProvider()
	if err != nil {
		return nil, errors.Wrap(err, "Getting cloud provider type")
	}
	switch provider := platformStatus.Type; provider {
	case config.AWSPlatformType:
		// 	Setup the AWS cloud provider in the same region where the cluster is running
		return awsProvider.SetupAWSCloudProvider(platformStatus.AWS.Region, hasCustomVXLANPort)
	case config.AzurePlatformType:
		return azureProvider.New(openshift, hasCustomVXLANPort)
	default:
		return nil, fmt.Errorf("the '%v' cloud provider is not supported", provider)
	}
}
