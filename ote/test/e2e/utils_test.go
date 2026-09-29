package winc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testHostProcessLogRetryConfig() hostProcessLogRetryConfig {
	config := defaultHostProcessLogRetryConfig()
	config.overallTimeout = time.Second
	config.perRequestTimeout = 100 * time.Millisecond
	config.retryDelay = 0
	return config
}

func TestHostProcessLogRetryBounds(t *testing.T) {
	config := defaultHostProcessLogRetryConfig()
	if config.maxAttempts != 5 {
		t.Fatalf("expected 5 attempts, got %d", config.maxAttempts)
	}
	if config.overallTimeout != 90*time.Second {
		t.Fatalf("expected 90s overall timeout, got %s", config.overallTimeout)
	}
	if config.perRequestTimeout != 10*time.Second {
		t.Fatalf("expected 10s request timeout, got %s", config.perRequestTimeout)
	}
}

func TestRetrieveHostProcessLogsRetriesResetThenSucceeds(t *testing.T) {
	attempts := 0
	runner := func(ctx context.Context, podName string) (string, error) {
		attempts++
		if podName != "hostprocess-pod" {
			t.Fatalf("unexpected pod name %q", podName)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("log request context has no deadline")
		}
		if attempts == 1 {
			return "", errors.New("read tcp: connection reset by peer")
		}
		return "command output", nil
	}

	output, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod", runner,
		testHostProcessLogRetryConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "command output" {
		t.Fatalf("unexpected output %q", output)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
}

func TestExecuteHostProcessPodReportsLogRetryExhaustion(t *testing.T) {
	attempts := 0
	diagnosticsCalls := 0
	cleanupCalls := 0
	operations := hostProcessPodOperations{
		create: func() error { return nil },
		cleanup: func() {
			cleanupCalls++
		},
		waitForTerminalPhase: func() (string, error) { return "Succeeded", nil },
		retrieveLogs: func(context.Context, string) (string, error) {
			attempts++
			return "", errors.New("unexpected EOF")
		},
		terminationDetails: func(context.Context) (string, string) {
			diagnosticsCalls++
			return "0", "Completed"
		},
	}

	_, err := executeHostProcessPod(context.Background(), "node-a", "hostprocess-pod", true,
		operations, testHostProcessLogRetryConfig())
	if err == nil {
		t.Fatal("expected log retrieval to fail")
	}
	for _, expected := range []string{
		"phase=Succeeded",
		"containerExitCode=0",
		"containerReason=Completed",
		"node=node-a",
		"attempts=5",
		"elapsed=",
		"lastError=unexpected EOF",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not contain %q", err, expected)
		}
	}
	if attempts != 5 {
		t.Fatalf("expected 5 attempts, got %d", attempts)
	}
	if diagnosticsCalls != 1 {
		t.Fatalf("expected diagnostics once, got %d", diagnosticsCalls)
	}
	if cleanupCalls != 1 {
		t.Fatalf("expected cleanup once, got %d", cleanupCalls)
	}
}

func TestRetrieveHostProcessLogsFailsFast(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "authorization", err: errors.New("forbidden: User cannot get resource pods/log")},
		{name: "missing pod", err: errors.New(`pods "hostprocess-pod" not found`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			_, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod",
				func(context.Context, string) (string, error) {
					attempts++
					return "", test.err
				}, testHostProcessLogRetryConfig())
			if err == nil {
				t.Fatal("expected log retrieval to fail")
			}
			if attempts != 1 {
				t.Fatalf("expected 1 attempt, got %d", attempts)
			}
			var retrievalErr *hostProcessLogRetrievalError
			if !errors.As(err, &retrievalErr) {
				t.Fatalf("expected retrieval error, got %T", err)
			}
			if retrievalErr.exhausted {
				t.Fatal("non-retriable error was reported as exhausted")
			}
		})
	}
}

func TestRetrieveHostProcessLogsRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	_, err := retrieveHostProcessLogs(ctx, "hostprocess-pod",
		func(context.Context, string) (string, error) {
			attempts++
			cancel()
			return "", errors.New("connection reset by peer")
		}, testHostProcessLogRetryConfig())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected 1 attempt, got %d", attempts)
	}
}

func TestExecuteHostProcessPodPreservesFailedPhase(t *testing.T) {
	createCalls := 0
	logCalls := 0
	diagnosticsCalls := 0
	cleanupCalls := 0
	operations := hostProcessPodOperations{
		create: func() error {
			createCalls++
			return nil
		},
		cleanup: func() {
			cleanupCalls++
		},
		waitForTerminalPhase: func() (string, error) { return "Failed", nil },
		retrieveLogs: func(context.Context, string) (string, error) {
			logCalls++
			return "", nil
		},
		terminationDetails: func(context.Context) (string, string) {
			diagnosticsCalls++
			return "1", "Error"
		},
	}

	_, err := executeHostProcessPod(context.Background(), "node-a", "hostprocess-pod", true,
		operations, testHostProcessLogRetryConfig())
	if err == nil || !strings.Contains(err.Error(), "phase Failed") {
		t.Fatalf("expected failed pod error, got %v", err)
	}
	if createCalls != 1 || cleanupCalls != 1 {
		t.Fatalf("expected one create and cleanup, got create=%d cleanup=%d", createCalls, cleanupCalls)
	}
	if logCalls != 0 || diagnosticsCalls != 0 {
		t.Fatalf("failed pod should fail before log retrieval, got logs=%d diagnostics=%d",
			logCalls, diagnosticsCalls)
	}
}

func TestExecuteHostProcessPodCreatesOnlyOnceDuringLogRetries(t *testing.T) {
	createCalls := 0
	waitCalls := 0
	logCalls := 0
	cleanupCalls := 0
	operations := hostProcessPodOperations{
		create: func() error {
			createCalls++
			return nil
		},
		cleanup: func() {
			cleanupCalls++
		},
		waitForTerminalPhase: func() (string, error) {
			waitCalls++
			return "Succeeded", nil
		},
		retrieveLogs: func(context.Context, string) (string, error) {
			logCalls++
			if logCalls < 3 {
				return "", fmt.Errorf("request timeout on attempt %d", logCalls)
			}
			return "PowerShell completed", nil
		},
		terminationDetails: func(context.Context) (string, string) {
			t.Fatal("diagnostics should not run after a successful retry")
			return "", ""
		},
	}

	output, err := executeHostProcessPod(context.Background(), "node-a", "hostprocess-pod", true,
		operations, testHostProcessLogRetryConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "PowerShell completed" {
		t.Fatalf("unexpected output %q", output)
	}
	if createCalls != 1 || waitCalls != 1 || cleanupCalls != 1 {
		t.Fatalf("pod lifecycle repeated: create=%d wait=%d cleanup=%d", createCalls, waitCalls, cleanupCalls)
	}
	if logCalls != 3 {
		t.Fatalf("expected 3 log attempts, got %d", logCalls)
	}
}

func TestExecuteHostProcessPodReturnsSemanticOutputUnchanged(t *testing.T) {
	expected := "NO_PROXY=api.example.test,10.0.0.0/8"
	operations := hostProcessPodOperations{
		create:               func() error { return nil },
		cleanup:              func() {},
		waitForTerminalPhase: func() (string, error) { return "Succeeded", nil },
		retrieveLogs: func(context.Context, string) (string, error) {
			return expected, nil
		},
		terminationDetails: func(context.Context) (string, string) { return "", "" },
	}

	output, err := executeHostProcessPod(context.Background(), "node-a", "hostprocess-pod", true,
		operations, testHostProcessLogRetryConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != expected {
		t.Fatalf("semantic output changed: expected %q, got %q", expected, output)
	}
}
