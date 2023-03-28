package vsphere

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/machineset"
)

const (
	defaultCredentialsSecretName = "vsphere-cloud-credentials"
	storageClassName             = "e2e"
)

// Provider is a provider struct for testing vSphere
type Provider struct {
	oc *clusterinfo.OpenShift
	*config.InfrastructureStatus
}

// New returns a new vSphere provider struct with the given client set and ssh key pair
func New(clientset *clusterinfo.OpenShift, infraStatus *config.InfrastructureStatus) (*Provider, error) {
	return &Provider{
		oc:                   clientset,
		InfrastructureStatus: infraStatus,
	}, nil
}

// newVSphereMachineProviderSpec returns a vSphereMachineProviderSpec generated from the inputs, or an error
func (p *Provider) newVSphereMachineProviderSpec() (*mapi.VSphereMachineProviderSpec, error) {
	workspace, err := p.getWorkspaceFromExistingMachineSet()
	if err != nil {
		return nil, err
	}
	log.Printf("creating machineset provider spec which targets %s\n", workspace.Server)

	// The template is an image which has been properly sysprepped.  The image is derived from an environment variable
	// defined in the job spec.
	vmTemplate := os.Getenv("VM_TEMPLATE")
	if vmTemplate == "" {
		vmTemplate = "windows-golden-images/windows-server-2022-template"
	}

	log.Printf("creating machineset based on template %s\n", vmTemplate)

	return &mapi.VSphereMachineProviderSpec{
		TypeMeta: meta.TypeMeta{
			APIVersion: "vsphereprovider.openshift.io/v1beta1",
			Kind:       "VSphereMachineProviderSpec",
		},
		CredentialsSecret: &core.LocalObjectReference{
			Name: defaultCredentialsSecretName,
		},
		DiskGiB:   int32(128),
		MemoryMiB: int64(16384),
		Network: mapi.NetworkSpec{
			Devices: []mapi.NetworkDeviceSpec{{NetworkName: getNetwork()}},
		},
		NumCPUs:           int32(4),
		NumCoresPerSocket: int32(1),
		Template:          vmTemplate,
		Workspace:         workspace,
	}, nil
}

// getWorkspaceFromExistingMachineSet returns Workspace from a machineset provisioned during installation
func (p *Provider) getWorkspaceFromExistingMachineSet() (*mapi.Workspace, error) {
	listOptions := meta.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-cluster=" +
		p.InfrastructureName}
	machineSets, err := p.oc.Machine.MachineSets(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get machinesets")
	}

	if len(machineSets.Items) == 0 {
		return nil, errors.Wrap(err, "no matching machinesets found")
	}

	machineSet := machineSets.Items[0]
	providerSpecRaw := machineSet.Spec.Template.Spec.ProviderSpec.Value
	if providerSpecRaw == nil || providerSpecRaw.Raw == nil {
		return nil, errors.Wrap(err, "no provider spec found")
	}
	var providerSpec mapi.VSphereMachineProviderSpec
	err = json.Unmarshal(providerSpecRaw.Raw, &providerSpec)
	if err != nil {
		return nil, errors.Wrap(err, "unable to unmarshal providerSpec")
	}

	return providerSpec.Workspace, nil
}

// getNetwork returns the network that needs to be used in the MachineSet
func getNetwork() string {
	// Default network for dev environment
	networkSegment := "dev-segment"
	if os.Getenv("OPENSHIFT_CI") == "true" {
		// $LEASED_RESOURCE holds the network the CI cluster is in
		networkSegment = os.Getenv("LEASED_RESOURCE")
		// Default to "ci-segment" if the environment variable is not set.
		if networkSegment == "" {
			networkSegment = "ci-segment"
		}
	}
	return networkSegment
}

// GenerateMachineSet generates the MachineSet object which is vSphere provider specific
func (p *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32) (*mapi.MachineSet, error) {
	// create new machine provider spec for deploying Windows node
	providerSpec, err := p.newVSphereMachineProviderSpec()
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new vSphere machine provider spec")
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal vSphere machine provider spec")
	}

	return machineset.New(rawProviderSpec, p.InfrastructureName, replicas, withIgnoreLabel, ""), nil
}

func (p *Provider) GetType() config.PlatformType {
	return config.VSpherePlatformType
}

func (p *Provider) StorageSupport() bool {
	return true
}

// CreatePVC creates a PVC for a dynamically provisioned volume
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
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteOnce},
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("2Gi")},
			},
			StorageClassName: &sc.Name,
		},
	}
	return client.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), &pvcSpec, meta.CreateOptions{})
}

// ensureStorageClass ensures a vsphere-volume NTFS storage class exists for use with in-tree storage
func (p *Provider) ensureStorageClass(client client.Interface) (*storage.StorageClass, error) {
	sc, err := client.StorageV1().StorageClasses().Get(context.TODO(), storageClassName, meta.GetOptions{})
	if err == nil {
		return sc, nil
	} else if !k8sapierrors.IsNotFound(err) {
		return nil, fmt.Errorf("error getting storage class '%s': %w", storageClassName, err)
	}
	volumeBinding := storage.VolumeBindingImmediate
	reclaimPolicy := core.PersistentVolumeReclaimDelete
	sc = &storage.StorageClass{
		ObjectMeta: meta.ObjectMeta{
			Name: storageClassName,
		},
		Provisioner:       "kubernetes.io/vsphere-volume",
		Parameters:        map[string]string{"fstype": "ntfs"},
		ReclaimPolicy:     &reclaimPolicy,
		VolumeBindingMode: &volumeBinding,
	}
	return client.StorageV1().StorageClasses().Create(context.TODO(), sc, meta.CreateOptions{})
}
