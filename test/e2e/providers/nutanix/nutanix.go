package nutanix

import (
	"context"
	"encoding/json"
	"fmt"

	config "github.com/openshift/api/config/v1"
	machinev1 "github.com/openshift/api/machine/v1"
	machinev1beta1 "github.com/openshift/api/machine/v1beta1"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers/machineset"
)

type Provider struct {
	oc *clusterinfo.OpenShift
	*config.InfrastructureStatus
}

// New returns a new Provider
func New(oc *clusterinfo.OpenShift, infraStatus *config.InfrastructureStatus) (*Provider, error) {
	return &Provider{
		oc:                   oc,
		InfrastructureStatus: infraStatus,
	}, nil
}

// GenerateMachineSet generates a Windows MachineSet which is Nutanix provider specific
func (a *Provider) GenerateMachineSet(withIgnoreLabel bool, replicas int32) (*machinev1beta1.MachineSet, error) {
	listOptions := meta.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-machine-role=worker"}
	machines, err := a.oc.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
	if err != nil {
		return nil, err
	}
	if len(machines.Items) == 0 {
		return nil, fmt.Errorf("found 0 worker role machines")
	}

	workerSpec := &machinev1.NutanixMachineProviderConfig{}
	err = json.Unmarshal(machines.Items[0].Spec.ProviderSpec.Value.Raw, workerSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal raw machine provider spec: %v", err)
	}

	// The Windows image named "nutanix-windows-server" was pre-uploaded to the Nutanix CI prism-central.
	// This image has "Windows Server 2022" type.
	winImageName := "nutanix-windows-server"
	winImageIdentifier := machinev1.NutanixResourceIdentifier{
		Type: machinev1.NutanixIdentifierName,
		Name: &winImageName,
	}

	providerSpec := &machinev1.NutanixMachineProviderConfig{
		Cluster:           workerSpec.Cluster,
		Image:             winImageIdentifier,
		Subnets:           workerSpec.Subnets,
		VCPUsPerSocket:    workerSpec.VCPUsPerSocket,
		VCPUSockets:       workerSpec.VCPUSockets,
		MemorySize:        workerSpec.MemorySize,
		SystemDiskSize:    workerSpec.SystemDiskSize,
		CredentialsSecret: workerSpec.CredentialsSecret,
		UserDataSecret:    &core.LocalObjectReference{Name: clusterinfo.UserDataSecretName},
	}

	rawBytes, err := json.Marshal(providerSpec)
	if err != nil {
		return nil, err
	}

	return machineset.New(rawBytes, a.InfrastructureName, replicas, withIgnoreLabel, a.InfrastructureName+"-"), nil
}

func (a *Provider) GetType() config.PlatformType {
	return config.NutanixPlatformType
}

func (a *Provider) StorageSupport() bool {
	return false
}

func (a *Provider) CreatePVC(_ client.Interface, _ string) (*core.PersistentVolumeClaim, error) {
	return nil, fmt.Errorf("storage not supported on Nutanix")
}
