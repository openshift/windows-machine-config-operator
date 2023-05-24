package azure

import (
	"context"
	"encoding/json"
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/machineset"
	"github.com/openshift/windows-machine-config-operator/test/e2e/windows"
)

const (
	defaultCredentialsSecretName = "azure-cloud-credentials"
	defaultImageOffer            = "WindowsServer"
	defaultImagePublisher        = "MicrosoftWindowsServer"
	defaultImageVersion          = "latest"
	defaultOSDiskSizeGB          = 128
	defaultStorageAccountType    = "Premium_LRS"
	// The default vm size set by machine-api-operator yields
	// "unknown instance type: Standard_D4s_V3" on dev cluster instances.
	// Use the instance type the other worker machines use.
	defaultVMSize = "Standard_D2s_v3"
	// storageClassName is the name of the StorageClass that will be created for tests
	storageClassName = "e2e"
)

// Provider is a provider struct for testing Azure
type Provider struct {
	oc *clusterinfo.OpenShift
	*config.InfrastructureStatus
	vmSize string
}

// New returns a new Azure provider struct with the give client set and ssh key pair
func New(clientset *clusterinfo.OpenShift, infraStatus *config.InfrastructureStatus) *Provider {
	return &Provider{
		oc:                   clientset,
		InfrastructureStatus: infraStatus,
		vmSize:               defaultVMSize,
	}
}

// newAzureMachineProviderSpec returns an AzureMachineProviderSpec generated from the inputs, or an error
func (p *Provider) newAzureMachineProviderSpec(location, zone string, windowsServerVersion windows.ServerVersion) (*mapi.AzureMachineProviderSpec, error) {
	return &mapi.AzureMachineProviderSpec{
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
		VMSize:   p.vmSize,
		Image: mapi.Image{
			Publisher: defaultImagePublisher,
			Offer:     defaultImageOffer,
			SKU:       getImageSKU(windowsServerVersion),
			Version:   defaultImageVersion,
		},
		OSDisk: mapi.OSDisk{
			OSType:     "Windows",
			DiskSizeGB: defaultOSDiskSizeGB,
			ManagedDisk: mapi.OSDiskManagedDiskParameters{
				StorageAccountType: defaultStorageAccountType,
			},
		},
		PublicIP:             false,
		Subnet:               fmt.Sprintf("%s-worker-subnet", p.InfrastructureName),
		ManagedIdentity:      fmt.Sprintf("%s-identity", p.InfrastructureName),
		Vnet:                 fmt.Sprintf("%s-vnet", p.InfrastructureName),
		ResourceGroup:        p.PlatformStatus.Azure.ResourceGroupName,
		NetworkResourceGroup: p.PlatformStatus.Azure.NetworkResourceGroupName,
	}, nil
}

// GenerateMachineSet generates the machineset object which is aws provider specific
func (p *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32, windowsServerVersion windows.ServerVersion) (*mapi.MachineSet, error) {
	// Create the Windows Machines in the same location and zone as another node in the cluster
	masterProviderSpec, err := p.getExistingMachineProviderSpec()
	if err != nil {
		return nil, err
	}

	// create new machine provider spec for deploying Windows node in the same Location and Zone as master-0
	providerSpec, err := p.newAzureMachineProviderSpec(masterProviderSpec.Location, *masterProviderSpec.Zone,
		windowsServerVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to create new azure machine provider spec: %v", err)
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal azure machine provider spec: %v", err)
	}

	return machineset.New(rawProviderSpec, p.InfrastructureName, replicas, withIgnoreLabel, ""), nil
}

// getExistingMachineProviderSpec returns the spec of the *-master-0 Machine. The same Machine will always be returned
// for the sake of consistency.
func (p *Provider) getExistingMachineProviderSpec() (*mapi.AzureMachineProviderSpec, error) {
	// Inspect master-0 to get Azure Location and Zone
	machine, err := p.oc.Machine.Machines(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		p.InfrastructureName+"-master-0", meta.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get master-0 machine resource: %v", err)
	}
	masterProviderSpec := new(mapi.AzureMachineProviderSpec)
	err = json.Unmarshal(machine.Spec.ProviderSpec.Value.Raw, masterProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal master-0 azure machine provider spec: %v", err)
	}
	return masterProviderSpec, nil
}

func (p *Provider) GetType() config.PlatformType {
	return config.AzurePlatformType
}

func (p *Provider) StorageSupport() bool {
	return true
}

// CreatePVC creates a PVC for a dynamically provisioned azure-file volume
func (p *Provider) CreatePVC(client client.Interface, namespace string) (*core.PersistentVolumeClaim, error) {
	// Use a StorageClass to allow for dynamic volume provisioning
	// https://docs.openshift.com/container-platform/4.12/storage/dynamic-provisioning.html#about_dynamic-provisioning
	sc, err := p.ensureStorageClass(client)
	if err != nil {
		return nil, fmt.Errorf("unable to ensure a usable StorageClass is created: %w", err)
	}
	pvcSpec := core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: "e2e-",
		},
		Spec: core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteMany},
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("2Gi")},
			},
			StorageClassName: &sc.Name,
		},
	}
	return client.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), &pvcSpec, meta.CreateOptions{})
}

// ensureStorageClass ensures an azure-file storage class exists for use with in-tree storage
func (p *Provider) ensureStorageClass(client client.Interface) (*storage.StorageClass, error) {
	sc, err := client.StorageV1().StorageClasses().Get(context.TODO(), storageClassName, meta.GetOptions{})
	if err == nil {
		return sc, nil
	} else if !k8sapierrors.IsNotFound(err) {
		return nil, fmt.Errorf("error getting storage class '%s': %w", storageClassName, err)
	}

	// get the location that this StorageClass will be usable in
	masterProviderSpec, err := p.getExistingMachineProviderSpec()
	if err != nil {
		return nil, err
	}
	clusterLocation := masterProviderSpec.Location
	volumeBinding := storage.VolumeBindingImmediate
	reclaimPolicy := core.PersistentVolumeReclaimDelete
	sc = &storage.StorageClass{
		ObjectMeta: meta.ObjectMeta{
			Name: storageClassName,
		},
		Provisioner:       "kubernetes.io/azure-file",
		Parameters:        map[string]string{"location": clusterLocation, "skuName": "Standard_LRS"},
		ReclaimPolicy:     &reclaimPolicy,
		VolumeBindingMode: &volumeBinding,
	}
	return client.StorageV1().StorageClasses().Create(context.TODO(), sc, meta.CreateOptions{})
}

// getImageSKU returns the SKU based on the Windows Server version
func getImageSKU(windowsServerVersion windows.ServerVersion) string {
	switch windowsServerVersion {
	case windows.Server2019:
		return "2019-datacenter-smalldisk"
	case windows.Server2022:
	default:
	}
	return "2022-datacenter-smalldisk"
}
