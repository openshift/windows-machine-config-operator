package clusterinfo

import (
	"context"
	"fmt"

	config "github.com/openshift/api/config/v1"
	configClient "github.com/openshift/client-go/config/clientset/versioned"
	operatorClient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	mapiClient "github.com/openshift/machine-api-operator/pkg/generated/clientset/versioned/typed/machine/v1beta1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlruntimecfg "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// OpenShift is an Client struct which will be used for all OpenShift related functions to interact with the existing
// Cluster.
type OpenShift struct {
	ConfigClient   *configClient.Clientset
	OperatorClient operatorClient.OperatorV1Interface
	MachineClient  mapiClient.MachineV1beta1Interface
}

// GetOpenShift creates client for the current OpenShift cluster. If KUBECONFIG env var is set, it is used to
// create client, otherwise it uses in-cluster config.
func GetOpenShift() (*OpenShift, error) {
	rc, err := ctrlruntimecfg.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating the config object %v", err)
	}

	cc, err := configClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	oc, err := operatorClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	mapic, err := mapiClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	return &OpenShift{
		ConfigClient:   cc,
		OperatorClient: oc,
		MachineClient:  mapic,
	}, nil
}

// GetInfrastructureID returns the infrastructure ID of the OpenShift cluster or an error.
func (o *OpenShift) GetInfrastructureID() (string, error) {
	infra, err := o.getInfrastructure()
	if err != nil {
		return "", err
	}
	if infra.Status == (config.InfrastructureStatus{}) {
		return "", fmt.Errorf("infrastructure status is nil")
	}
	return infra.Status.InfrastructureName, nil
}

// GetCloudProvider returns the Provider details of a given OpenShift client including provider type and region or
// an error.
func (o *OpenShift) GetCloudProvider() (*config.PlatformStatus, error) {
	infra, err := o.getInfrastructure()
	if err != nil {
		return nil, err
	}
	if infra.Status == (config.InfrastructureStatus{}) || infra.Status.PlatformStatus == nil {
		return nil, fmt.Errorf("error getting infrastructure status")
	}
	return infra.Status.PlatformStatus, nil
}

// getInfrastructure returns the information of current Infrastructure referred by the OpenShift client or an error.
func (o *OpenShift) getInfrastructure() (*config.Infrastructure, error) {
	infra, err := o.ConfigClient.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	return infra, nil
}

// HasCustomVXLANPort tells if the custom VXLAN port is set or not in the cluster
func (o *OpenShift) HasCustomVXLANPort() (bool, error) {
	networkCR, err := o.OperatorClient.Networks().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return false, err
	}
	if networkCR.Spec.DefaultNetwork.OVNKubernetesConfig != nil &&
		networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig != nil &&
		networkCR.Spec.DefaultNetwork.OVNKubernetesConfig.HybridOverlayConfig.HybridOverlayVXLANPort != nil {
		return true, nil
	}
	return false, nil
}
