package clusterinfo

import (
	"context"
	"fmt"

	v1 "github.com/openshift/api/config/v1"
	clientset "github.com/openshift/client-go/config/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// OpenShift is an Client struct which will be used for all OpenShift related functions to interact with the existing
// Cluster.
type OpenShift struct {
	Client *clientset.Clientset
}

// GetOpenShift creates client for the current OpenShift cluster. If KUBECONFIG env var is set, it is used to
// create client, otherwise it uses in-cluster config.
func GetOpenShift() (*OpenShift, error) {
	rc, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating the config object %v", err)
	}

	oc, err := clientset.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &OpenShift{oc}, nil
}

// GetInfrastructureID returns the infrastructure ID of the OpenShift cluster or an error.
func (o *OpenShift) GetInfrastructureID() (string, error) {
	infra, err := o.getInfrastructure()
	if err != nil {
		return "", err
	}
	if infra.Status == (v1.InfrastructureStatus{}) {
		return "", fmt.Errorf("infrastructure status is nil")
	}
	return infra.Status.InfrastructureName, nil
}

// GetCloudProvider returns the Provider details of a given OpenShift client including provider type and region or
// an error.
func (o *OpenShift) GetCloudProvider() (*v1.PlatformStatus, error) {
	infra, err := o.getInfrastructure()
	if err != nil {
		return nil, err
	}
	if infra.Status == (v1.InfrastructureStatus{}) || infra.Status.PlatformStatus == nil {
		return nil, fmt.Errorf("error getting infrastructure status")
	}
	return infra.Status.PlatformStatus, nil
}

// getInfrastructure returns the information of current Infrastructure referred by the OpenShift client or an error.
func (o *OpenShift) getInfrastructure() (*v1.Infrastructure, error) {
	infra, err := o.Client.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return infra, nil
}
