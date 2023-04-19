package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/machineset"
)

type Provider struct {
	oc *clusterinfo.OpenShift
	*config.InfrastructureStatus
	// imageID is the AMI which will be used when creating VMs
	imageID string
}

// New returns a new Provider
func New(oc *clusterinfo.OpenShift, infraStatus *config.InfrastructureStatus) (*Provider, error) {
	ami, err := getLatestWindowsAMI(infraStatus.PlatformStatus.AWS.Region)
	if err != nil {
		return nil, errors.Wrap(err, "error choosing AMI")
	}
	return &Provider{
		oc:                   oc,
		InfrastructureStatus: infraStatus,
		imageID:              ami,
	}, nil
}

// newEC2Client returns an EC2 client for the given region
func newEC2Client(region string) (*ec2.EC2, error) {
	credentialPath := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if len(credentialPath) == 0 {
		return nil, fmt.Errorf("AWS_SHARED_CREDENTIALS_FILE env var is empty")
	}
	if _, err := os.Stat(credentialPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to find AWS credentials from path '%v'", credentialPath)
	}

	awsSession, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewSharedCredentials(credentialPath, "default"),
		Region:      aws.String(region),
	})
	if err != nil {
		return nil, errors.Wrap(err, "error initializing aws session")
	}
	return ec2.New(awsSession, aws.NewConfig()), nil
}

// getLatestWindowsAMI returns the ID of the latest released "Windows Server with Containers" AMI
func getLatestWindowsAMI(region string) (string, error) {
	ec2Client, err := newEC2Client(region)
	if err != nil {
		return "", err
	}
	// This filter will grab all ami's that match the exact name. The '?' indicate any character will match.
	// The ami's will have the name format: Windows_Server-2019-English-Full-ContainersLatest-2022.01.19
	// so the question marks will match the date of creation
	// The image obtained by using windowsAMIFilterValue is compatible with the test container image -
	// "mcr.microsoft.com/powershell:lts-nanoserver-1809".
	// If the windowsAMIFilterValue changes, the test container image also needs to be changed.
	windowsAMIFilterValue := "Windows_Server-2019-English-Full-ContainersLatest-????.??.??"
	searchFilter := ec2.Filter{Name: aws.String("name"), Values: []*string{&windowsAMIFilterValue}}

	describedImages, err := ec2Client.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{&searchFilter},
		Owners:  []*string{aws.String("amazon")},
	})
	if err != nil {
		return "", err
	}
	if len(describedImages.Images) < 1 {
		return "", fmt.Errorf("found zero images matching given filter: %v", searchFilter)
	}

	// Find the last created image
	latestImage := describedImages.Images[0]
	latestTime, err := time.Parse(time.RFC3339, *latestImage.CreationDate)
	if err != nil {
		return "", err
	}
	for _, image := range describedImages.Images[1:] {
		newTime, err := time.Parse(time.RFC3339, *image.CreationDate)
		if err != nil {
			return "", err
		}
		if newTime.After(latestTime) {
			latestImage = image
			latestTime = newTime
		}
	}
	return *latestImage.ImageId, nil
}

// GenerateMachineSet generates a Windows MachineSet which is AWS provider specific
func (a *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32) (*mapi.MachineSet, error) {
	listOptions := meta.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-machine-role=worker"}
	machines, err := a.oc.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}
	if len(machines.Items) == 0 {
		return nil, fmt.Errorf("found 0 worker role machines")
	}
	linuxWorkerSpec := &mapi.AWSMachineProviderConfig{}
	err = json.Unmarshal(machines.Items[0].Spec.ProviderSpec.Value.Raw, linuxWorkerSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal raw machine provider spec: %v", err)
	}
	providerSpec := &mapi.AWSMachineProviderConfig{
		AMI:                mapi.AWSResourceReference{ID: &a.imageID},
		InstanceType:       linuxWorkerSpec.InstanceType,
		IAMInstanceProfile: linuxWorkerSpec.IAMInstanceProfile,
		CredentialsSecret:  linuxWorkerSpec.CredentialsSecret,
		SecurityGroups:     linuxWorkerSpec.SecurityGroups,
		Subnet:             linuxWorkerSpec.Subnet,
		Placement:          linuxWorkerSpec.Placement,
		UserDataSecret:     &core.LocalObjectReference{Name: clusterinfo.UserDataSecretName},
	}

	rawBytes, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, err
	}

	return machineset.New(rawBytes, a.InfrastructureName, replicas, withIgnoreLabel, a.InfrastructureName+"-"), nil
}

func (a *Provider) GetType() config.PlatformType {
	return config.AWSPlatformType
}
