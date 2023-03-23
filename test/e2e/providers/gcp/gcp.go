package gcp

import (
	"context"
	"encoding/json"
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/machineset"
)

// Provider is a provider struct for testing GCP
type Provider struct {
	oc *clusterinfo.OpenShift
	*config.InfrastructureStatus
}

// New returns a new GCP provider
func New(clientset *clusterinfo.OpenShift, infraStatus *config.InfrastructureStatus) *Provider {
	return &Provider{
		oc:                   clientset,
		InfrastructureStatus: infraStatus,
	}
}

// GenerateMachineSet generates a MachineSet object which is GCP provider specific
func (p *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32) (*mapi.MachineSet, error) {
	gcpSpec, err := p.newGCPProviderSpec()
	if err != nil {
		return nil, err
	}
	rawSpec, err := json.Marshal(gcpSpec)
	if err != nil {
		return nil, fmt.Errorf("error marshalling gcp provider spec: %w", err)
	}
	return machineset.New(rawSpec, p.InfrastructureName, replicas, withIgnoreLabel, p.InfrastructureName+"-"), nil
}

// newGCPProviderSpec returns a GCPMachineProviderSpec which describes a Windows server 2022 VM
func (p *Provider) newGCPProviderSpec() (*mapi.GCPMachineProviderSpec, error) {
	listOptions := meta.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-machine-role=worker"}
	machines, err := p.oc.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}
	if len(machines.Items) == 0 {
		return nil, fmt.Errorf("found 0 worker role machines")
	}
	foundSpec := &mapi.GCPMachineProviderSpec{}
	err = json.Unmarshal(machines.Items[0].Spec.ProviderSpec.Value.Raw, foundSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal raw machine provider spec: %v", err)
	}

	return &mapi.GCPMachineProviderSpec{
		TypeMeta: meta.TypeMeta{
			APIVersion: "machine.openshift.io/v1beta1",
			Kind:       "GCPMachineProviderSpec",
		},
		ObjectMeta: meta.ObjectMeta{},
		UserDataSecret: &core.LocalObjectReference{
			Name: clusterinfo.UserDataSecretName,
		},
		CredentialsSecret: &core.LocalObjectReference{
			Name: foundSpec.CredentialsSecret.Name,
		},
		CanIPForward:       false,
		DeletionProtection: false,
		Disks: []*mapi.GCPDisk{{
			AutoDelete: true,
			Boot:       true,
			SizeGB:     128,
			Type:       "pd-ssd",
			// use the latest image from the `windows-2022-core` family in the `windows-cloud` project
			Image: "projects/windows-cloud/global/images/family/windows-2022-core",
		}},
		NetworkInterfaces: foundSpec.NetworkInterfaces,
		ServiceAccounts:   foundSpec.ServiceAccounts,
		Tags:              foundSpec.Tags,
		MachineType:       foundSpec.MachineType,
		Region:            foundSpec.Region,
		Zone:              foundSpec.Zone,
		ProjectID:         foundSpec.ProjectID,
	}, nil
}

// GetType returns the GCP platform type
func (p *Provider) GetType() config.PlatformType {
	return config.GCPPlatformType
}

func (p *Provider) StorageSupport() bool {
	return false
}

func (p *Provider) CreatePVC(_ client.Interface, _ string) (*core.PersistentVolumeClaim, error) {
	return nil, fmt.Errorf("storage not supported on gcp")
}
