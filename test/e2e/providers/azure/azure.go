package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
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
	csiNamespace  = "openshift-cluster-csi-drivers"
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
func (p *Provider) newAzureMachineProviderSpec(location string, zone *string, windowsServerVersion windows.ServerVersion) (*mapi.AzureMachineProviderSpec, error) {
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
		Zone:     zone,
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
	// Inspect master-0 to get Azure Location and Zone
	machines, err := p.oc.Machine.Machines(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		p.InfrastructureName+"-master-0", meta.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get master-0 machine resource: %v", err)
	}
	masterProviderSpec := new(mapi.AzureMachineProviderSpec)
	err = json.Unmarshal(machines.Spec.ProviderSpec.Value.Raw, masterProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal master-0 azure machine provider spec: %v", err)
	}

	// create new machine provider spec for deploying Windows node in the same Location and Zone as master-0
	providerSpec, err := p.newAzureMachineProviderSpec(masterProviderSpec.Location, masterProviderSpec.Zone,
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

func (p *Provider) GetType() config.PlatformType {
	return config.AzurePlatformType
}

func (p *Provider) StorageSupport() bool {
	return true
}

func (p *Provider) CreatePVC(c client.Interface, namespace string) (*core.PersistentVolumeClaim, error) {
	if err := p.ensureWindowsCSIDaemonSet(c); err != nil {
		return nil, err
	}
	storageClassName := "azurefile-csi"
	pvcSpec := core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: "e2e" + "-",
		},
		Spec: core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteMany},
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("2Gi")},
			},
			StorageClassName: &storageClassName,
		},
	}
	return c.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), &pvcSpec, meta.CreateOptions{})
}

// ensureWindowsCSIDaemonSet deploys the Windows CSI driver DaemonSet if it doesn't already exist
func (p *Provider) ensureWindowsCSIDaemonSet(client client.Interface) error {
	dsName := "azure-file-csi-driver-node-windows"
	directoryType := core.HostPathDirectory
	directoryOrCreate := core.HostPathDirectoryOrCreate
	ds := apps.DaemonSet{
		ObjectMeta: meta.ObjectMeta{
			Name: dsName,
		},
		Spec: apps.DaemonSetSpec{
			Selector: &meta.LabelSelector{MatchLabels: map[string]string{"app": dsName}},
			Template: core.PodTemplateSpec{
				ObjectMeta: meta.ObjectMeta{
					Labels: map[string]string{"app": dsName},
				},
				Spec: core.PodSpec{
					PriorityClassName: "system-node-critical",
					NodeSelector:      map[string]string{core.LabelOSStable: "windows"},
					// Use the controller-sa as the node-sa doesn't have GET secrets permissions required for Windows
					ServiceAccountName: "azure-file-csi-driver-controller-sa",
					OS:                 &core.PodOS{Name: core.Windows},
					Tolerations: []core.Toleration{
						{
							Key:    "os",
							Value:  "Windows",
							Effect: core.TaintEffectNoSchedule,
						},
					},
					Containers: []core.Container{
						{
							Name:  "node-driver-registrar",
							Image: "mcr.microsoft.com/oss/kubernetes-csi/csi-node-driver-registrar:v2.8.0",
							Args:  []string{"--v=2", "--csi-address=$(CSI_ENDPOINT)", "-kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)"},
							Env: []core.EnvVar{
								{
									Name:  "CSI_ENDPOINT",
									Value: `unix://C:\\csi\\csi.sock`,
								},
								{
									Name:  "DRIVER_REG_SOCK_PATH",
									Value: `C:\\var\\lib\\kubelet\\plugins\\file.csi.azure.com\\csi.sock`,
								},
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &core.EnvVarSource{
										FieldRef: &core.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "kubelet-dir",
									MountPath: "/var/lib/kubelet",
								},
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "registration-dir",
									MountPath: "/registration",
								},
							},
						},
						{
							Name:  "azurefile",
							Image: "mcr.microsoft.com/k8s/csi/azurefile-csi:latest",
							Args:  []string{"--v=5", "--endpoint=$(CSI_ENDPOINT)", "--nodeid=$(KUBE_NODE_NAME)"},
							Env: []core.EnvVar{
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &core.EnvVarSource{
										FieldRef: &core.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name:  "CSI_ENDPOINT",
									Value: `unix://C:\\csi\\csi.sock`,
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "kubelet-dir",
									MountPath: "/var/lib/kubelet",
								},
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "azure-config",
									MountPath: "/k",
								},
								{
									Name:      "csi-proxy-filesystem-v1",
									MountPath: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
								{
									Name:      "csi-proxy-smb-pipe-v1",
									MountPath: `\\.\pipe\csi-proxy-smb-v1`,
								},
							},
						},
					},
					Volumes: []core.Volume{
						{
							Name: "csi-proxy-filesystem-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
							},
						},
						{
							Name: "csi-proxy-smb-pipe-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-smb-v1`,
								},
							},
						},
						{
							Name: "registration-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet\plugins_registry\`,
									Type: &directoryType,
								},
							},
						},
						{
							Name: "kubelet-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet`,
									Type: &directoryType,
								},
							},
						},
						{
							Name: "plugin-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet\plugins\file.csi.azure.com\`,
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "azure-config",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\k\`,
									Type: &directoryOrCreate,
								},
							},
						},
					},
				},
			},
		},
	}
	// See if the DaemonSet already exists in the state we expect it to be in.
	existingDS, err := client.AppsV1().DaemonSets(csiNamespace).Get(context.TODO(), dsName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing Windows CSI DaemonSet: %w", err)
		}
	} else {
		if reflect.DeepEqual(existingDS.Spec, ds.Spec) {
			// DaemonSet is already as expected, nothing to do here.
			return nil
		}
		// Delete the DaemonSet as it has the wrong spec.
		err = client.AppsV1().DaemonSets(csiNamespace).Delete(context.TODO(), dsName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing Windows CSI DaemonSet: %w", err)

		}
	}
	_, err = client.AppsV1().DaemonSets(csiNamespace).Create(context.TODO(), &ds, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating Windows CSI DaemonSet: %w", err)
	}
	return nil
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
