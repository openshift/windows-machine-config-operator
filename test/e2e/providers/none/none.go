package none

import (
	"context"
	"fmt"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	client "k8s.io/client-go/kubernetes"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/windows"
)

// Provider is a provider struct for testing platform=none
type Provider struct {
	oc *clusterinfo.OpenShift
}

// New returns a Provider implementation for platform=none
func New(clientset *clusterinfo.OpenShift) (*Provider, error) {
	return &Provider{
		oc: clientset,
	}, nil
}

// GenerateMachineSet is not supported for platform=none and throws an exception
func (p *Provider) GenerateMachineSet(_ bool, replicas int32, version windows.ServerVersion) (*mapi.MachineSet, error) {
	return nil, fmt.Errorf("MachineSet generation not supported for platform=none")
}

// GetType returns the platform type for platform=none
func (p *Provider) GetType() config.PlatformType {
	return config.NonePlatformType
}

func (p *Provider) StorageSupport() bool {
	return true
}

func (p *Provider) CreatePVC(c client.Interface, namespace string, pv *core.PersistentVolume) (*core.PersistentVolumeClaim, error) {
	if pv == nil {
		return nil, fmt.Errorf("a PV must be provided for platform none")
	}
	pvcSpec := core.PersistentVolumeClaim{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: "smb-pvc" + "-",
		},
		Spec: core.PersistentVolumeClaimSpec{
			AccessModes: []core.PersistentVolumeAccessMode{core.ReadWriteMany},
			Resources: core.ResourceRequirements{
				Requests: core.ResourceList{core.ResourceStorage: resource.MustParse("1Gi")},
			},
			StorageClassName: &pv.Spec.StorageClassName,
		},
	}
	return c.CoreV1().PersistentVolumeClaims(namespace).Create(context.TODO(), &pvcSpec, meta.CreateOptions{})
}
