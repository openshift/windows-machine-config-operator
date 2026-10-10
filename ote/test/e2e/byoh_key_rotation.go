package winc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

// These key helpers are adapted from the public, still-unmerged PR 4614. They intentionally
// contain no MachineSet discovery or scaling behavior.
func generateTestPrivateKey() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
}

func publicKeyHash(privateKey []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	authorizedKey := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(authorizedKey))), nil
}

func nodeReadyWithKeyHash(node *corev1.Node, expectedHash string) bool {
	if node == nil || expectedHash == "" || node.Annotations[byohPublicKeyHashAnno] != expectedHash {
		return false
	}
	return nodeReadyAndConverged(node)
}

func leaseNodeKeyAndUsernameConverged(ctx context.Context, nodes corev1client.NodeInterface,
	lease *byohHostLease, expectedHash string, key []byte, previousCiphertext string,
	requireCiphertextChange bool) (bool, error) {
	if lease == nil || lease.NodeName == "" || lease.NodeUID == "" || expectedHash == "" || len(key) == 0 {
		return false, nil
	}
	node, err := nodes.Get(ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if node.UID != lease.NodeUID || !nodeReadyWithKeyHash(node, expectedHash) {
		return false, nil
	}
	ciphertext := node.Annotations[byohUsernameAnno]
	if ciphertext == "" || (requireCiphertextChange && ciphertext == previousCiphertext) {
		return false, nil
	}
	username, err := decryptBYOHUsername(ciphertext, key)
	return err == nil && username == lease.Username, nil
}

func decryptBYOHUsername(ciphertext string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("decryption passphrase cannot be empty")
	}
	ciphertext = strings.ReplaceAll(ciphertext, "<wmcoMarker>", "\n")
	const startTag = "-----BEGIN ENCRYPTED DATA-----"
	const endTag = "-----END ENCRYPTED DATA-----"
	if !strings.HasPrefix(ciphertext, startTag) {
		ciphertext = startTag + "\n\n" + ciphertext + "\n" + endTag
	}
	block, err := armor.Decode(bytes.NewBufferString(ciphertext))
	if err != nil {
		return "", fmt.Errorf("decode encrypted username")
	}
	attempted := false
	message, err := openpgp.ReadMessage(block.Body, nil, func(_ []openpgp.Key, _ bool) ([]byte, error) {
		if attempted {
			return nil, fmt.Errorf("invalid passphrase")
		}
		attempted = true
		return key, nil
	}, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt username annotation")
	}
	plaintext, err := io.ReadAll(message.UnverifiedBody)
	if err != nil {
		return "", fmt.Errorf("read decrypted username")
	}
	return string(plaintext), nil
}

func privateKeySecretMatches(ctx context.Context, secrets corev1client.SecretInterface, expectedUID types.UID,
	key []byte) error {
	secret, err := secrets.Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if expectedUID == "" || secret.UID != expectedUID {
		return fmt.Errorf("cloud private key Secret object identity changed")
	}
	stored := secret.Data[byohPrivateKeyDataKey]
	if len(stored) != len(key) || subtle.ConstantTimeCompare(stored, key) != 1 {
		return fmt.Errorf("cloud private key Secret does not contain the expected key")
	}
	return nil
}

func updatePrivateKeySecret(ctx context.Context, secrets corev1client.SecretInterface, expectedUID types.UID,
	key []byte) error {
	writeAttempted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret, err := secrets.Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if expectedUID == "" || secret.UID != expectedUID {
			return fmt.Errorf("cloud private key Secret object identity changed")
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[byohPrivateKeyDataKey] = append([]byte(nil), key...)
		writeAttempted = true
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		if verifyErr := privateKeySecretMatches(ctx, secrets, expectedUID, key); verifyErr != nil {
			return fmt.Errorf("verify cloud private key Secret update: %w", verifyErr)
		}
		return nil
	}
	if writeAttempted {
		if verifyErr := privateKeySecretMatches(ctx, secrets, expectedUID, key); verifyErr == nil {
			return nil
		} else {
			return errors.Join(fmt.Errorf("update cloud private key Secret: %w", err),
				fmt.Errorf("reconcile ambiguous cloud private key Secret update: %w", verifyErr))
		}
	}
	return fmt.Errorf("update cloud private key Secret: %w", err)
}

func (f *byohTestFixture) rotateBYOHPrivateKey(lease *byohHostLease) error {
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(f.ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("snapshot cloud private key Secret: %w", err)
	}
	originalKey := secret.Data[byohPrivateKeyDataKey]
	if len(originalKey) == 0 {
		return fmt.Errorf("cloud private key Secret has no private key")
	}
	if secret.UID == "" {
		return fmt.Errorf("cloud private key Secret has no object identity")
	}
	node, err := f.coreClient.Nodes().Get(f.ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	originalCiphertext := node.Annotations[byohUsernameAnno]
	if originalCiphertext == "" {
		return fmt.Errorf("selected BYOH Node has no encrypted username annotation")
	}
	originalHash := node.Annotations[byohPublicKeyHashAnno]
	expectedOriginalHash, err := publicKeyHash(originalKey)
	if err != nil {
		return err
	}
	if node.UID != lease.NodeUID || originalHash == "" || originalHash != expectedOriginalHash ||
		!nodeReadyWithKeyHash(node, expectedOriginalHash) {
		return fmt.Errorf("selected BYOH Node does not have the expected original public key hash")
	}
	f.secretSnapshot, f.secretLease, f.secretRestored = secret.DeepCopy(), lease, false
	f.originalKeyHash = originalHash
	replacementKey, err := generateTestPrivateKey()
	if err != nil {
		return err
	}
	replacementHash, err := publicKeyHash(replacementKey)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(replacementKey)
	if err != nil {
		return err
	}
	authorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	// Retain the exact generated line before the mutation attempt. Cleanup can safely remove this
	// one owned line idempotently even when the HostProcess response is lost.
	f.replacementAuthorizedKey = authorizedKey
	if err := f.placeAuthorizedKeyWithHostProcess(f.ctx, lease, authorizedKey); err != nil {
		return fmt.Errorf("place replacement authorized key through owned HostProcess probe: %w", err)
	}
	if err := updatePrivateKeySecret(f.ctx, f.coreClient.Secrets(wmcoNamespace), f.secretSnapshot.UID,
		replacementKey); err != nil {
		return err
	}
	err = f.waitForKeyConvergence(f.ctx, lease, replacementHash, replacementKey, originalCiphertext, true)
	if err != nil {
		return fmt.Errorf("replacement key annotations did not converge: %w", err)
	}
	if err := f.verifyLeaseSSHAuthentication(f.ctx, lease, replacementKey); err != nil {
		return err
	}
	return f.assertConfiguredServicesRunning(f.ctx, lease)
}

func (f *byohTestFixture) restoreBYOHPrivateKeyWithContext(ctx context.Context) error {
	if f.secretSnapshot == nil || f.secretRestored {
		return nil
	}
	originalKey := f.secretSnapshot.Data[byohPrivateKeyDataKey]
	if len(originalKey) == 0 || f.secretLease == nil {
		return fmt.Errorf("cloud private key restoration snapshot is incomplete")
	}
	originalHash, err := publicKeyHash(originalKey)
	if err != nil {
		return err
	}
	if f.originalKeyHash == "" || originalHash != f.originalKeyHash {
		return fmt.Errorf("cloud private key restoration hash does not match the captured original hash")
	}
	if err := updatePrivateKeySecret(ctx, f.coreClient.Secrets(wmcoNamespace), f.secretSnapshot.UID,
		originalKey); err != nil {
		return err
	}
	err = f.waitForKeyConvergence(ctx, f.secretLease, originalHash, originalKey, "", false)
	if err != nil {
		return fmt.Errorf("original key annotations did not converge: %w", err)
	}
	if err := f.verifyLeaseSSHAuthentication(ctx, f.secretLease, originalKey); err != nil {
		return err
	}
	if f.replacementAuthorizedKey != "" {
		if err := f.removeAuthorizedKeyWithHostProcess(ctx, f.secretLease, f.replacementAuthorizedKey); err != nil {
			return fmt.Errorf("remove replacement authorized key through owned HostProcess probe: %w", err)
		}
		f.replacementAuthorizedKey = ""
	}
	f.secretRestored = true
	return nil
}

func (f *byohTestFixture) waitForKeyConvergence(ctx context.Context, lease *byohHostLease,
	expectedHash string, key []byte, previousCiphertext string, requireCiphertextChange bool) error {
	if f.keyConvergenceForTest != nil {
		return f.keyConvergenceForTest(ctx, lease, expectedHash, key, previousCiphertext, requireCiphertextChange)
	}
	return wait.PollUntilContextTimeout(ctx, 15*time.Second, 15*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			return leaseNodeKeyAndUsernameConverged(ctx, f.coreClient.Nodes(), lease, expectedHash, key,
				previousCiphertext, requireCiphertextChange)
		})
}

func (f *byohTestFixture) verifyLeaseSSHAuthentication(ctx context.Context, lease *byohHostLease,
	privateKey []byte) error {
	if f.verifySSHForTest != nil {
		return f.verifySSHForTest(ctx, lease, privateKey)
	}
	return verifySSHAuthentication(ctx, lease, privateKey)
}

func (f *byohTestFixture) restoreBYOHPrivateKey() error {
	return f.restoreBYOHPrivateKeyWithContext(f.ctx)
}
