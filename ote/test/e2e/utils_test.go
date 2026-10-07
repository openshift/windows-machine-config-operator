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

	g "github.com/onsi/ginkgo/v2"
	exutil "github.com/openshift/origin/test/extended/util"
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

// TestDebugWindowsNodesNotReadyProductionWiring checks the real diagnostic entry and its bindings.
// Seven-step ordering, shared node discovery, context forwarding, and single cancellation reporting remain intact.
func TestDebugWindowsNodesNotReadyProductionWiring(t *testing.T) {
	wantCalls := [][]string{
		{"nodes", "-l", "kubernetes.io/os=windows", "-o=custom-columns=NAME:.metadata.name,STATUS:.status.conditions[?(@.type==\"Ready\")].status,REASON:.status.conditions[?(@.type==\"Ready\")].reason,MESSAGE:.status.conditions[?(@.type==\"Ready\")].message"},
		{"nodes", "-l", "kubernetes.io/os=windows", "-o=jsonpath={.items[*].metadata.name}"},
		{"node", "node-z", "-o=jsonpath={.status.conditions[*].type}"},
		{"node", "node-a", "-o=jsonpath={.status.conditions[*].type}"},
		{"pods", "-n", "openshift-windows-machine-config-operator", "-l", "app=windows-machine-config-operator", "-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase"},
		{"events", "--all-namespaces", "--field-selector", "involvedObject.name=node-z", "--sort-by=.lastTimestamp", "-o=custom-columns=TYPE:.type,REASON:.reason"},
		{"events", "--all-namespaces", "--field-selector", "involvedObject.name=node-a", "--sort-by=.lastTimestamp", "-o=custom-columns=TYPE:.type,REASON:.reason"},
		{"pods", "-n", "openshift-windows-machine-config-operator", "-l", "app=windows-instance-config-daemon", "-o=custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,READY:.status.conditions[?(@.type==\"Ready\")].status"},
		{"machines", "-n", "openshift-machine-api", "-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase,NODE:.status.nodeRef.name,PROVIDERSTATE:.status.providerStatus.instanceState"},
		{"node", "node-z", "-o=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}"},
		{"HostProcess", "node-z", "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022", "Get-Service kubelet,windows-instance-config-daemon,containerd | Format-Table -AutoSize | Out-String -Width 200"},
		{"node", "node-a", "-o=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}"},
		{"HostProcess", "node-a", "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022", "Get-Service kubelet,windows-instance-config-daemon,containerd | Format-Table -AutoSize | Out-String -Width 200"},
	}
	tests := []struct {
		name          string
		preCanceled   bool
		expired       bool
		cancelAfter   int
		wantCalls     int
		wantCompleted int
	}{
		{name: "seven production steps and shared node names", wantCalls: len(wantCalls)},
		{name: "pre-canceled entry", preCanceled: true},
		{name: "expired entry", expired: true},
		{name: "cancel between condition nodes", cancelAfter: 3, wantCalls: 3, wantCompleted: 2},
		{name: "cancel between event nodes", cancelAfter: 6, wantCalls: 6, wantCompleted: 4},
		{name: "cancel during service readiness", cancelAfter: 10, wantCalls: 10, wantCompleted: 7},
		{name: "cancel between service nodes", cancelAfter: 11, wantCalls: 11, wantCompleted: 7},
		{name: "cancel in final service call", cancelAfter: 13, wantCalls: 13, wantCompleted: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deadline := time.Now().Add(time.Hour)
			if test.expired {
				deadline = time.Unix(0, 0)
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			if test.preCanceled {
				cancel()
			}
			oc := &exutil.CLI{}
			var calls [][]string
			recordCall := func(args []string) {
				calls = append(calls, append([]string(nil), args...))
				if len(calls) == test.cancelAfter {
					cancel()
				}
			}
			originalOutput, originalHostProcess := windowsDiagnosticOutput, windowsDiagnosticHostProcess
			t.Cleanup(func() {
				windowsDiagnosticOutput, windowsDiagnosticHostProcess = originalOutput, originalHostProcess
			})
			windowsDiagnosticOutput = func(receivedCLI *exutil.CLI, args ...string) (string, error) {
				assert.Same(t, oc, receivedCLI)
				recordCall(args)
				if len(args) == 4 && args[3] == "-o=jsonpath={.items[*].metadata.name}" {
					return "  node-z\n node-a  ", nil
				}
				return "False", nil
			}
			windowsDiagnosticHostProcess = func(receivedCtx context.Context, receivedCLI *exutil.CLI,
				nodeName, image, command string, waitForCompletion ...bool) (string, error) {
				assert.Same(t, ctx, receivedCtx)
				receivedDeadline, hasDeadline := receivedCtx.Deadline()
				assert.True(t, hasDeadline)
				assert.Equal(t, deadline, receivedDeadline)
				assert.NoError(t, receivedCtx.Err())
				assert.Same(t, oc, receivedCLI)
				assert.Empty(t, waitForCompletion)
				recordCall([]string{"HostProcess", nodeName, image, command})
				return "service status", receivedCtx.Err()
			}
			var logs bytes.Buffer
			g.GinkgoWriter.TeeTo(&logs)
			defer g.GinkgoWriter.ClearTeeWriters()

			debugWindowsNodesNotReady(ctx, oc)

			if test.wantCalls == 0 {
				assert.Empty(t, calls)
			} else {
				assert.Equal(t, wantCalls[:test.wantCalls], calls)
			}
			if test.preCanceled || test.expired || test.cancelAfter > 0 {
				message := "Diagnostic capture canceled"
				if test.expired {
					message = "Diagnostic timeout reached"
				}
				assert.Contains(t, logs.String(), fmt.Sprintf("%s after %d completed steps, stopping debug capture", message, test.wantCompleted))
				assert.Equal(t, 1, strings.Count(logs.String(), "stopping debug capture"))
				assert.NotContains(t, logs.String(), "===== END DEBUG =====")
			} else {
				assert.NotContains(t, logs.String(), "stopping debug capture")
				assert.Equal(t, 1, strings.Count(logs.String(), "===== END DEBUG ====="))
			}
		})
	}
}

// TestRunDiagnostics checks ordered diagnostic steps and cancellation accounting.
// Pre-canceled, expired, and mid/final-step cancellation cases must stop later work and report completion exactly once.
func TestRunDiagnostics(t *testing.T) {
	tests := []struct {
		name          string
		preCanceled   bool
		expired       bool
		cancelAt      int
		wantSteps     []int
		wantCompleted int
		wantMessage   string
	}{
		{name: "active steps run in order", wantSteps: []int{1, 2, 3}},
		{name: "pre-canceled", preCanceled: true, wantMessage: "Diagnostic capture canceled"},
		{name: "expired deadline", expired: true, wantMessage: "Diagnostic timeout reached"},
		{name: "canceled in first step", cancelAt: 1, wantSteps: []int{1}, wantCompleted: 1, wantMessage: "Diagnostic capture canceled"},
		{name: "canceled between steps", cancelAt: 2, wantSteps: []int{1, 2}, wantCompleted: 2, wantMessage: "Diagnostic capture canceled"},
		{name: "canceled in final step", cancelAt: 3, wantSteps: []int{1, 2, 3}, wantCompleted: 3, wantMessage: "Diagnostic capture canceled"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.preCanceled {
				cancel()
			}
			if test.expired {
				var cancelDeadline context.CancelFunc
				ctx, cancelDeadline = context.WithDeadline(ctx, time.Unix(0, 0))
				defer cancelDeadline()
			}
			var logs bytes.Buffer
			g.GinkgoWriter.TeeTo(&logs)
			defer g.GinkgoWriter.ClearTeeWriters()
			oc := &exutil.CLI{}
			var stepsRun []int
			var steps []func(context.Context, *exutil.CLI)
			for stepNumber := 1; stepNumber <= 3; stepNumber++ {
				steps = append(steps, func(stepCtx context.Context, stepCLI *exutil.CLI) {
					assert.Same(t, ctx, stepCtx)
					assert.Same(t, oc, stepCLI)
					stepsRun = append(stepsRun, stepNumber)
					if stepNumber == test.cancelAt {
						cancel()
					}
				})
			}
			runDiagnostics(ctx, oc, steps...)
			assert.Equal(t, test.wantSteps, stepsRun)
			if test.wantMessage == "" {
				assert.Empty(t, logs.String())
			} else {
				assert.Contains(t, logs.String(), fmt.Sprintf("%s after %d completed steps, stopping debug capture", test.wantMessage, test.wantCompleted))
				assert.Equal(t, 1, strings.Count(logs.String(), "stopping debug capture"))
			}
		})
	}
}

// TestRunDiagnosticsWithoutSteps checks an active diagnostic sequence with no callbacks.
// The empty sequence returns without emitting a cancellation or timeout message.
func TestRunDiagnosticsWithoutSteps(t *testing.T) {
	var logs bytes.Buffer
	g.GinkgoWriter.TeeTo(&logs)
	defer g.GinkgoWriter.ClearTeeWriters()
	runDiagnostics(context.Background(), nil)
	assert.Empty(t, logs.String())
}

// TestRunDiagnosticsServiceStatusCancellation checks cancellation inside service diagnostics.
// Cancellation during readiness or service capture skips later nodes and steps while reporting the completed step once.
func TestRunDiagnosticsServiceStatusCancellation(t *testing.T) {
	for _, cancelDuringReadiness := range []bool{false, true} {
		for _, nodeNames := range [][]string{{"node-1"}, {"node-1", "node-2"}} {
			t.Run(fmt.Sprintf("readiness=%t/nodes=%d", cancelDuringReadiness, len(nodeNames)), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var logs bytes.Buffer
				g.GinkgoWriter.TeeTo(&logs)
				defer g.GinkgoWriter.ClearTeeWriters()
				var readyNodes, serviceNodes []string
				runDiagnostics(ctx, nil, func(ctx context.Context, oc *exutil.CLI) {
					captureWindowsServiceStatus(ctx, nodeNames,
						func(nodeName string) (bool, error) {
							readyNodes = append(readyNodes, nodeName)
							if cancelDuringReadiness {
								cancel()
							}
							return false, nil
						},
						func(serviceCtx context.Context, nodeName string) (string, error) {
							assert.Same(t, ctx, serviceCtx)
							serviceNodes = append(serviceNodes, nodeName)
							cancel()
							return "", context.Canceled
						})
				}, func(context.Context, *exutil.CLI) {
					t.Error("diagnostics continued after cancellation")
				})
				assert.Equal(t, []string{"node-1"}, readyNodes)
				if cancelDuringReadiness {
					assert.Empty(t, serviceNodes)
				} else {
					assert.Equal(t, []string{"node-1"}, serviceNodes)
				}
				assert.Equal(t, 1, strings.Count(logs.String(), "stopping debug capture"))
				assert.Contains(t, logs.String(), "after 1 completed steps")
			})
		}
	}
}

// TestCaptureWindowsServiceStatusUsesDiagnosticContext checks the context passed to service capture.
// The HostProcess callback receives the diagnostic deadline rather than the broader parent deadline.
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

// TestRunDiagnosticsContinuesAfterServiceStatusFailures checks non-canceling query failures.
// Readiness failures and Ready nodes skip service capture, while service errors still allow later nodes and steps.
func TestRunDiagnosticsContinuesAfterServiceStatusFailures(t *testing.T) {
	var logs bytes.Buffer
	g.GinkgoWriter.TeeTo(&logs)
	defer g.GinkgoWriter.ClearTeeWriters()
	var readyNodes, serviceNodes []string
	laterStepRan := false
	runDiagnostics(context.Background(), nil, func(ctx context.Context, oc *exutil.CLI) {
		captureWindowsServiceStatus(ctx, []string{"ready-error", "ready", "service-error", "service-ok"},
			func(nodeName string) (bool, error) {
				readyNodes = append(readyNodes, nodeName)
				if nodeName == "ready-error" {
					return false, errors.New("readiness failed")
				}
				return nodeName == "ready", nil
			},
			func(ctx context.Context, nodeName string) (string, error) {
				serviceNodes = append(serviceNodes, nodeName)
				if nodeName == "service-error" {
					return "", errors.New("service query failed")
				}
				return "Running", nil
			})
	}, func(context.Context, *exutil.CLI) { laterStepRan = true })
	assert.Equal(t, []string{"ready-error", "ready", "service-error", "service-ok"}, readyNodes)
	assert.Equal(t, []string{"service-error", "service-ok"}, serviceNodes)
	assert.True(t, laterStepRan)
	assert.NotContains(t, logs.String(), "stopping debug capture")
}

// TestCaptureWindowsServiceStatusPreCanceledContextStartsNoNodeWork checks canceled entry behavior.
// Neither readiness nor HostProcess callbacks may run when the context is already canceled.
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

// TestCaptureWindowsServiceStatusMidCallDeadlineStopsEnclosingNodeLoop checks an expiring service call.
// The first callback observes the diagnostic deadline, and no readiness or HostProcess work starts for later nodes.
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

// testHostProcessPod builds a minimal named pod fixture for lifecycle tests.
// Callers choose the UID, phase, and creation time without introducing containers or other lifecycle state.
func testHostProcessPod(uid types.UID, phase corev1.PodPhase, createdAt time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "hostprocess-pod", UID: uid, CreationTimestamp: metav1.NewTime(createdAt)},
		Status:     corev1.PodStatus{Phase: phase},
	}
}
