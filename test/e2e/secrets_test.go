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

	config "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

// getExpectedKeyPair returns the private key within the cloud-private-key secret and the associated public key
func (tc *testContext) getExpectedKeyPair() ([]byte, ssh.PublicKey, error) {
	privateKey, err := tc.getCloudPrivateKey()
	if err != nil {
		return nil, nil, errors.Wrap(err, "error retrieving private key")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "unable to parse private key")
	}

	return privateKey, signer.PublicKey(), nil
}

// getCloudPrivateKey returns the private key present within the cloud-private-key secret
func (tc *testContext) getCloudPrivateKey() ([]byte, error) {
	privateKeySecret, err := tc.client.K8s.CoreV1().Secrets("openshift-windows-machine-config-operator").Get(context.TODO(),
		secrets.PrivateKeySecret, meta.GetOptions{})
	if err != nil {
		return []byte{}, errors.Wrapf(err, "failed to retrieve cloud private key secret")
	}

	privateKeyBytes := privateKeySecret.Data[secrets.PrivateKeySecretKey][:]
	if privateKeyBytes == nil {
		return []byte{}, errors.New("failed to retrieve private key using cloud private key secret")
	}
	return privateKeyBytes, nil
}

// getUserDataContents returns the contents of the windows-user-data secret
func (tc *testContext) getUserDataContents() (string, error) {
	secret, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(), "windows-user-data", meta.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data["userData"]), nil
}

// testUserData tests if the userData created in the 'openshift-machine-api' namespace is valid
func testUserData(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	_, pubKey, err := tc.getExpectedKeyPair()
	require.NoError(t, err, "error getting the expected public/private key pair")
	userData, err := tc.getUserDataContents()
	require.NoError(t, err, "could not retrieve userdata contents")
	assert.Contains(t, userData, string(ssh.MarshalAuthorizedKey(pubKey)), "public key not found within Windows userdata")
}

// testUserDataTamper tests if userData reverts to previous value if updated
func testUserDataTamper(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	secretInstance := &core.Secret{}
	validUserDataSecret, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(), "windows-user-data", meta.GetOptions{})
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
				_, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Update(context.TODO(), updatedSecret,
					meta.UpdateOptions{})
				require.NoError(t, err, "could not update userData secret")
			}
			if tt.operation == "Delete" {
				err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
				require.NoError(t, err, "could not delete userData secret")
			}

			// wait for userData secret creation / update to take effect.
			err := wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (done bool, err error) {
				secretInstance, err = tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(), "windows-user-data", meta.GetOptions{})
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

// testPrivateKeyChange alters the private key used to SSH into instances and ensures nodes are updated properly
func testPrivateKeyChange(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	// This test cannot be run on vSphere because this random key is not part of the vSphere template image.
	// Moreover this test is platform agnostic so is not needed to be run for every supported platform.
	if tc.CloudProvider.GetType() == config.VSpherePlatformType {
		t.Skipf("Skipping for %s", config.VSpherePlatformType)
	}
	err = tc.createPrivateKeySecret(false)
	require.NoError(t, err, "error replacing known private key secret with a random key")

	// Ensure Machines nodes are re-created and configured using the new private key
	err = tc.waitForWindowsNodes(gc.numberOfMachineNodes, false, false, false)
	assert.NoError(t, err, "error waiting for Windows nodes configured with newly created private key")
	// Ensure public key hash and encrypted username annotations are updated correctly on BYOH nodes
	err = tc.waitForWindowsNodes(gc.numberOfBYOHNodes, false, false, true)
	assert.NoError(t, err, "error waiting for Windows nodes updated with newly created private key")

	// Re-create the known private key so SSH connection can be re-established
	// TODO: Remove dependency on this secret by rotating keys as part of https://issues.redhat.com/browse/WINC-655
	require.NoError(t, tc.createPrivateKeySecret(true), "error confirming known private key secret exists")
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

// createPrivateKeySecret ensures that a private key secret exists with the correct data in both the operator and test
// namespaces
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
			Name: secrets.PrivateKeySecret,
		},
	}

	// Create the private key secret in both the operator's namespace, and the test namespace. This is needed to make it
	// possible to SSH into the Windows nodes from pods spun up in the test namespace.
	for _, ns := range []string{tc.namespace, tc.workloadNamespace} {
		_, err := tc.client.K8s.CoreV1().Secrets(ns).Create(context.TODO(), &privateKeySecret, meta.CreateOptions{})
		if err != nil {
			return errors.Wrapf(err, "could not create private key secret in namespace %s", ns)
		}
	}
	return nil
}

// ensurePrivateKeyDeleted ensures that the privateKeySecret is deleted in both the operator and test namespaces
func (tc *testContext) ensurePrivateKeyDeleted() error {
	for _, ns := range []string{tc.namespace, tc.workloadNamespace} {
		err := tc.client.K8s.CoreV1().Secrets(ns).Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrapf(err, "could not delete private key secret in namespace %s", ns)
		}
	}
	return nil
}
