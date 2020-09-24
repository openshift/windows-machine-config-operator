package aws

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	awssession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	awsprovider "sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsprovider/v1beta1"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

const (
	infraIDTagKeyPrefix = "kubernetes.io/cluster/"
	infraIDTagValue     = "owned"
	windowsLabel        = "machine.openshift.io/os-id"
	// instanceType is the AWS specific instance type to create the VM with
	instanceType = "m5a.large"
)

type awsProvider struct {
	// imageID is the AMI image-id to be used for creating Virtual Machine
	imageID string
	// instanceType is the flavor of VM to be used
	instanceType string
	// A client for IAM.
	iam *iam.IAM
	// A client for EC2. to query Windows AMI images
	ec2 ec2iface.EC2API
	// openShiftClient is the client of the existing OpenShift cluster.
	openShiftClient *clusterinfo.OpenShift
	// region in which the Machine needs to be created
	region string
}

// newSession uses AWS credentials to create and returns a session for interacting with EC2.
func newSession(credentialPath, credentialAccountID, region string) (*awssession.Session, error) {
	if _, err := os.Stat(credentialPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to find AWS credentials from path '%v'", credentialPath)
	}
	return awssession.NewSession(&aws.Config{
		Credentials: credentials.NewSharedCredentials(credentialPath, credentialAccountID),
		Region:      aws.String(region),
	})
}

// newAWSProvider returns the AWS implementations of the Cloud interface with AWS session in the same region as OpenShift Cluster.
// credentialPath is the file path the AWS credentials file.
// credentialAccountID is the account name the user uses to create VM instance.
// The credentialAccountID should exist in the AWS credentials file pointing at one specific credential.
func newAWSProvider(openShiftClient *clusterinfo.OpenShift, credentialPath,
	credentialAccountID, instanceType, region string, hasCustomVXLANPort bool) (*awsProvider, error) {
	session, err := newSession(credentialPath, credentialAccountID, region)
	if err != nil {
		return nil, fmt.Errorf("could not create new AWS session: %v", err)
	}

	ec2Client := ec2.New(session, aws.NewConfig())

	iamClient := iam.New(session, aws.NewConfig())

	imageID, err := getLatestWindowsAMI(ec2Client, hasCustomVXLANPort)
	if err != nil {
		return nil, fmt.Errorf("unable to get latest Windows AMI: %v", err)
	}
	return &awsProvider{imageID, instanceType,
		iamClient,
		ec2Client,
		openShiftClient,
		region,
	}, nil
}

// SetupAWSCloudProvider creates AWS provider using the give OpenShift client
// This is the first step of the e2e test and fails the test upon error.
func SetupAWSCloudProvider(region string, hasCustomVXLANPort bool) (*awsProvider, error) {
	oc, err := clusterinfo.GetOpenShift()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize OpenShift client with error: %v", err)
	}
	// awsCredentials is set by OpenShift CI
	awsCredentials := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if len(awsCredentials) == 0 {
		return nil, fmt.Errorf("AWS_SHARED_CREDENTIALS_FILE env var is empty")
	}
	awsProvider, err := newAWSProvider(oc, awsCredentials, "default", instanceType, region, hasCustomVXLANPort)
	if err != nil {
		return nil, fmt.Errorf("error obtaining aws interface object: %v", err)
	}

	return awsProvider, nil
}

// getInfraID returns the infrastructure ID associated with the OpenShift cluster. This is public for
// testing purposes as of now.
func (a *awsProvider) getInfraID() (string, error) {
	infraID, err := a.openShiftClient.GetInfrastructureID()
	if err != nil {
		return "", fmt.Errorf("erroring getting OpenShift infrastructure ID associated with the cluster")
	}
	return infraID, nil
}

// getLatestWindowsAMI returns the imageid of the latest released "Windows Server with Containers" image
func getLatestWindowsAMI(ec2Client *ec2.EC2, hasCustomVXLANPort bool) (string, error) {
	// Have to create these variables, as the below functions require pointers to them
	windowsAMIOwner := "amazon"
	windowsAMIFilterName := "name"
	windowsAMIFilterValue := ""
	// This filter will grab all ami's that match the exact name. The '?' indicate any character will match.
	// The ami's will have the name format: Windows_Server-2019-English-Full-ContainersLatest-2020.01.15
	// so the question marks will match the date of creation
	// The image obtained by using windowsAMIFilterValue is compatible with  the test container image -
	// "mcr.microsoft.com/powershell:lts-nanoserver-1909" or "mcr.microsoft.com/powershell:lts-nanoserver-1809".
	// If the windowsAMIFilterValue changes, the test container image also needs to be changed.
	// if hasCustomVXLANPort is set use 1909 image as it has the custom VXLAN port changes, if not use Windows Server 2019 image
	if hasCustomVXLANPort {
		windowsAMIFilterValue = "Windows_Server-1909-English-Core-ContainersLatest-????.??.??"
	} else {
		windowsAMIFilterValue = "Windows_Server-2019-English-Full-ContainersLatest-????.??.??"
	}
	searchFilter := ec2.Filter{Name: &windowsAMIFilterName, Values: []*string{&windowsAMIFilterValue}}

	describedImages, err := ec2Client.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{&searchFilter},
		Owners:  []*string{&windowsAMIOwner},
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

// getSubnet tries to find a subnet under the VPC and returns subnet or an error.
// These subnets belongs to the OpenShift cluster.
func (a *awsProvider) getSubnet(infraID string) (*ec2.Subnet, error) {
	vpc, err := a.getVPCByInfrastructure(infraID)
	if err != nil {
		return nil, fmt.Errorf("unable to get the VPC %v", err)
	}
	// search subnet by the vpcid owned by the vpcID
	subnets, err := a.ec2.DescribeSubnets(&ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{vpc.VpcId},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	// Get the instance offerings that support Windows instances
	scope := "Availability Zone"
	productDescription := "Windows"
	f := false
	offerings, err := a.ec2.DescribeReservedInstancesOfferings(&ec2.DescribeReservedInstancesOfferingsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("scope"),
				Values: []*string{&scope},
			},
		},
		IncludeMarketplace: &f,
		InstanceType:       &a.instanceType,
		ProductDescription: &productDescription,
	})
	if err != nil {
		return nil, fmt.Errorf("error checking instance offerings of %s: %v", a.instanceType, err)
	}
	if offerings.ReservedInstancesOfferings == nil {
		return nil, fmt.Errorf("no instance offerings returned for %s", a.instanceType)
	}

	// Finding required subnet within the vpc.
	foundSubnet := false
	requiredSubnet := "-private-"

	for _, subnet := range subnets.Subnets {
		for _, tag := range subnet.Tags {
			// TODO: find required subnet by checking igw gateway in routing.
			if *tag.Key == "Name" && strings.Contains(*tag.Value, infraID+requiredSubnet) {
				foundSubnet = true
				// Ensure that the instance type we want is supported in the zone that the subnet is in
				for _, instanceOffering := range offerings.ReservedInstancesOfferings {
					if instanceOffering.AvailabilityZone == nil {
						continue
					}
					if *instanceOffering.AvailabilityZone == *subnet.AvailabilityZone {
						return subnet, nil
					}
				}
			}
		}
	}

	err = fmt.Errorf("could not find the required subnet in VPC: %v", *vpc.VpcId)
	if !foundSubnet {
		err = fmt.Errorf("could not find the required subnet in a zone that supports %s instance type",
			a.instanceType)
	}
	return nil, err
}

// getClusterWorkerSGID gets worker security group id from the existing cluster or returns an error.
// This function is exposed for testing purpose.
func (a *awsProvider) getClusterWorkerSGID(infraID string) (string, error) {
	sg, err := a.ec2.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: aws.StringSlice([]string{fmt.Sprintf("%s-worker-sg", infraID)}),
			},
			{
				Name:   aws.String("tag:" + infraIDTagKeyPrefix + infraID),
				Values: aws.StringSlice([]string{infraIDTagValue}),
			},
		},
	})
	if err != nil {
		return "", err
	}
	if sg == nil || len(sg.SecurityGroups) < 1 {
		return "", fmt.Errorf("no security group is found for the cluster worker nodes")
	}
	return *sg.SecurityGroups[0].GroupId, nil
}

// GetVPCByInfrastructure finds the VPC of an infrastructure and returns the VPC struct or an error.
func (a *awsProvider) getVPCByInfrastructure(infraID string) (*ec2.Vpc, error) {
	res, err := a.ec2.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:" + infraIDTagKeyPrefix + infraID),
				Values: aws.StringSlice([]string{infraIDTagValue}),
			},
			{
				Name:   aws.String("state"),
				Values: aws.StringSlice([]string{"available"}),
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(res.Vpcs) < 1 {
		return nil, fmt.Errorf("failed to find the VPC of the infrastructure")
	} else if len(res.Vpcs) > 1 {
		log.Printf("more than one VPC is found, using %s", *res.Vpcs[0].VpcId)
	}
	return res.Vpcs[0], nil
}

// getIAMWorkerRole gets worker IAM information from the existing cluster including IAM arn or an error.
// This function is exposed for testing purpose.
func (a *awsProvider) getIAMWorkerRole(infraID string) (*ec2.IamInstanceProfileSpecification, error) {
	iamspc, err := a.iam.GetInstanceProfile(&iam.GetInstanceProfileInput{
		InstanceProfileName: aws.String(fmt.Sprintf("%s-worker-profile", infraID)),
	})
	if err != nil {
		return nil, err
	}
	return &ec2.IamInstanceProfileSpecification{
		Arn: iamspc.InstanceProfile.Arn,
		// The ARN itself is not good enough in the MachineSet spec. we need the id to map the worker
		// IAM profile in MachineSet spec
		Name: iamspc.InstanceProfile.InstanceProfileName,
	}, nil
}

// GenerateMachineSet generates the machineset object which is aws provider specific
func (a *awsProvider) GenerateMachineSet(withWindowsLabel bool, replicas int32) (*mapi.MachineSet, error) {
	clusterName, err := a.getInfraID()
	if err != nil {
		return nil, fmt.Errorf("unable to get infrastructure id %v", err)
	}

	instanceProfile, err := a.getIAMWorkerRole(clusterName)
	if err != nil {
		return nil, fmt.Errorf("unable to get instance profile %v", err)
	}

	sgID, err := a.getClusterWorkerSGID(clusterName)
	if err != nil {
		return nil, fmt.Errorf("unable to get security group id: %v", err)
	}

	subnet, err := a.getSubnet(clusterName)
	if err != nil {
		return nil, fmt.Errorf("unable to get subnet: %v", err)
	}
	machineSetName := "e2e-windows-machineset-"
	matchLabels := map[string]string{
		"machine.openshift.io/cluster-api-cluster": clusterName,
	}

	if withWindowsLabel {
		matchLabels[windowsLabel] = "Windows"
		machineSetName = machineSetName + "with-windows-label-"
	}
	matchLabels["machine.openshift.io/cluster-api-machineset"] = machineSetName + *subnet.AvailabilityZone

	machineLabels := map[string]string{
		"machine.openshift.io/cluster-api-machine-role": "worker",
		"machine.openshift.io/cluster-api-machine-type": "worker",
	}
	// append matchlabels to machinelabels
	for k, v := range matchLabels {
		machineLabels[k] = v
	}

	providerSpec := &awsprovider.AWSMachineProviderConfig{
		AMI: awsprovider.AWSResourceReference{
			ID: &a.imageID,
		},
		InstanceType: a.instanceType,
		IAMInstanceProfile: &awsprovider.AWSResourceReference{
			ID: instanceProfile.Name,
		},
		CredentialsSecret: &core.LocalObjectReference{
			Name: "aws-cloud-credentials",
		},
		SecurityGroups: []awsprovider.AWSResourceReference{
			{
				ID: &sgID,
			},
		},
		Subnet: awsprovider.AWSResourceReference{
			ID: subnet.SubnetId,
		},
		// query placement
		Placement: awsprovider.Placement{
			a.region,
			*subnet.AvailabilityZone,
		},
		UserDataSecret: &core.LocalObjectReference{Name: "windows-user-data"},
	}

	rawBytes, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, err
	}

	// Set up the test machineSet
	machineSet := &mapi.MachineSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      machineSetName + rand.String(4),
			Namespace: "openshift-machine-api",
			Labels: map[string]string{
				mapi.MachineClusterIDLabel: clusterName,
			},
		},
		Spec: mapi.MachineSetSpec{
			Selector: meta.LabelSelector{
				MatchLabels: matchLabels,
			},
			Replicas: &replicas,
			Template: mapi.MachineTemplateSpec{
				ObjectMeta: mapi.ObjectMeta{Labels: machineLabels},
				Spec: mapi.MachineSpec{
					ObjectMeta: mapi.ObjectMeta{Labels: map[string]string{"node-role.kubernetes.io/worker": ""}},
					ProviderSpec: mapi.ProviderSpec{
						Value: &runtime.RawExtension{
							Raw: rawBytes,
						},
					},
				},
			},
		},
	}
	return machineSet, nil
}
