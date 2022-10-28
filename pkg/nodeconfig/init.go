package nodeconfig

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	crclientcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

// cache holds the information of the nodeConfig that is invariant for multiple reconciliation cycles. We'll use this
// information when we don't want to get the information from the global context coming from reconciler
// but to have something at nodeConfig package locally which will be passed onto other structs. There is no need to
// invalidate this cache as of now, since the only entry in this workerIgnitionEndPoint which will be immutable. If
// someone wants to change it, they've to restart the operator which will invalidate the cache automatically.
// Note: It is ok to remove this struct in future, if we don't want to continue. As of now, I can think of only
// worker ignition endpoint being part of this struct.
type cache struct {
	// workerIgnitionEndpoint is the Machine Config Server(MCS) endpoint from which we can download the
	// the OpenShift worker ignition file.
	workerIgnitionEndPoint string
	// apiServerEndpoint is the address which clients can interact with the API server through
	apiServerEndpoint string
	// credentials holds a certificate and token needed to interact with the API server
	credentials *windows.Authentication
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
	clusterAddress, err := getClusterAddr(kubeAPIServerEndpoint)
	if err != nil {
		log.Error(err, "error getting cluster address")
		return
	}
	// populate the cache
	nodeConfigCache.apiServerEndpoint = kubeAPIServerEndpoint
	nodeConfigCache.workerIgnitionEndPoint = "https://" + clusterAddress + ":22623/config/worker"
	nodeConfigCache.credentials, err = getWICDCredentials()
	if err != nil {
		log.Error(err, "unable to get WICD service account credentials")
	}
}

// getWICDCredentials returns the CA cert and access token associated with the WICD service account
func getWICDCredentials() (*windows.Authentication, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get client from the given config")
	}

	operatorNamespaceVar := "WATCH_NAMESPACE"
	wmcoNamespace, found := os.LookupEnv(operatorNamespaceVar)
	if !found {
		return nil, errors.Errorf("operator namespace must be set in %s", operatorNamespaceVar)
	}

	secrets, err := client.CoreV1().Secrets(wmcoNamespace).List(context.TODO(), meta.ListOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "error listing secrets in namespace %s", wmcoNamespace)
	}
	tokenSecretPrefix := "windows-instance-config-daemon-token-"
	var filteredSecrets []core.Secret
	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, tokenSecretPrefix) {
			filteredSecrets = append(filteredSecrets, secret)
		}
	}
	if len(filteredSecrets) != 1 {
		return nil, fmt.Errorf("expected 1 secret with '%s' prefix, found %d", tokenSecretPrefix, len(filteredSecrets))
	}
	caCert := filteredSecrets[0].Data[core.ServiceAccountRootCAKey]
	if len(caCert) == 0 {
		return nil, errors.Errorf("WICD ServiceAccount %s data not found", core.ServiceAccountRootCAKey)
	}
	token := filteredSecrets[0].Data[core.ServiceAccountTokenKey]
	if len(token) == 0 {
		return nil, errors.Errorf("WICD ServiceAccount %s data not found", core.ServiceAccountTokenKey)
	}
	return &windows.Authentication{CaCert: caCert, Token: token}, nil
}
