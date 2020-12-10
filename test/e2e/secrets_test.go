package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io/ioutil"
	"log"
	"testing"
	"time"

	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
)

// createSigner creates a signer using the private key retrieved from the secret
func createSigner() (ssh.Signer, error) {
	privateKeySecret := &core.Secret{}
	err := framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "cloud-private-key", Namespace: "openshift-windows-machine-config-operator"}, privateKeySecret)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve cloud private key secret")
	}

	privateKeyBytes := privateKeySecret.Data[secrets.PrivateKeySecretKey][:]
	if privateKeyBytes == nil {
		return nil, errors.New("failed to retrieve private key using cloud private key secret")
	}

	signer, err := ssh.ParsePrivateKey(privateKeyBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse private key")
	}
	return signer, nil
}

// testUserData tests if the userData created in the 'openshift-machine-api' namespace is valid
func testUserData(t *testing.T) {
	signer, err := createSigner()
	require.NoError(t, err, "error creating signer using private key")
	pubKeyBytes := ssh.MarshalAuthorizedKey(signer.PublicKey())
	require.NotNil(t, pubKeyBytes, "failed to retrieve public key using signer for private key")
	found := &core.Secret{}
	err = framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data", Namespace: "openshift-machine-api"}, found)
	require.NoError(t, err, "could not find Windows user data secret in required namespace")
	assert.Contains(t, string(found.Data["userData"][:]), string(pubKeyBytes[:]), "expected user data not present in required namespace")
}

// testUserDataTamper tests if userData reverts to previous value if updated
func testUserDataTamper(t *testing.T) {
	secretInstance := &core.Secret{}
	validUserDataSecret, err := framework.Global.KubeClient.CoreV1().Secrets("openshift-machine-api").Get(context.TODO(), "windows-user-data", meta.GetOptions{})
	require.NoError(t, err, "could not find Windows userData secret in required namespace")

	var tests = []struct {
		name           string
		operation      string
		expectedSecret *core.Secret
	}{
		{"Update the userData secret with invalid data", "Update", validUserDataSecret},
		{"Delete the userData secret", "Delete", validUserDataSecret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.operation == "Update" {
				updatedSecret := validUserDataSecret.DeepCopy()
				updatedSecret.Data["userData"] = []byte("invalid data")
				err := framework.Global.Client.Update(context.TODO(), updatedSecret)
				require.NoError(t, err, "could not update userData secret")
			}
			if tt.operation == "Delete" {
				err := framework.Global.KubeClient.CoreV1().Secrets("openshift-machine-api").Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
				require.NoError(t, err, "could not delete userData secret")
			}

			// wait for userData secret creation / update to take effect.
			err := wait.Poll(5*time.Second, 20*time.Second, func() (done bool, err error) {
				err = framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data",
					Namespace: "openshift-machine-api"}, secretInstance)
				if err != nil {
					if apierrors.IsNotFound(err) {
						log.Printf("still waiting for user data secret: %v", err)
						return false, nil
					}
					log.Printf("error listing secrets: %v", err)
					return false, nil
				}
				if string(validUserDataSecret.Data["userData"][:]) != string(secretInstance.Data["userData"][:]) {
					return false, nil
				}
				return true, nil
			})
			require.NoError(t, err, "could not find a valid userData secret in the namespace : %v", secretInstance.Namespace)
		})
	}
}

// generatePrivateKey generates a random RSA private key
func generatePrivateKey() ([]byte, error) {
	var keyData []byte
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, errors.Wrap(err, "error generating key")
	}
	var privateKey = &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	buf := bytes.NewBuffer(keyData)
	err = pem.Encode(buf, privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding generated private key")
	}
	return buf.Bytes(), nil
}

// createPrivateKeySecret ensures that a private key secret exists with the correct data
func (tc *testContext) createPrivateKeySecret(useKnownKey bool) error {
	if err := tc.ensurePrivateKeyDeleted(); err != nil {
		return errors.Wrap(err, "error ensuring any existing private key is removed")
	}
	var keyData []byte
	var err error
	if useKnownKey {
		keyData, err = ioutil.ReadFile(gc.privateKeyPath)
		if err != nil {
			return errors.Wrapf(err, "unable to read private key data from file %s", gc.privateKeyPath)
		}
	} else {
		keyData, err = generatePrivateKey()
		if err != nil {
			return errors.Wrap(err, "error generating private key")
		}
	}

	privateKeySecret := core.Secret{
		Data: map[string][]byte{secrets.PrivateKeySecretKey: keyData},
		ObjectMeta: meta.ObjectMeta{
			Name:      secrets.PrivateKeySecret,
			Namespace: tc.namespace,
		},
	}
	_, err = tc.kubeclient.CoreV1().Secrets(tc.namespace).Create(context.TODO(), &privateKeySecret, meta.CreateOptions{})
	return err
}

// ensurePrivateKeyDeleted ensures that the privateKeySecret is deleted
func (tc *testContext) ensurePrivateKeyDeleted() error {
	secretsClient := tc.kubeclient.CoreV1().Secrets(tc.namespace)
	if _, err := secretsClient.Get(context.TODO(), secrets.PrivateKeySecret, meta.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			// secret doesnt exist, do nothing
			return nil
		}
		return errors.Wrap(err, "could not get private key secret")
	}
	// Secret exists, delete it
	return secretsClient.Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
}
