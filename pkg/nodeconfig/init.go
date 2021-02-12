package nodeconfig

import (
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// cache holds the information of the nodeConfig that is invariant for multiple reconciliation cycles. We'll use this
// information when we don't want to get the information from the global context coming from reconciler
// but to have something at nodeConfig package locally which will be passed onto other structs. There is no need to
// invalidate this cache as of now, since the only entry in this workerIgnitionEndPoint which will be immutable. If
// someone wants to change it, they've to restart the operator which will invalidate the cache automatically.
// Note : It is ok to remove this struct in future, if we don't want to continue. As of now, I can think of only
// 		  worker ignition endpoint being part of this struct.
type cache struct {
	// workerIgnitionEndpoint is the Machine Config Server(MCS) endpoint from which we can download the
	// the OpenShift worker ignition file.
	workerIgnitionEndPoint string
}

var log = logf.Log.WithName("nodeconfig")

// cache has the information related to nodeConfig that should not be changed.
var nodeConfigCache = cache{}

// init populates the cache that we need for nodeConfig
func init() {
	var kubeAPIServerEndpoint string
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
	nodeConfigCache.workerIgnitionEndPoint = "https://" + clusterAddress + ":22623/config/worker"
}
