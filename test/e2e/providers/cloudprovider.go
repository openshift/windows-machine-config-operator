package providers

import (
	"fmt"

	"github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	oc "github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	awsProvider "github.com/openshift/windows-machine-config-operator/test/e2e/providers/aws"
	"github.com/pkg/errors"
)

type CloudProvider interface {
	GenerateMachineSet(bool, int32) (*mapi.MachineSet, error)
}

func NewCloudProvider(sshKeyPair string) (CloudProvider, error) {
	openshift, err := oc.GetOpenShift()
	if err != nil {
		return nil, errors.Wrap(err, "Getting OpenShift client failed")
	}
	cloudProvider, err := openshift.GetCloudProvider()
	if err != nil {
		return nil, errors.Wrap(err, "Getting cloud provider type")
	}
	switch provider := cloudProvider.Type; provider {
	case v1.AWSPlatformType:
		// 	Setup the AWS cloud provider in the same region where the cluster is running
		return awsProvider.SetupAWSCloudProvider(cloudProvider.AWS.Region, sshKeyPair)
	default:
		return nil, fmt.Errorf("the '%v' cloud provider is not supported", provider)
	}
}
