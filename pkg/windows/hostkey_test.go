package windows

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// generateTestHostKey generates a test RSA host key
func generateTestHostKey(t *testing.T) ssh.PublicKey {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err, "failed to generate RSA key")

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	require.NoError(t, err, "failed to create SSH public key")

	return publicKey
}

func TestHostKeyValidator_TOFU(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	ctx := context.Background()

	validator := NewHostKeyValidator(fakeClient, "openshift-windows-machine-config-operator", "test-node", "")

	callback, err := validator.GetHostKeyCallback(ctx)
	require.NoError(t, err, "failed to get TOFU callback")

	hostKey := generateTestHostKey(t)

	err = callback("test-host", &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}, hostKey)
	require.NoError(t, err, "TOFU callback should accept first key")

	err = validator.PersistSeenKey(ctx)
	require.NoError(t, err, "failed to persist seen key")

	var updatedNode core.Node
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-node"}, &updatedNode)
	require.NoError(t, err, "failed to get updated node")

	storedKey, exists := updatedNode.Annotations[SSHHostKeyAnnotation]
	require.True(t, exists, "host key annotation should be stored")

	storedKeyBytes, err := base64.StdEncoding.DecodeString(storedKey)
	require.NoError(t, err, "failed to decode stored key")

	parsedStoredKey, err := ssh.ParsePublicKey(storedKeyBytes)
	require.NoError(t, err, "failed to parse stored key")

	assert.Equal(t, hostKey.Marshal(), parsedStoredKey.Marshal(), "stored key should match original")
	assert.Equal(t, hostKey.Type(), updatedNode.Annotations[SSHHostKeyTypeAnnotation], "key type should match")
}

func TestHostKeyValidator_ValidateStoredKey(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))

	hostKey := generateTestHostKey(t)
	keyBytes := hostKey.Marshal()
	keyB64 := base64.StdEncoding.EncodeToString(keyBytes)

	node := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "test-node",
			Annotations: map[string]string{
				SSHHostKeyAnnotation:     keyB64,
				SSHHostKeyTypeAnnotation: hostKey.Type(),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	ctx := context.Background()

	validator := NewHostKeyValidator(fakeClient, "openshift-windows-machine-config-operator", "test-node", "")

	callback, err := validator.GetHostKeyCallback(ctx)
	require.NoError(t, err, "failed to get validation callback")

	err = callback("test-host", &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}, hostKey)
	assert.NoError(t, err, "validation should succeed with correct key")

	wrongKey := generateTestHostKey(t)
	err = callback("test-host", &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}, wrongKey)
	assert.Error(t, err, "validation should fail with wrong key")
}

func TestHostKeyValidator_ConfigMapFallback(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	ctx := context.Background()

	validator := NewHostKeyValidator(fakeClient, "openshift-windows-machine-config-operator", "", "10.1.42.1")

	callback, err := validator.GetHostKeyCallback(ctx)
	require.NoError(t, err, "failed to get TOFU callback")

	hostKey := generateTestHostKey(t)

	err = callback("test-host", &net.TCPAddr{IP: net.ParseIP("10.1.42.1"), Port: 22}, hostKey)
	require.NoError(t, err, "TOFU callback should accept first key")

	err = validator.PersistSeenKey(ctx)
	require.NoError(t, err, "failed to persist seen key")

	cm := &core.ConfigMap{}
	err = fakeClient.Get(ctx, client.ObjectKey{
		Namespace: "openshift-windows-machine-config-operator",
		Name:      SSHHostKeysConfigMap,
	}, cm)
	require.NoError(t, err, "ConfigMap should be created")

	storedKey, exists := cm.Data["10.1.42.1"]
	require.True(t, exists, "host key should be stored in ConfigMap")

	storedKeyBytes, err := base64.StdEncoding.DecodeString(storedKey)
	require.NoError(t, err, "failed to decode stored key")

	parsedStoredKey, err := ssh.ParsePublicKey(storedKeyBytes)
	require.NoError(t, err, "failed to parse stored key")

	assert.Equal(t, hostKey.Marshal(), parsedStoredKey.Marshal(), "stored key should match original")

	newValidator := NewHostKeyValidator(fakeClient, "openshift-windows-machine-config-operator", "", "10.1.42.1")
	callback2, err := newValidator.GetHostKeyCallback(ctx)
	require.NoError(t, err, "should retrieve key from ConfigMap")

	err = callback2("test-host", &net.TCPAddr{IP: net.ParseIP("10.1.42.1"), Port: 22}, hostKey)
	assert.NoError(t, err, "should validate against ConfigMap key")

	wrongKey := generateTestHostKey(t)
	err = callback2("test-host", &net.TCPAddr{IP: net.ParseIP("10.1.42.1"), Port: 22}, wrongKey)
	assert.Error(t, err, "should reject different key")
}
