package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
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
		return nil, nil, fmt.Errorf("error retrieving private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to parse private key: %w", err)
	}

	return privateKey, signer.PublicKey(), nil
}

// getCloudPrivateKey returns the private key present within the cloud-private-key secret
func (tc *testContext) getCloudPrivateKey() ([]byte, error) {
	privateKeySecret, err := tc.client.K8s.CoreV1().Secrets(wmcoNamespace).Get(context.TODO(),
		secrets.PrivateKeySecret, meta.GetOptions{})
	if err != nil {
		return []byte{}, fmt.Errorf("failed to retrieve cloud private key secret: %w", err)
	}

	privateKeyBytes := privateKeySecret.Data[secrets.PrivateKeySecretKey][:]
	if privateKeyBytes == nil {
		return []byte{}, fmt.Errorf("failed to retrieve private key using cloud private key secret")
	}
	return privateKeyBytes, nil
}

// getUserDataContents returns the contents of the windows-user-data secret
func (tc *testContext) getUserDataContents() (string, error) {
	secret, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		clusterinfo.UserDataSecretName, meta.GetOptions{})
	if err != nil {
		return "", err
	}
	return string(secret.Data["userData"]), nil
}

// testUserData tests if the userData created in the 'openshift-machine-api' namespace is valid
func (tc *testContext) testUserData(t *testing.T) {
	_, pubKey, err := tc.getExpectedKeyPair()
	require.NoError(t, err, "error getting the expected public/private key pair")
	userData, err := tc.getUserDataContents()
	require.NoError(t, err, "could not retrieve userdata contents")
	assert.Contains(t, userData, string(ssh.MarshalAuthorizedKey(pubKey)), "public key not found within Windows userdata")
	t.Run("Delete the userdata secret", tc.testUserDataRegeneration)
	t.Run("Update the userdata secret with invalid data", tc.testUserDataTamper)
}

// testUserDataTamper tests if userdata reverts to previous value if updated
func (tc *testContext) testUserDataTamper(t *testing.T) {
	validUserDataSecret, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		secrets.UserDataSecret, meta.GetOptions{})
	require.NoError(t, err, "could not find Windows userData secret in required namespace")

	updatedSecret := validUserDataSecret.DeepCopy()
	updatedSecret.Data["userData"] = []byte("invalid data")
	_, err = tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Update(context.TODO(), updatedSecret,
		meta.UpdateOptions{})
	require.NoError(t, err, "could not update userData secret")

	// Updating the userdata with incorrect contents will cause the Machine nodes to be deleted and recreated, wait
	// until the Machine is back up.
	assert.NoError(t, tc.waitForNewMachineNodes(), "error waiting for Machine nodes to be reconfigured")
	assert.NoError(t, tc.waitForValidUserData(validUserDataSecret), "error waiting for valid userdata")
}

// waitForNewMachineNodes returns an error if waitForConfiguredWindowsNodes returns the same Machine backed nodes
func (tc *testContext) waitForNewMachineNodes() error {
	var oldNodes []string
	for _, node := range gc.machineNodes {
		oldNodes = append(oldNodes, node.GetName())
	}

	// waitForConfiguredWindowsNodes will re-populate gc.machineNodes with the configured nodes found
	log.Printf("waiting for existing Machine nodes to be removed and replaced")
	return wait.Poll(retryInterval, time.Minute*10, func() (done bool, err error) {
		err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, false, false)
		if err != nil {
			log.Printf("error waiting for configured Windows Nodes: %s", err)
			return false, nil
		}
		for _, newNode := range gc.machineNodes {
			for _, oldNode := range oldNodes {
				if newNode.GetName() == oldNode {
					log.Printf("node %s is not a new Node, continuing to wait", oldNode)
					return false, nil
				}
			}
			if !tc.nodeReadyAndSchedulable(newNode) {
				log.Printf("node %s is not yet ready and schedulable, continuing to wait", newNode.GetName())
				return false, nil
			}
		}
		return true, nil
	})
}

// testUserDataRegeneration tests that the userdata will be created by WMCO if deleted by a user
func (tc *testContext) testUserDataRegeneration(t *testing.T) {
	validUserDataSecret, err := tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Get(context.TODO(),
		secrets.UserDataSecret, meta.GetOptions{})
	require.NoError(t, err, "could not find Windows userData secret in required namespace")
	err = tc.client.K8s.CoreV1().Secrets(clusterinfo.MachineAPINamespace).Delete(context.TODO(),
		secrets.UserDataSecret, meta.DeleteOptions{})
	require.NoError(t, err, "could not delete userData secret")
	assert.NoError(t, tc.waitForValidUserData(validUserDataSecret))
}

// waitForValidUserData waits until the userdata secret exists with expected content
func (tc *testContext) waitForValidUserData(expected *core.Secret) error {
	return wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (done bool, err error) {
		actualData, err := tc.getUserDataContents()
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("still waiting for user data secret: %v", err)
				return false, nil
			}
			log.Printf("error getting secret: %v", err)
			return false, nil
		}
		return string(expected.Data["userData"][:]) == actualData, nil
	})
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
	// Load the state of nodes before the test begins, this is required to determine if new Machine nodes were created
	require.NoError(t, tc.loadExistingNodes())
	err = tc.createPrivateKeySecret(false)
	require.NoError(t, err, "error replacing known private key secret with a random key")
	// Ensure operator communicates to OLM that upgrade is not safe when processing secret resources
	err = tc.validateUpgradeableCondition(meta.ConditionFalse)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")

	// Ensure Machines nodes are re-created and configured using the new private key
	assert.NoError(t, tc.waitForNewMachineNodes(),
		"error waiting for Machine nodes configured with newly created private key")
	// Ensure public key hash and encrypted username annotations are updated correctly on BYOH nodes
	assert.NoError(t, tc.waitForBYOHPrivateKeyUpdate(),
		"error waiting for BYOH nodes to be updated with the newly created private key")

	err = tc.validateUpgradeableCondition(meta.ConditionTrue)
	require.NoError(t, err, "operator Upgradeable condition not in proper state")

	// revert key changes so the test suite is able to SSH into the VMs
	require.NoError(t, tc.createPrivateKeySecret(true))
	require.NoError(t, tc.waitForNewMachineNodes())
}

// waitForBYOHPrivateKeyUpdate waits until all BYOH Nodes annotations are updated to reflect the expected private key
func (tc *testContext) waitForBYOHPrivateKeyUpdate() error {
	return wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (done bool, err error) {
		nodes, err := tc.listFullyConfiguredWindowsNodes(true)
		if err != nil {
			return false, nil
		}
		for _, node := range nodes {
			if pubKeyCorrect, err := tc.checkPubKeyAnnotation(&node); err != nil || !pubKeyCorrect {
				return false, nil
			}
			if usernameCorrect, err := tc.checkUsernameAnnotation(&node); err != nil || !usernameCorrect {
				return false, nil
			}
		}
		return true, nil
	})
}

// generatePrivateKey generates a random RSA private key
func generatePrivateKey() ([]byte, error) {
	var keyData []byte
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, fmt.Errorf("error generating key: %w", err)
	}
	var privateKey = &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	buf := bytes.NewBuffer(keyData)
	err = pem.Encode(buf, privateKey)
	if err != nil {
		return nil, fmt.Errorf("error encoding generated private key: %w", err)
	}
	return buf.Bytes(), nil
}

// createPrivateKeySecret ensures that a private key secret exists with the correct data in both the operator and test
// namespaces
func (tc *testContext) createPrivateKeySecret(useKnownKey bool) error {
	if err := tc.ensurePrivateKeyDeleted(); err != nil {
		return fmt.Errorf("error ensuring any existing private key is removed: %w", err)
	}
	var keyData []byte
	var err error
	if useKnownKey {
		keyData, err = ioutil.ReadFile(gc.privateKeyPath)
		if err != nil {
			return fmt.Errorf("unable to read private key data from file %s: %w", gc.privateKeyPath, err)
		}
	} else {
		keyData, err = generatePrivateKey()
		if err != nil {
			return fmt.Errorf("error generating private key: %w", err)
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
	for _, ns := range []string{wmcoNamespace, tc.workloadNamespace} {
		_, err := tc.client.K8s.CoreV1().Secrets(ns).Create(context.TODO(), &privateKeySecret, meta.CreateOptions{})
		if err != nil {
			return fmt.Errorf("could not create private key secret in namespace %s: %w", ns, err)
		}
	}
	return nil
}

// ensurePrivateKeyDeleted ensures that the privateKeySecret is deleted in both the operator and test namespaces
func (tc *testContext) ensurePrivateKeyDeleted() error {
	for _, ns := range []string{wmcoNamespace, tc.workloadNamespace} {
		err := tc.client.K8s.CoreV1().Secrets(ns).Delete(context.TODO(), secrets.PrivateKeySecret, meta.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("could not delete private key secret in namespace %s: %w", ns, err)
		}
	}
	return nil
}
