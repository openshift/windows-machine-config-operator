package certificates

import (
	"encoding/base64"
	"fmt"

	core "k8s.io/api/core/v1"
)

const (
	// CABundleKey is the key in the Kube API Server CA and trusted CA ConfigMaps where the CA bundle is located
	CABundleKey = "ca-bundle.crt"
	// KubeApiServerOperatorNamespace is the namespace of the ConfigMap that contains the CA for the kubelet
	// to recognize the kube-apiserver client certificate.
	KubeApiServerOperatorNamespace = "openshift-kube-apiserver-operator"
	// KubeAPIServerServingCAConfigMapName is the name of the ConfigMap that contains the CA for the kubelet to
	// recognize the kube-apiserver client certificate.
	KubeAPIServerServingCAConfigMapName = "kube-apiserver-to-kubelet-client-ca"

	// ProxyCertsConfigMap is the name of the ConfigMap that holds the trusted CA bundle for a cluster-wide proxy
	ProxyCertsConfigMap = "trusted-ca"
)

// GetCAsFromConfigMap extracts the given key from the ConfigMap object
// Adapted from https://github.com/openshift/machine-config-operator/blob/1a9f70f333a2287c4a8f2e75cb37b94c7c7b2a20/pkg/operator/sync.go#L874
func GetCAsFromConfigMap(configMap *core.ConfigMap, key string) ([]byte, error) {
	if configMap == nil {
		return nil, fmt.Errorf("configMap cannot be nil")
	}
	if key == "" {
		return nil, fmt.Errorf("key cannot be empty")
	}
	if bd, bdok := configMap.BinaryData[key]; bdok {
		return bd, nil
	} else if d, dok := configMap.Data[key]; dok {
		raw, err := base64.StdEncoding.DecodeString(d)
		if err != nil {
			// this is actually the result of a bad assumption.  configmap values are not encoded.
			// After the installer pull merges, this entire attempt to decode can go away.
			return []byte(d), nil
		}
		return raw, nil
	} else {
		return nil, fmt.Errorf("%s not found in %s/%s", key, configMap.Namespace, configMap.Name)
	}
}
