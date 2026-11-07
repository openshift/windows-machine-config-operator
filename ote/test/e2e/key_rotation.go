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

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

func generateTestPrivateKeyPEM() ([]byte, error) {
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
	if node == nil || expectedHash == "" || node.Annotations[pubKeyHashAnno] != expectedHash {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func decryptWMCOUsername(ciphertext string, key []byte) (string, error) {
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

func privateKeySecretMatches(ctx context.Context, secrets corev1client.SecretInterface, secretName string, dataKey string, expectedUID types.UID,
	key []byte) error {
	secret, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if expectedUID == "" || secret.UID != expectedUID {
		return fmt.Errorf("cloud private key Secret object identity changed")
	}
	stored := secret.Data[dataKey]
	if len(stored) != len(key) || subtle.ConstantTimeCompare(stored, key) != 1 {
		return fmt.Errorf("cloud private key Secret does not contain the expected key")
	}
	return nil
}

func updatePrivateKeySecret(ctx context.Context, secrets corev1client.SecretInterface, secretName string, dataKey string, expectedUID types.UID,
	key []byte) error {
	writeAttempted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret, err := secrets.Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if expectedUID == "" || secret.UID != expectedUID {
			return fmt.Errorf("cloud private key Secret object identity changed")
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[dataKey] = append([]byte(nil), key...)
		writeAttempted = true
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		if verifyErr := privateKeySecretMatches(ctx, secrets, secretName, dataKey, expectedUID, key); verifyErr != nil {
			return fmt.Errorf("verify cloud private key Secret update: %w", verifyErr)
		}
		return nil
	}
	if writeAttempted {
		if verifyErr := privateKeySecretMatches(ctx, secrets, secretName, dataKey, expectedUID, key); verifyErr == nil {
			return nil
		} else {
			return errors.Join(fmt.Errorf("update cloud private key Secret: %w", err),
				fmt.Errorf("reconcile ambiguous cloud private key Secret update: %w", verifyErr))
		}
	}
	return fmt.Errorf("update cloud private key Secret: %w", err)
}
