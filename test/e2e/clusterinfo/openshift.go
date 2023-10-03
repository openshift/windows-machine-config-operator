package clusterinfo

import (
	"context"
	"fmt"

	config "github.com/openshift/api/config/v1"
	configClient "github.com/openshift/client-go/config/clientset/versioned"
	imageClient "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	mapiClient "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	operatorClient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	routeClient "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/operator-framework/operator-lifecycle-manager/pkg/api/client/clientset/versioned"
	monitoringClient "github.com/prometheus-operator/prometheus-operator/pkg/client/versioned/typed/monitoring/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sclient "k8s.io/client-go/kubernetes"
	ctrlruntimecfg "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// OpenShift contains clients for interacting with the OpenShift API
type OpenShift struct {
	Config     *configClient.Clientset
	Operator   operatorClient.OperatorV1Interface
	Machine    mapiClient.MachineV1beta1Interface
	Monitoring monitoringClient.MonitoringV1Interface
	Route      routeClient.RouteV1Interface
	K8s        k8sclient.Interface
	Images     imageClient.ImageV1Interface
	Olm        versioned.Interface
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

	monc, err := monitoringClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	routec, err := routeClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}

	kc, err := k8sclient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	ic, err := imageClient.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	olmc, err := versioned.NewForConfig(rc)
	if err != nil {
		return nil, err
	}
	return &OpenShift{
		Config:     cc,
		Operator:   oc,
		Machine:    mapic,
		Monitoring: monc,
		Route:      routec,
		K8s:        kc,
		Images:     ic,
		Olm:        olmc,
	}, nil
}

// GetInfrastructure returns the information of current Infrastructure referred by the OpenShift client or an error.
func (o *OpenShift) GetInfrastructure() (*config.Infrastructure, error) {
	infra, err := o.Config.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return nil, err
	}
	return infra, nil
}

// ProxyEnabled queries the Proxy resource to see if a cluster-wide proxy is enabled in this environment
func (o *OpenShift) ProxyEnabled() (bool, error) {
	clusterProxy, err := o.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return false, err
	}
	return clusterProxy.Status.HTTPProxy != "" || clusterProxy.Status.HTTPSProxy != "", nil
}
