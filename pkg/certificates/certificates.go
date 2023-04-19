package certificates

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// CABundleKey is the key in the Kube API Server CA ConfigMap where the CA bundle is located
	CABundleKey = "ca-bundle.crt"
	// KubeApiServerOperatorNamespace is the namespace of the ConfigMap that contains the CA for the kubelet
	// to recognize the kube-apiserver client certificate.
	KubeApiServerOperatorNamespace = "openshift-kube-apiserver-operator"
	// KubeAPIServerServingCAConfigMapName is the name of the ConfigMap that contains the CA for the kubelet to
	// recognize the kube-apiserver client certificate.
	KubeAPIServerServingCAConfigMapName = "kube-apiserver-to-kubelet-client-ca"

	// configNamespace is the namespace that contains the initial kubelet CA ConfigMap
	configNamespace = "openshift-config"
	// kubeAPIServerInitialCAConfigMapName is the name of the ConfigMap that contains the initial CA certificates
	// created during the cluster installation, where the initial kubelet CA is valid only for the first year.
	kubeAPIServerInitialCAConfigMapName = "initial-kube-apiserver-server-ca"
)

// MergeCAsConfigMaps merges the given CA ConfigMaps for the specified subject
func MergeCAsConfigMaps(initialCAConfigMap, currentCAConfigMap *core.ConfigMap, subject string) ([]byte, error) {
	if subject == "" {
		return nil, errors.New("subject cannot be empty")
	}
	// extract CAs from initial CA ConfigMap
	initialCABytes, err := GetCAsFromConfigMap(initialCAConfigMap, CABundleKey)
	if err != nil {
		return nil, err
	}
	if currentCAConfigMap == nil {
		return initialCABytes, nil
	}
	// extract CAs from new bundle CA ConfigMap
	bundleCABytes, err := GetCAsFromConfigMap(currentCAConfigMap, CABundleKey)
	if err != nil {
		return nil, err
	}
	// check bundle length
	if len(bundleCABytes) == 0 {
		// this is an odd edge case
		return nil, errors.New("CA bundle cannot be empty")
	}
	// merge initial with new bundle
	bundleCABytes = mergeCABundles(initialCABytes, bundleCABytes, subject)
	// return merged CA bytes
	return bundleCABytes, nil
}

// GetCAsFromConfigMap extracts the given key from the ConfigMap object
// Adapted from https://github.com/openshift/machine-config-operator/blob/1a9f70f333a2287c4a8f2e75cb37b94c7c7b2a20/pkg/operator/sync.go#L874
func GetCAsFromConfigMap(configMap *core.ConfigMap, key string) ([]byte, error) {
	if configMap == nil {
		return nil, errors.New("configMap cannot be nil")
	}
	if key == "" {
		return nil, errors.New("key cannot be empty")
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

// GetInitialCAConfigMap returns the ConfigMap that contains the initial CA certificates
func GetInitialCAConfigMap(ctx context.Context, client client.Client) (*core.ConfigMap, error) {
	if client == nil {
		return nil, errors.New("client cannot be nil")
	}
	if ctx == nil {
		ctx = context.TODO()
	}
	initialCAConfigMap := &core.ConfigMap{}
	if err := client.Get(ctx, kubeTypes.NamespacedName{Name: kubeAPIServerInitialCAConfigMapName,
		Namespace: configNamespace}, initialCAConfigMap); err != nil {
		return nil, errors.Wrapf(err, "error getting initial CA ConfigMap %s/%s",
			configNamespace, kubeAPIServerInitialCAConfigMapName)
	}
	return initialCAConfigMap, nil
}

// mergeCABundles merges the content of the source bundle with new bundle if the specified subject is found in
// any of the source certificates
// Adapted from https://github.com/openshift/machine-config-operator/blob/1a9f70f333a2287c4a8f2e75cb37b94c7c7b2a20/pkg/operator/sync.go#L939
func mergeCABundles(sourceBundle, newBundle []byte, subjectLike string) []byte {
	mergedBytes := []byte{}
	for len(sourceBundle) > 0 {
		b, next := pem.Decode(sourceBundle)
		if b == nil {
			break
		}
		// get next portion
		sourceBundle = next
		// parse certificate
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			// skip invalid block
			continue
		}
		if strings.Contains(c.Subject.String(), subjectLike) {
			// merge and replace this cert with the new one
			mergedBytes = append(mergedBytes, newBundle...)
		} else {
			// merge the original cert
			mergedBytes = append(mergedBytes, pem.EncodeToMemory(b)...)
		}
	}
	return mergedBytes
}
