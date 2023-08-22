package nodeconfig

import (
	"context"
	"fmt"

	clientset "github.com/openshift/client-go/config/clientset/versioned"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	crclientcfg "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// cache holds the information of the nodeConfig that is invariant for multiple reconciliation cycles. We'll use this
// information when we don't want to get the information from the global context coming from reconciler
// but to have something at nodeConfig package locally which will be passed onto other structs. There is no need to
// invalidate this cache as of now, since if someone wants to change any of the fields, they've to restart the operator
// which will invalidate the cache automatically.
type cache struct {
	// apiServerEndpoint is the address which clients can interact with the API server through
	apiServerEndpoint string
}

func (c cache) GetEndpoint() string {
	return c.apiServerEndpoint
}

// cache has the information related to nodeConfig that should not be changed.
var nodeConfigCache = cache{}

// init populates the cache that we need for nodeConfig
func init() {
	var kubeAPIServerEndpoint string
	log := ctrl.Log.WithName("nodeconfig").WithName("init")

	kubeAPIServerEndpoint, err := discoverKubeAPIServerEndpoint()
	if err != nil {
		log.Error(err, "unable to find kube api server endpoint")
		return
	}
	nodeConfigCache.apiServerEndpoint = kubeAPIServerEndpoint
}

// discoverKubeAPIServerEndpoint discovers the kubernetes api server endpoint
func discoverKubeAPIServerEndpoint() (string, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return "", fmt.Errorf("unable to get config to talk to kubernetes api server: %w", err)
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("unable to get client from the given config: %w", err)
	}

	host, err := client.ConfigV1().Infrastructures().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("unable to get cluster infrastructure resource: %w", err)
	}
	// get API server internal url of format https://api-int.abc.devcluster.openshift.com:6443
	if host.Status.APIServerInternalURL == "" {
		return "", fmt.Errorf("could not get host name for the kubernetes api server")
	}
	return host.Status.APIServerInternalURL, nil
}
