package nodeconfig

import (
	ctrl "sigs.k8s.io/controller-runtime"
)

// cache holds the information of the nodeConfig that is invariant for multiple reconciliation cycles. We'll use this
// information when we don't want to get the information from the global context coming from reconciler
// but to have something at nodeConfig package locally which will be passed onto other structs. There is no need to
// invalidate this cache as of now, since the entries will be immutable.
// If someone wants to change it, they've to restart the operator which will invalidate the cache automatically.
// Note: It is ok to remove this struct in future, if we don't want to continue.
type cache struct {
	// apiServerHostname is the hostname of the infrastructure API server
	apiServerHostname string
	// apiServerInternalHostname is the hostname of the infrastructure internal API server
	apiServerInternalHostname string
}

// cache has the information related to nodeConfig that should not be changed.
var nodeConfigCache = cache{}

// init populates the cache that we need for nodeConfig
func init() {
	// init
	err := initializeNodeConfigCache()
	// check error
	if err != nil {
		// get logger
		log := ctrl.Log.
			WithName("nodeconfig").
			WithName("init")
		// log error
		log.Error(err, "unable to initialize node config cache")
	}
}

func initializeNodeConfigCache() error {
	// find endpoints
	apiServerURL, apiServerInternalURL, err := discoverKubeAPIServerEndpoints()
	if err != nil {
		return err
	}
	// set apiServerHostname value
	if nodeConfigCache.apiServerHostname, err = parseHostname(apiServerURL); err != nil {
		return err
	}
	// set apiServerInternalHostname value
	if nodeConfigCache.apiServerInternalHostname, err = parseHostname(apiServerInternalURL); err != nil {
		return err
	}
	// return no error
	return nil
}
