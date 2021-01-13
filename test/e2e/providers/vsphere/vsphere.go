package vsphere

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	vsphere "github.com/openshift/machine-api-operator/pkg/apis/vsphereprovider/v1beta1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

const defaultCredentialsSecretName = "vsphere-cloud-credentials"

// Provider is a provider struct for testing vSphere
type Provider struct {
	oc *clusterinfo.OpenShift
}

// New returns a new vSphere provider struct with the given client set and ssh key pair
func New(clientset *clusterinfo.OpenShift) (*Provider, error) {
	return &Provider{
		oc: clientset,
	}, nil
}

// newVSphereMachineProviderSpec returns a vSphereMachineProviderSpec generated from the inputs, or an error
func newVSphereMachineProviderSpec(clusterID string) (*vsphere.VSphereMachineProviderSpec, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("clusterID is empty")
	}

	return &vsphere.VSphereMachineProviderSpec{
		TypeMeta: meta.TypeMeta{
			APIVersion: "vsphereprovider.openshift.io/v1beta1",
			Kind:       "VSphereMachineProviderSpec",
		},
		CredentialsSecret: &core.LocalObjectReference{
			Name: defaultCredentialsSecretName,
		},
		DiskGiB:   int32(128),
		MemoryMiB: int64(16384),
		Network: vsphere.NetworkSpec{
			Devices: []vsphere.NetworkDeviceSpec{{NetworkName: "ci-segment"}},
		},
		NumCPUs:           int32(4),
		NumCoresPerSocket: int32(1),
		// The template is hardcoded with an image which has been properly sysprepped.
		// TODO: Find a way to automatically update this with latest image
		Template: "1909-template-with-docker-ssh",
		Workspace: &vsphere.Workspace{
			Datacenter:   "SDDC-Datacenter",
			Datastore:    "WorkloadDatastore",
			Folder:       "/SDDC-Datacenter/vm/" + clusterID,
			ResourcePool: "/SDDC-Datacenter/host/Cluster-1/Resources",
			Server:       "vcenter.sddc-44-236-21-251.vmwarevmc.com",
		},
	}, nil
}

// GenerateMachineSet generates the MachineSet object which is vSphere provider specific
func (p *Provider) GenerateMachineSet(withWindowsLabel bool, replicas int32) (*mapi.MachineSet, error) {
	clusterID, err := p.oc.GetInfrastructureID()
	if err != nil {
		return nil, fmt.Errorf("unable to get cluster id: %v", err)
	}

	// create new machine provider spec for deploying Windows node
	providerSpec, err := newVSphereMachineProviderSpec(clusterID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new vSphere machine provider spec")
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal vSphere machine provider spec")
	}

	matchLabels := map[string]string{
		mapi.MachineClusterIDLabel: clusterID,
	}

	// On vSphere the Machine name for Windows VMs cannot be more than 15 characters long
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
