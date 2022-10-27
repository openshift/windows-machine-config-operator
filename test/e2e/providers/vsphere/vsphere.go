package vsphere

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/pkg/errors"

	config "github.com/openshift/api/config/v1"
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
func (p *Provider) newVSphereMachineProviderSpec(clusterID string) (*vsphere.VSphereMachineProviderSpec, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("clusterID is empty")
	}
	workspace, err := p.getWorkspaceFromExistingMachineSet(clusterID)
	if err != nil {
		return nil, err
	}
	log.Printf("creating machineset provider spec which targets %s\n", workspace.Server)

	// The template is an image which has been properly sysprepped.  The image is derived from an environment variable
	// defined in the job spec.
	vmTemplate := os.Getenv("VM_TEMPLATE")
	if vmTemplate == "" {
		vmTemplate = "windows-golden-images/windows-server-2022-template-with-docker"
	}

	log.Printf("creating machineset based on template %s\n", vmTemplate)

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
			Devices: []vsphere.NetworkDeviceSpec{{NetworkName: getNetwork()}},
		},
		NumCPUs:           int32(4),
		NumCoresPerSocket: int32(1),
		Template:          vmTemplate,
		Workspace:         workspace,
	}, nil
}

// getWorkspaceFromExistingMachineSet returns Workspace from a machineset provisioned during installation
func (p *Provider) getWorkspaceFromExistingMachineSet(clusterID string) (*vsphere.Workspace, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("clusterID is empty")
	}
	listOptions := meta.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-cluster=" + clusterID}
	machineSets, err := p.oc.Machine.MachineSets("openshift-machine-api").List(context.TODO(), listOptions)
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
	var providerSpec vsphere.VSphereMachineProviderSpec
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
func (p *Provider) GenerateMachineSet(withWindowsLabel bool, replicas int32) (*mapi.MachineSet, error) {
	clusterID, err := p.oc.GetInfrastructureID()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get cluster id")
	}

	// create new machine provider spec for deploying Windows node
	providerSpec, err := p.newVSphereMachineProviderSpec(clusterID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create new vSphere machine provider spec")
	}

	rawProviderSpec, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal vSphere machine provider spec")
	}

	matchLabels := map[string]string{
		mapi.MachineClusterIDLabel:  clusterID,
		clusterinfo.MachineE2ELabel: "true",
	}

	// On vSphere the Machine name for Windows VMs cannot be more than 15 characters long
	// The machine-api-operator derives the name from the MachineSet name,
	// adding '"-" + rand.String(5)'.
	// This leaves a max. 9 characters for the MachineSet name
	machineSetName := clusterinfo.WindowsMachineSetName(withWindowsLabel)
	if withWindowsLabel {
		matchLabels[clusterinfo.MachineOSIDLabel] = "Windows"
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
				mapi.MachineClusterIDLabel:  clusterID,
				clusterinfo.MachineE2ELabel: "true",
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

func (p *Provider) GetType() config.PlatformType {
	return config.VSpherePlatformType
}
