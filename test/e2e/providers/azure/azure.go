package azure

import (
	"context"
	"encoding/json"
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	azureprovider "sigs.k8s.io/cluster-api-provider-azure/pkg/apis/azureprovider/v1beta1"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

const (
	defaultCredentialsSecretName = "azure-cloud-credentials"
	defaultImageOffer            = "WindowsServer"
	defaultImagePublisher        = "MicrosoftWindowsServer"
	defaultImageSKU              = "2019-Datacenter-with-Containers"
	defaultImageVersion          = "latest"
	defaultOSDiskSizeGB          = 128
	defaultStorageAccountType    = "Premium_LRS"
	// The default vm size set by machine-api-operator yields
	// "unknown instance type: Standard_D4s_V3" on dev cluster instances.
	// Use the instance type the other worker machines use.
	defaultVMSize = "Standard_D2s_v3"
)

// Provider is a provider struct for testing Azure
type Provider struct {
	oc     *clusterinfo.OpenShift
	vmSize string
}

// New returns a new Azure provider struct with the give client set and ssh key pair
func New(clientset *clusterinfo.OpenShift, hasCustomVXLANPort bool) (*Provider, error) {
	if hasCustomVXLANPort == true {
		return nil, fmt.Errorf("custom VXLAN port is not supported on current Azure image")
	}

	return &Provider{
		oc:     clientset,
		vmSize: defaultVMSize,
	}, nil
}

// newAzureMachineProviderSpec returns an AzureMachineProviderSpec generated from the inputs, or an error
func newAzureMachineProviderSpec(clusterID string, status *config.PlatformStatus, location, zone, vmSize string) (*azureprovider.AzureMachineProviderSpec, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("clusterID is empty")
	}
	if status == nil || status == (&config.PlatformStatus{}) {
		return nil, fmt.Errorf("platform status is nil")
	}
	if status.Azure == nil || status.Azure == (&config.AzurePlatformStatus{}) {
		return nil, fmt.Errorf("azure platform status is nil")
	}
	if status.Azure.NetworkResourceGroupName == "" {
		return nil, fmt.Errorf("azure network resource group name is empty")
	}
	rg := status.Azure.ResourceGroupName
	netrg := status.Azure.NetworkResourceGroupName

	return &azureprovider.AzureMachineProviderSpec{
		TypeMeta: meta.TypeMeta{
			APIVersion: "azureproviderconfig.openshift.io/v1beta1",
			Kind:       "AzureMachineProviderSpec",
		},
		UserDataSecret: &core.SecretReference{
			Name:      clusterinfo.UserDataSecretName,
			Namespace: clusterinfo.MachineAPINamespace,
		},
		CredentialsSecret: &core.SecretReference{
			Name:      defaultCredentialsSecretName,
			Namespace: clusterinfo.MachineAPINamespace,
		},
		Location: location,
		Zone:     &zone,
		VMSize:   vmSize,
		Image: azureprovider.Image{
			Publisher: defaultImagePublisher,
			Offer:     defaultImageOffer,
			SKU:       defaultImageSKU,
			Version:   defaultImageVersion,
		},
		OSDisk: azureprovider.OSDisk{
			OSType:     "Windows",
			DiskSizeGB: defaultOSDiskSizeGB,
			ManagedDisk: azureprovider.ManagedDisk{
				StorageAccountType: defaultStorageAccountType,
			},
		},
		PublicIP:             false,
		Subnet:               fmt.Sprintf("%s-worker-subnet", clusterID),
		ManagedIdentity:      fmt.Sprintf("%s-identity", clusterID),
		Vnet:                 fmt.Sprintf("%s-vnet", clusterID),
		ResourceGroup:        rg,
		NetworkResourceGroup: netrg,
	}, nil
}

// GenerateMachineSet generates the machineset object which is aws provider specific
func (p *Provider) GenerateMachineSet(withWindowsLabel bool, replicas int32) (*mapi.MachineSet, error) {
	clusterID, err := p.oc.GetInfrastructureID()
	if err != nil {
		return nil, fmt.Errorf("unable to get cluster id: %v", err)
	}
	platformStatus, err := p.oc.GetCloudProvider()
	if err != nil {
		return nil, fmt.Errorf("unable to get azure platform status: %v", err)
	}

	// Inspect master-0 to get Azure Location and Zone
	machines, err := p.oc.MachineClient.Machines("openshift-machine-api").Get(context.TODO(), clusterID+"-master-0", meta.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get master-0 machine resource: %v", err)
	}
	masterProviderSpec := new(azureprovider.AzureMachineProviderSpec)
	err = json.Unmarshal(machines.Spec.ProviderSpec.Value.Raw, masterProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal master-0 azure machine provider spec: %v", err)
	}

	// create new machine provider spec for deploying Windows node in the same Location and Zone as master-0
	providerSpec, err := newAzureMachineProviderSpec(clusterID, platformStatus, masterProviderSpec.Location, *masterProviderSpec.Zone, p.vmSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create new azure machine provider spec: %v", err)
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal azure machine provider spec: %v", err)
	}

	matchLabels := map[string]string{
		mapi.MachineClusterIDLabel: clusterID,
	}

	// On Azure the machine name for Windows VMs cannot be more than 15 characters long
	// The machine-api-operator derives the name from the MachineSet name,
	// adding '"-" + rand.String(5)'.
	// This leaves a max. 9 characters for the MachineSet name
	machineSetName := "e2e-wmco"
	if withWindowsLabel {
		matchLabels[clusterinfo.MachineOSIDLabel] = "Windows"
		// Designate machineSets that set a windows label on the machine with
		// "e2e-wmcow", as opposed to "e2e-wmco" for MachineSets that do not set it.
		machineSetName = machineSetName + "w"
	}
	matchLabels[clusterinfo.MachineSetLabel] = machineSetName

	machineLabels := map[string]string{
		clusterinfo.MachineRoleLabel: "worker",
		clusterinfo.MachineTypeLabel: "worker",
	}
	// append matchlabels to machinelabels
	for k, v := range matchLabels {
		machineLabels[k] = v
	}

	// Set up the test machineSet
	machineSet := &mapi.MachineSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      machineSetName,
			Namespace: clusterinfo.MachineAPINamespace,
			Labels: map[string]string{
				mapi.MachineClusterIDLabel: clusterID,
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
					ObjectMeta: mapi.ObjectMeta{
						Labels: map[string]string{
							"node-role.kubernetes.io/worker": "",
						},
					},
					ProviderSpec: mapi.ProviderSpec{
						Value: &runtime.RawExtension{
							Raw: rawProviderSpec,
						},
					},
				},
			},
		},
	}
	return machineSet, nil
}
