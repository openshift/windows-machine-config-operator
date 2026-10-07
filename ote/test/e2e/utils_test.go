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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

func TestNodeReadyWithKeyHash(t *testing.T) {
	const expectedHash = "replacement-key-hash"

	tests := []struct {
		name         string
		node         *corev1.Node
		expectedHash string
		want         bool
	}{
		{
			name: "Ready with replacement key hash",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
			want:         true,
		},
		{
			name: "not Ready",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}),
			expectedHash: expectedHash,
		},
		{
			name: "wrong key hash",
			node: newNodeWithKeyHash("old-key-hash",
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
		},
		{
			name:         "missing Ready condition",
			node:         newNodeWithKeyHash(expectedHash),
			expectedHash: expectedHash,
		},
		{
			name: "ignores other conditions",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
			want:         true,
		},
		{
			name:         "empty expected hash",
			node:         newNodeWithKeyHash(expectedHash),
			expectedHash: "",
		},
		{
			name:         "nil node",
			expectedHash: expectedHash,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nodeReadyWithKeyHash(test.node, test.expectedHash); got != test.want {
				t.Fatalf("nodeReadyWithKeyHash() = %t, want %t", got, test.want)
			}
		})
	}
}

func newNodeWithKeyHash(hash string, conditions ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{pubKeyHashAnno: hash}},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}

func TestCaptureWindowsServiceStatusUsesDiagnosticContext(t *testing.T) {
	parentCtx, cancelParent := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancelParent()
	diagnosticCtx, cancelDiagnostic := context.WithTimeout(parentCtx, time.Hour)
	defer cancelDiagnostic()
	diagnosticDeadline, hasDiagnosticDeadline := diagnosticCtx.Deadline()
	require.True(t, hasDiagnosticDeadline)

	calls := 0
	captureWindowsServiceStatus(diagnosticCtx, []string{"windows-node"},
		func(string) (bool, error) { return false, nil },
		func(ctx context.Context, nodeName string) (string, error) {
			calls++
			assert.Equal(t, "windows-node", nodeName)
			receivedDeadline, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			assert.Equal(t, diagnosticDeadline, receivedDeadline,
				"the diagnostic deadline, not the broader parent deadline, must own HostProcess work")
			return "Running", nil
		})

	assert.Equal(t, 1, calls)
}

func TestCaptureWindowsServiceStatusPreCanceledContextStartsNoNodeWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	readyCalls := 0
	hostProcessCalls := 0
	captureWindowsServiceStatus(ctx, []string{"node-1", "node-2"},
		func(string) (bool, error) {
			readyCalls++
			return false, nil
		},
		func(context.Context, string) (string, error) {
			hostProcessCalls++
			return "", nil
		})

	assert.Equal(t, 0, readyCalls)
	assert.Equal(t, 0, hostProcessCalls)
}

func TestCaptureWindowsServiceStatusMidCallDeadlineStopsEnclosingNodeLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var readyNodes []string
	var hostProcessNodes []string
	captureWindowsServiceStatus(ctx, []string{"node-1", "node-2"},
		func(nodeName string) (bool, error) {
			readyNodes = append(readyNodes, nodeName)
			return false, nil
		},
		func(callCtx context.Context, nodeName string) (string, error) {
			hostProcessNodes = append(hostProcessNodes, nodeName)
			<-callCtx.Done()
			return "", callCtx.Err()
		})

	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	assert.Equal(t, []string{"node-1"}, readyNodes)
	assert.Equal(t, []string{"node-1"}, hostProcessNodes)
}

func testHostProcessPod(uid types.UID, phase corev1.PodPhase, createdAt time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "hostprocess-pod", UID: uid, CreationTimestamp: metav1.NewTime(createdAt)},
		Status:     corev1.PodStatus{Phase: phase},
	}
}
