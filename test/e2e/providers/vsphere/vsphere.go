package vsphere

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	apps "k8s.io/api/apps/v1"
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
	defaultCredentialsSecretName = "vsphere-cloud-credentials"
	storageClassName             = "ntfs"
	windowsFSSName               = "win-internal-feature-states.csi.vsphere.vmware.com"
	csiNamespace                 = "openshift-cluster-csi-drivers"
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
		vmTemplate = "windows-golden-images/windows-server-2022-template-ipv6-disabled"
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
		return nil, fmt.Errorf("unable to get machinesets: %w", err)
	}

	if len(machineSets.Items) == 0 {
		return nil, fmt.Errorf("no matching machinesets found")
	}

	machineSet := machineSets.Items[0]
	providerSpecRaw := machineSet.Spec.Template.Spec.ProviderSpec.Value
	if providerSpecRaw == nil || providerSpecRaw.Raw == nil {
		return nil, fmt.Errorf("no provider spec found")
	}
	var providerSpec mapi.VSphereMachineProviderSpec
	err = json.Unmarshal(providerSpecRaw.Raw, &providerSpec)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal providerSpec: %w", err)
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
func (p *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32, windowsServerVersion windows.ServerVersion) (*mapi.MachineSet, error) {
	if windowsServerVersion != windows.Server2022 {
		return nil, fmt.Errorf("vSphere does not support Windows Server %s", windowsServerVersion)
	}

	// create new machine provider spec for deploying Windows node
	providerSpec, err := p.newVSphereMachineProviderSpec()
	if err != nil {
		return nil, fmt.Errorf("failed to create new vSphere machine provider spec: %w", err)
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal vSphere machine provider spec: %w", err)
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
	if err := p.ensureWindowsCSIDrivers(client); err != nil {
		return nil, err
	}
	// Use a StorageClass to allow for dynamic volume provisioning
	// https://docs.openshift.com/container-platform/4.12/storage/dynamic-provisioning.html#about_dynamic-provisioning
	sc, err := p.ensureStorageClass(client)
	if err != nil {
		return nil, fmt.Errorf("unable to ensure a usable StorageClass is created: %w", err)
	}
	pvcSpec := core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: storageClassName + "-",
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

// ensureStorageClass ensures a usable vSphere NTFS storage class exists
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
		Provisioner:       "csi.vsphere.vmware.com",
		Parameters:        map[string]string{"fstype": "ntfs"},
		ReclaimPolicy:     &reclaimPolicy,
		VolumeBindingMode: &volumeBinding,
	}
	return client.StorageV1().StorageClasses().Create(context.TODO(), sc, meta.CreateOptions{})
}

// ensureWindowsCSIDrivers ensures that the vSphere CSI drivers are deployed across Windows nodes
func (p *Provider) ensureWindowsCSIDrivers(client client.Interface) error {
	if err := p.ensureFSSConfigMap(client); err != nil {
		return err
	}
	return p.ensureWindowsCSIDaemonSet(client)
}

// ensureFSSConfigMap creates a feature state switch ConfigMap for Windows Nodes. The FSS used by Linux nodes is
// unusable as it does not set csi-windows-support to true
func (p *Provider) ensureFSSConfigMap(client client.Interface) error {
	fssCM := core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name: windowsFSSName,
		},
		Data: map[string]string{
			"block-volume-snapshot":            "true",
			"cnsmgr-suspend-create-volume":     "true",
			"csi-auth-check":                   "true",
			"csi-migration":                    "true",
			"improved-csi-idempotency":         "true",
			"improved-volume-topology":         "false",
			"online-volume-extend":             "true",
			"topology-preferential-datastores": "true",
			"csi-windows-support":              "true",
			"use-csinode-id":                   "true",
		},
		BinaryData: nil,
	}

	// See if the ConfigMap already exists in the state we expect it to be in.
	existingCM, err := client.CoreV1().ConfigMaps(csiNamespace).Get(context.TODO(), windowsFSSName, meta.GetOptions{})
	if err != nil {
		if !k8sapierrors.IsNotFound(err) {
			return fmt.Errorf("error getting existing FSS ConfigMap: %w", err)
		}
	} else if err == nil {
		if reflect.DeepEqual(existingCM.Data, fssCM.Data) {
			// ConfigMap is already as expected, nothing to do here.
			return nil
		}
		// Delete the ConfigMap as it has the wrong data.
		err = client.CoreV1().ConfigMaps(csiNamespace).Delete(context.TODO(), windowsFSSName, meta.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("error deleting existing FSS ConfigMap: %w", err)
		}
	}
	_, err = client.CoreV1().ConfigMaps(csiNamespace).Create(context.TODO(), &fssCM, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("could not create FSS ConfigMap: %w", err)
	}
	return nil
}

// ensureWindowsCSIDaemonSet deploys the Windows CSI driver DaemonSet if it doesn't already exist
func (p *Provider) ensureWindowsCSIDaemonSet(client client.Interface) error {
	dsName := "vmware-vsphere-csi-driver-node-windows"
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
					PriorityClassName:  "system-node-critical",
					NodeSelector:       map[string]string{core.LabelOSStable: "windows"},
					ServiceAccountName: "vmware-vsphere-csi-driver-node-sa",
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
							Image: "k8s.gcr.io/sig-storage/csi-node-driver-registrar:v2.7.0",
							Args:  []string{"--v=5", "--csi-address=$(ADDRESS)", "-kubelet-registration-path=$(DRIVER_REG_SOCK_PATH)"},
							Env: []core.EnvVar{
								{
									Name:  "ADDRESS",
									Value: `unix://C:\\csi\\csi.sock`,
								},
								{
									Name:  "DRIVER_REG_SOCK_PATH",
									Value: `C:\\var\\lib\\kubelet\\plugins\\csi.vsphere.vmware.com\\csi.sock`,
								},
							},
							VolumeMounts: []core.VolumeMount{
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
							Name:  "vsphere-csi-node",
							Image: "gcr.io/cloud-provider-vsphere/csi/release/driver:v3.0.0",
							Args:  []string{"--fss-name=" + windowsFSSName, "--fss-namespace=$(CSI_NAMESPACE)"},
							Env: []core.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &core.EnvVarSource{
										FieldRef: &core.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "spec.nodeName",
										},
									},
								},
								{
									Name:  "CSI_ENDPOINT",
									Value: `unix://C:\\csi\\csi.sock`,
								},
								{
									Name:  "MAX_VOLUMES_PER_NODE",
									Value: "59",
								},
								{
									Name:  "X_CSI_MODE",
									Value: "node",
								},
								{
									Name:  "X_CSI_SPEC_REQ_VALIDATION",
									Value: "false",
								},
								{
									Name:  "X_CSI_SPEC_DISABLE_LEN_CHECK",
									Value: "true",
								},
								{
									Name: "CSI_NAMESPACE",
									ValueFrom: &core.EnvVarSource{
										FieldRef: &core.ObjectFieldSelector{
											APIVersion: "v1",
											FieldPath:  "metadata.namespace",
										},
									},
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/csi",
								},
								{
									Name:      "pod-mount-dir",
									MountPath: "/var/lib/kubelet",
								},
								{
									Name:      "csi-proxy-volume-v1",
									MountPath: `\\.\pipe\csi-proxy-volume-v1`,
								},
								{
									Name:      "csi-proxy-filesystem-v1",
									MountPath: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
								{
									Name:      "csi-proxy-disk-v1",
									MountPath: `\\.\pipe\csi-proxy-disk-v1`,
								},
								{
									Name:      "csi-proxy-system-v1alpha1",
									MountPath: `\\.\pipe\csi-proxy-system-v1alpha1`,
								},
							},
						},
					},
					Volumes: []core.Volume{
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
							Name: "plugin-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet\plugins\csi.vsphere.vmware.com\`,
									Type: &directoryOrCreate,
								},
							},
						},
						{
							Name: "pod-mount-dir",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `C:\var\lib\kubelet`,
									Type: &directoryType,
								},
							},
						},
						{
							Name: "csi-proxy-disk-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-disk-v1`,
								},
							},
						},
						{
							Name: "csi-proxy-volume-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-volume-v1`,
								},
							},
						},
						{
							Name: "csi-proxy-filesystem-v1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-filesystem-v1`,
								},
							},
						},
						{
							Name: "csi-proxy-system-v1alpha1",
							VolumeSource: core.VolumeSource{
								HostPath: &core.HostPathVolumeSource{
									Path: `\\.\pipe\csi-proxy-system-v1alpha1`,
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
	} else if err == nil {
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
