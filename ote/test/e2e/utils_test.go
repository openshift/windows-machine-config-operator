package winc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSafePollErrorReason(t *testing.T) {
	const sensitiveValue = "https://api.example.invalid/nodes/secret-node?token=private"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "wrapped timeout",
			err:  fmt.Errorf("list nodes: %w", context.DeadlineExceeded),
			want: "request timed out",
		},
		{
			name: "wrapped cancellation",
			err:  fmt.Errorf("list nodes: %w", context.Canceled),
			want: "request canceled",
		},
		{
			name: "unknown API error",
			err:  apierrors.NewInternalError(errors.New(sensitiveValue)),
			want: "API request failed",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := safePollErrorReason(test.err)
			if got != test.want {
				t.Fatalf("safePollErrorReason() = %q, want %q", got, test.want)
			}
			if strings.Contains(got, sensitiveValue) {
				t.Fatalf("safePollErrorReason() exposed sensitive error content: %q", got)
			}
		})
	}
}

func TestPublicKeyHashFromPrivateKey(t *testing.T) {
	t.Run("canonical hash", func(t *testing.T) {
		privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
		privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
		if err != nil {
			t.Fatalf("failed to marshal test private key: %v", err)
		}
		privateKeyPath := filepath.Join(t.TempDir(), "id_ed25519")
		if err := os.WriteFile(privateKeyPath, pem.EncodeToMemory(&pem.Block{
			Type: "PRIVATE KEY", Bytes: privateKeyDER,
		}), 0600); err != nil {
			t.Fatalf("failed to write test private key: %v", err)
		}

		got, err := publicKeyHashFromPrivateKey(privateKeyPath)
		if err != nil {
			t.Fatalf("publicKeyHashFromPrivateKey() returned an error: %v", err)
		}
		const want = "13e92e91a8938a3d10a228e38f943d340faf2cf552586d0d88ff21baebabe261"
		if got != want {
			t.Fatalf("publicKeyHashFromPrivateKey() = %q, want %q", got, want)
		}
		keyBytes, err := os.ReadFile(privateKeyPath)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := publicKeyHash(keyBytes); err != nil || got != want {
			t.Fatalf("publicKeyHash() = %q, %v, want %q", got, err, want)
		}
	})

	t.Run("malformed key", func(t *testing.T) {
		privateKeyPath := filepath.Join(t.TempDir(), "malformed-key")
		if err := os.WriteFile(privateKeyPath, []byte("malformed test key"), 0600); err != nil {
			t.Fatalf("failed to write malformed test key: %v", err)
		}
		if _, err := publicKeyHashFromPrivateKey(privateKeyPath); err == nil {
			t.Fatal("publicKeyHashFromPrivateKey() returned no error for malformed key")
		}
	})

	t.Run("unreadable key", func(t *testing.T) {
		privateKeyPath := filepath.Join(t.TempDir(), "missing-key")
		if _, err := publicKeyHashFromPrivateKey(privateKeyPath); err == nil {
			t.Fatal("publicKeyHashFromPrivateKey() returned no error for unreadable key")
		}
	})
}

func TestGenerateTestPrivateKeyPathWrapper(t *testing.T) {
	checkKey := func(keyBytes []byte) {
		t.Helper()
		block, rest := pem.Decode(keyBytes)
		if block == nil || block.Type != "RSA PRIVATE KEY" || len(rest) != 0 {
			t.Fatal("generated key is not a single PKCS#1 PEM block")
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if key.N.BitLen() != 2048 {
			t.Fatal("generated RSA key is not 2048 bits")
		}
	}
	path, err := generateTestPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil {
			t.Error(err)
		}
	})
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions = %o, want 600", info.Mode().Perm())
	}
	keyBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkKey(keyBytes)
	byteHash, err := publicKeyHash(keyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if pathHash, err := publicKeyHashFromPrivateKey(path); err != nil || pathHash != byteHash {
		t.Fatalf("path and byte hashes differ: error=%v", err)
	}
}

func TestTargetNodesReadyWithKeyHash(t *testing.T) {
	const expectedHash = "replacement-key-hash"

	t.Run("multiple nodes ready", func(t *testing.T) {
		firstNode := newNodeWithKeyHash(expectedHash,
			corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})
		firstNode.Name = "first-node"
		secondNode := newNodeWithKeyHash(expectedHash,
			corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})
		secondNode.Name = "second-node"
		nodeClient := fake.NewSimpleClientset(firstNode, secondNode).CoreV1().Nodes()

		ready, err := targetNodesReadyWithKeyHash(context.Background(), nodeClient,
			[]string{firstNode.Name, secondNode.Name}, 2, expectedHash)
		if err != nil {
			t.Fatalf("targetNodesReadyWithKeyHash() returned an error: %v", err)
		}
		if !ready {
			t.Fatal("targetNodesReadyWithKeyHash() = false, want true")
		}
	})

	t.Run("one node not ready", func(t *testing.T) {
		readyNode := newNodeWithKeyHash(expectedHash,
			corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})
		readyNode.Name = "ready-node"
		notReadyNode := newNodeWithKeyHash(expectedHash,
			corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse})
		notReadyNode.Name = "not-ready-node"
		nodeClient := fake.NewSimpleClientset(readyNode, notReadyNode).CoreV1().Nodes()

		ready, err := targetNodesReadyWithKeyHash(context.Background(), nodeClient,
			[]string{readyNode.Name, notReadyNode.Name}, 2, expectedHash)
		if err != nil {
			t.Fatalf("targetNodesReadyWithKeyHash() returned an error: %v", err)
		}
		if ready {
			t.Fatal("targetNodesReadyWithKeyHash() = true, want false")
		}
	})

	t.Run("empty selection", func(t *testing.T) {
		nodeClient := fake.NewSimpleClientset().CoreV1().Nodes()
		ready, err := targetNodesReadyWithKeyHash(context.Background(), nodeClient, nil, 1, expectedHash)
		if err != nil {
			t.Fatalf("targetNodesReadyWithKeyHash() returned an error: %v", err)
		}
		if ready {
			t.Fatal("targetNodesReadyWithKeyHash() = true, want false")
		}
	})

	t.Run("incomplete selection", func(t *testing.T) {
		node := newNodeWithKeyHash(expectedHash,
			corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue})
		node.Name = "only-node"
		nodeClient := fake.NewSimpleClientset(node).CoreV1().Nodes()

		ready, err := targetNodesReadyWithKeyHash(context.Background(), nodeClient,
			[]string{node.Name}, 2, expectedHash)
		if err != nil {
			t.Fatalf("targetNodesReadyWithKeyHash() returned an error: %v", err)
		}
		if ready {
			t.Fatal("targetNodesReadyWithKeyHash() = true, want false")
		}
	})

	t.Run("node read error", func(t *testing.T) {
		nodeClient := fake.NewSimpleClientset().CoreV1().Nodes()
		if _, err := targetNodesReadyWithKeyHash(context.Background(), nodeClient,
			[]string{"missing-node"}, 1, expectedHash); err == nil {
			t.Fatal("targetNodesReadyWithKeyHash() returned no error for failed node read")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		nodeClient := fake.NewSimpleClientset().CoreV1().Nodes()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := targetNodesReadyWithKeyHash(ctx, nodeClient,
			[]string{"target-node"}, 1, expectedHash); err != context.Canceled {
			t.Fatalf("targetNodesReadyWithKeyHash() error = %v, want context.Canceled", err)
		}
	})
}
