package windows

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

const (
	// SSHHostKeyAnnotation stores the trusted SSH host public key for the Windows instance
	SSHHostKeyAnnotation = "windowsmachineconfig.openshift.io/ssh-host-key"
	// SSHHostKeyTypeAnnotation stores the SSH host key algorithm type (e.g., ssh-rsa, ssh-ed25519)
	SSHHostKeyTypeAnnotation = "windowsmachineconfig.openshift.io/ssh-host-key-type"
	// SSHHostKeysConfigMap stores host keys by address before Node exists
	SSHHostKeysConfigMap = "windows-ssh-host-keys"
)

// hostKeyStore persists SSH host keys in cluster storage
type hostKeyStore interface {
	GetHostKey(ctx context.Context, address string) (ssh.PublicKey, error)
	StoreHostKey(ctx context.Context, address string, key ssh.PublicKey) error
}

// nodeAnnotationHostKeyStore stores host keys in Node annotations or ConfigMap
type nodeAnnotationHostKeyStore struct {
	namespace string
	client    client.Client
	nodeName  string
	address   string
	log       logr.Logger
}

// HostKeyValidator validates SSH host keys using TOFU (Trust On First Use) pinning
type HostKeyValidator struct {
	store   hostKeyStore
	seenKey ssh.PublicKey
}

// NewHostKeyValidator creates a new HostKeyValidator
func NewHostKeyValidator(c client.Client, namespace, nodeName, address string) *HostKeyValidator {
	var store hostKeyStore
	// Only create store if namespace is available (required for ConfigMap operations and cleanup)
	if c != nil && namespace != "" && (nodeName != "" || address != "") {
		log := ctrl.Log.WithName("hostkey")
		if nodeName != "" {
			log = log.WithValues("node", nodeName)
		}
		if address != "" {
			log = log.WithValues("address", address)
		}
		store = &nodeAnnotationHostKeyStore{
			client:    c,
			nodeName:  nodeName,
			address:   address,
			namespace: namespace,
			log:       log,
		}
	}
	return &HostKeyValidator{
		store: store,
	}
}

// GetHostKeyCallback returns an ssh.HostKeyCallback that implements TOFU host key pinning
func (v *HostKeyValidator) GetHostKeyCallback(ctx context.Context) (ssh.HostKeyCallback, error) {
	if v.seenKey != nil {
		return v.validateCallback(base64.StdEncoding.EncodeToString(v.seenKey.Marshal()))
	}

	if v.store == nil {
		return v.tofuCallback(), nil
	}

	storedKey, err := v.store.GetHostKey(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get stored host key: %w", err)
	}
	if storedKey == nil {
		return v.tofuCallback(), nil
	}

	return v.validateCallback(base64.StdEncoding.EncodeToString(storedKey.Marshal()))
}

// tofuCallback returns a callback that trusts the first key seen
func (v *HostKeyValidator) tofuCallback() ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if v.seenKey == nil {
			v.seenKey = key
			return nil
		}
		if !bytes.Equal(v.seenKey.Marshal(), key.Marshal()) {
			return fmt.Errorf("host key changed during connection attempts")
		}
		return nil
	}
}

// PersistSeenKey persists the in-memory seenKey to cluster storage after successful SSH connection
func (v *HostKeyValidator) PersistSeenKey(ctx context.Context) error {
	if v.seenKey == nil || v.store == nil {
		return nil
	}
	return v.store.StoreHostKey(ctx, "", v.seenKey)
}

// GetStore returns the underlying hostKeyStore for direct access
func (v *HostKeyValidator) GetStore() hostKeyStore {
	return v.store
}

// RemoveHostKey removes the stored host key to prevent stale keys blocking IP reuse
func (v *HostKeyValidator) RemoveHostKey(ctx context.Context) error {
	if v.store == nil {
		return nil
	}
	store, ok := v.store.(*nodeAnnotationHostKeyStore)
	if !ok {
		return nil
	}
	if store.address == "" {
		return nil
	}
	return store.cleanupConfigMapEntry(ctx, store.address)
}

// validateCallback returns a callback that validates against the stored key
func (v *HostKeyValidator) validateCallback(storedKeyB64 string) (ssh.HostKeyCallback, error) {
	storedKeyBytes, err := base64.StdEncoding.DecodeString(storedKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid stored host key format: %w", err)
	}

	storedKey, err := ssh.ParsePublicKey(storedKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse stored host key: %w", err)
	}

	return ssh.FixedHostKey(storedKey), nil
}

// GetHostKey retrieves the stored host key from Node annotation or ConfigMap
func (s *nodeAnnotationHostKeyStore) GetHostKey(ctx context.Context, address string) (ssh.PublicKey, error) {
	if s.nodeName != "" {
		key, err := s.getKeyFromNode(ctx)
		if err != nil {
			return nil, err
		}
		if key != nil {
			return key, nil
		}
	}

	lookupAddress := address
	if lookupAddress == "" {
		lookupAddress = s.address
	}
	if lookupAddress != "" {
		return s.getKeyFromConfigMap(ctx, lookupAddress)
	}

	return nil, nil
}

// getKeyFromNode retrieves the SSH host key from the Node annotation
func (s *nodeAnnotationHostKeyStore) getKeyFromNode(ctx context.Context) (ssh.PublicKey, error) {
	node := &core.Node{}
	err := s.client.Get(ctx, types.NamespacedName{Name: s.nodeName}, node)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", s.nodeName, err)
	}

	storedKeyB64, hasKey := node.GetAnnotations()[SSHHostKeyAnnotation]
	if !hasKey {
		return nil, nil
	}

	storedKeyBytes, err := base64.StdEncoding.DecodeString(storedKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid stored host key format: %w", err)
	}

	return ssh.ParsePublicKey(storedKeyBytes)
}

// getKeyFromConfigMap retrieves the SSH host key from the ConfigMap by address
// Returns nil if namespace is not set (graceful degradation to session-only TOFU)
func (s *nodeAnnotationHostKeyStore) getKeyFromConfigMap(ctx context.Context, address string) (ssh.PublicKey, error) {
	if s.namespace == "" {
		return nil, nil
	}

	cm := &core.ConfigMap{}
	err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: SSHHostKeysConfigMap}, cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap: %w", err)
	}

	storedKeyB64, exists := cm.Data[address]
	if !exists {
		return nil, nil
	}

	storedKeyBytes, err := base64.StdEncoding.DecodeString(storedKeyB64)
	if err != nil {
		return nil, fmt.Errorf("invalid stored host key format in ConfigMap: %w", err)
	}

	return ssh.ParsePublicKey(storedKeyBytes)
}

// StoreHostKey stores the SSH host key in Node annotation or ConfigMap
func (s *nodeAnnotationHostKeyStore) StoreHostKey(ctx context.Context, address string, key ssh.PublicKey) error {
	if s.nodeName != "" {
		err := s.storeKeyToNode(ctx, key)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			// Node not found, fall through to ConfigMap storage
		} else {
			// Successfully stored to Node annotation, cleanup ConfigMap
			cleanupAddress := address
			if cleanupAddress == "" {
				cleanupAddress = s.address
			}
			if cleanupAddress != "" {
				if err := s.cleanupConfigMapEntry(ctx, cleanupAddress); err != nil {
					s.log.Error(err, "failed to cleanup ConfigMap entry, key remains but will not be used", "address", cleanupAddress)
				}
			}
			return nil
		}
	}

	storeAddress := address
	if storeAddress == "" {
		storeAddress = s.address
	}
	if storeAddress == "" {
		return fmt.Errorf("cannot store host key: no node name or address available")
	}

	return s.storeKeyToConfigMap(ctx, storeAddress, key)
}

// storeKeyToNode stores the SSH host key as a Node annotation with retry on conflicts
func (s *nodeAnnotationHostKeyStore) storeKeyToNode(ctx context.Context, key ssh.PublicKey) error {
	keyB64 := base64.StdEncoding.EncodeToString(key.Marshal())

	err := wait.PollImmediate(retry.Interval, retry.Timeout, func() (bool, error) {
		node := &core.Node{}
		if err := s.client.Get(ctx, types.NamespacedName{Name: s.nodeName}, node); err != nil {
			return false, err
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations[SSHHostKeyAnnotation] = keyB64
		node.Annotations[SSHHostKeyTypeAnnotation] = key.Type()

		if err := s.client.Update(ctx, node); err != nil {
			if apierrors.IsConflict(err) {
				// Retry on conflict
				return false, nil
			}
			return false, err
		}

		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to store host key annotation: %w", err)
	}

	return nil
}

// storeKeyToConfigMap stores the SSH host key in the ConfigMap by address using JSON Patch
// Creates the ConfigMap if it doesn't exist
func (s *nodeAnnotationHostKeyStore) storeKeyToConfigMap(ctx context.Context, address string, key ssh.PublicKey) error {
	if s.namespace == "" {
		return fmt.Errorf("namespace not set, cannot store host key")
	}

	keyB64 := base64.StdEncoding.EncodeToString(key.Marshal())
	cm := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      SSHHostKeysConfigMap,
			Namespace: s.namespace,
		},
	}

	patchData := fmt.Sprintf(`{"data":{"%s":"%s"}}`, address, keyB64)
	err := s.client.Patch(ctx, cm, client.RawPatch(types.StrategicMergePatchType, []byte(patchData)))
	if err == nil {
		return nil
	}

	if apierrors.IsNotFound(err) {
		cm = &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Name:      SSHHostKeysConfigMap,
				Namespace: s.namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "windows-machine-config-operator",
				},
			},
			Data: map[string]string{address: keyB64},
		}
		if err := s.client.Create(ctx, cm); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create ConfigMap: %w", err)
			}
			// ConfigMap was created concurrently, retry patch
			patchErr := s.client.Patch(ctx, cm, client.RawPatch(types.StrategicMergePatchType, []byte(patchData)))
			if patchErr != nil {
				return fmt.Errorf("failed to patch ConfigMap after concurrent creation: %w", patchErr)
			}
		}
		return nil
	}

	return fmt.Errorf("failed to patch ConfigMap: %w", err)
}

// cleanupConfigMapEntry removes the ConfigMap entry for the given address with retry on conflicts
func (s *nodeAnnotationHostKeyStore) cleanupConfigMapEntry(ctx context.Context, address string) error {
	if s.namespace == "" {
		return nil
	}

	err := wait.PollImmediate(retry.Interval, retry.Timeout, func() (bool, error) {
		cm := &core.ConfigMap{}
		if err := s.client.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: SSHHostKeysConfigMap}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("failed to get ConfigMap for cleanup: %w", err)
		}

		if _, exists := cm.Data[address]; !exists {
			return true, nil
		}

		delete(cm.Data, address)
		if err := s.client.Update(ctx, cm); err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, fmt.Errorf("failed to update ConfigMap for cleanup: %w", err)
		}

		return true, nil
	})

	if err != nil {
		return fmt.Errorf("failed to cleanup ConfigMap entry: %w", err)
	}

	s.log.V(1).Info("cleaned up ConfigMap entry", "address", address)
	return nil
}
