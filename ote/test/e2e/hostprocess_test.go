package winc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// testHostProcessLogRetryConfig provides short retry timing for log-read unit tests.
// It keeps the production attempt limit while shortening deadlines and removing the retry delay.
func testHostProcessLogRetryConfig() hostProcessLogRetryConfig {
	config := defaultHostProcessLogRetryConfig()
	config.overallTimeout = time.Second
	config.perRequestTimeout = 100 * time.Millisecond
	config.retryDelay = 0
	return config
}

// TestHostProcessLogRetryBounds checks the production log-retry constants.
// The default policy must retain five attempts, a 90-second retry budget, and ten-second request timeouts.
func TestHostProcessLogRetryBounds(t *testing.T) {
	t.Parallel()

	config := defaultHostProcessLogRetryConfig()
	assert.Equal(t, 5, config.maxAttempts)
	assert.Equal(t, 90*time.Second, config.overallTimeout)
	assert.Equal(t, 10*time.Second, config.perRequestTimeout)
}

// TestRunHostProcessPSLegacySignaturePreserved checks compatibility with the original runner type.
// The variadic completion argument and string/error results must remain assignable to existing callers.
func TestRunHostProcessPSLegacySignaturePreserved(t *testing.T) {
	var legacyRunner func(*exutil.CLI, string, string, string, ...bool) (string, error) = runHostProcessPS
	assert.NotNil(t, legacyRunner)
}

// TestIsTransientHostProcessAPIError covers retry classification of API and transport failures.
// Authorization, missing pods, and permanent TLS alerts stay terminal while transient transport cases remain eligible.
func TestIsTransientHostProcessAPIError(t *testing.T) {
	t.Parallel()

	pods := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "connection reset", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, transient: true},
		{name: "connection refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, transient: true},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, transient: true},
		{name: "request deadline", err: context.DeadlineExceeded, transient: true},
		{name: "TLS handshake", err: errors.New("net/http: TLS handshake timeout"), transient: true},
		{name: "HTTP2 connection", err: errors.New("http2: client connection lost"), transient: true},
		{name: "GOAWAY", err: errors.New("server sent GOAWAY"), transient: true},
		{name: "service unavailable", err: apierrors.NewServiceUnavailable("apiserver unavailable"), transient: true},
		{name: "internal error", err: apierrors.NewInternalError(errors.New("backend failure")), transient: true},
		{name: "internal error with unrelated not found text", err: apierrors.NewInternalError(errors.New("backend service not found")), transient: true},
		{name: "service unavailable with unrelated not found text", err: apierrors.NewServiceUnavailable("backend service not found"), transient: true},
		{name: "transport error with unrelated not found text", err: errors.New("backend service not found: connection reset by peer"), transient: true},
		{name: "server timeout", err: apierrors.NewServerTimeout(pods, "get", 1), transient: true},
		{name: "too many requests", err: apierrors.NewTooManyRequests("slow down", 1), transient: true},
		{name: "forbidden", err: apierrors.NewForbidden(pods, "hostprocess-pod", errors.New("denied"))},
		{name: "unauthorized", err: apierrors.NewUnauthorized("credentials rejected")},
		{name: "missing pod", err: apierrors.NewNotFound(pods, "hostprocess-pod")},
		{name: "permanent remote TLS alert", err: errors.New("remote error: tls: bad certificate")},
		{name: "other error", err: errors.New("certificate is invalid")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.transient, isTransientHostProcessAPIError(test.err))
		})
	}
}

// TestIsTransientHostProcessAPIErrorPrecedence checks conflicting status and transport evidence.
// Typed terminal statuses win first; typed transient statuses can override text that rejects untyped failures.
func TestIsTransientHostProcessAPIErrorPrecedence(t *testing.T) {
	t.Parallel()
	pods := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name      string
		err       error
		transient bool
	}{
		{name: "nil"},
		{name: "typed forbidden beats transport", err: apierrors.NewForbidden(pods, "pod", io.EOF)},
		{name: "typed unauthorized beats transport", err: apierrors.NewUnauthorized("connection reset")},
		{name: "typed not found beats transport", err: fmt.Errorf("TLS handshake timeout: %w", apierrors.NewNotFound(pods, "pod"))},
		{name: "typed transient beats authorization text", err: apierrors.NewServiceUnavailable("forbidden: unauthorized"), transient: true},
		{name: "typed transient beats missing pod text", err: apierrors.NewInternalError(errors.New(`pods "pod" not found`)), transient: true},
		{name: "terminal text beats deadline", err: fmt.Errorf("forbidden: %w", context.DeadlineExceeded)},
		{name: "terminal text beats network", err: fmt.Errorf("unauthorized: %w", &net.DNSError{IsTimeout: true})},
		{name: "missing pod text beats EOF", err: fmt.Errorf(`pods "pod" not found: %w`, io.EOF)},
		{name: "caller cancellation alone", err: context.Canceled},
		{name: "transport text with caller cancellation", err: fmt.Errorf("connection reset: %w", context.Canceled), transient: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.transient, baselineIsTransientHostProcessAPIError(test.err))
			assert.Equal(t, test.transient, isTransientHostProcessAPIError(test.err))
		})
	}
}

// TestIsTransientHostProcessAPIErrorBaselineEquivalence compares classification with the frozen oracle.
// The matrix exercises plain, wrapped, and joined errors in both orders to preserve short-circuit precedence.
func TestIsTransientHostProcessAPIErrorBaselineEquivalence(t *testing.T) {
	t.Parallel()
	pods := schema.GroupResource{Resource: "pods"}
	baseErrors := []struct {
		name string
		err  error
	}{
		{name: "ordinary", err: errors.New("ordinary failure")},
		{name: "forbidden", err: apierrors.NewForbidden(pods, "pod", errors.New("denied"))},
		{name: "unauthorized", err: apierrors.NewUnauthorized("denied")},
		{name: "not-found", err: apierrors.NewNotFound(pods, "pod")},
		{name: "timeout", err: apierrors.NewTimeoutError("request timed out", 1)},
		{name: "server-timeout", err: apierrors.NewServerTimeout(pods, "get", 1)},
		{name: "rate-limited", err: apierrors.NewTooManyRequests("slow down", 1)},
		{name: "internal", err: apierrors.NewInternalError(errors.New("backend failure"))},
		{name: "unavailable", err: apierrors.NewServiceUnavailable("unavailable")},
		{name: "bad-request", err: apierrors.NewBadRequest("bad request")},
		{name: "conflict", err: apierrors.NewConflict(pods, "pod", errors.New("conflict"))},
		{name: "gone", err: apierrors.NewGone("gone")},
		{name: "terminal-reason-transient-code", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonForbidden, Code: 503}}},
		{name: "transient-reason-terminal-code", err: &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonTimeout, Code: 403}}},
		{name: "unknown-reason-terminal-code", err: &apierrors.StatusError{ErrStatus: metav1.Status{Code: 404}}},
		{name: "unknown-reason-transient-code", err: &apierrors.StatusError{ErrStatus: metav1.Status{Code: 503}}},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "canceled", err: context.Canceled},
		{name: "EOF", err: io.EOF},
		{name: "unexpected-EOF", err: io.ErrUnexpectedEOF},
		{name: "reset", err: syscall.ECONNRESET},
		{name: "refused", err: syscall.ECONNREFUSED},
		{name: "broken-pipe", err: syscall.EPIPE},
		{name: "network-timeout", err: &net.DNSError{IsTimeout: true}},
		{name: "network-temporary", err: &net.DNSError{IsTemporary: true}},
		{name: "network-permanent", err: &net.DNSError{}},
		{name: "network-op-reset", err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}},
		{name: "network-op-refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
	}
	messages := []string{
		"ordinary failure", "forbidden", "unauthorized", "pod not found",
		`pod "pod" not found`, `pods "pod" not found`, "backend service not found",
		"connection reset", "connection refused", "tls handshake timeout",
		"http2: client connection lost", "goaway", "unexpected eof",
		"remote error: tls: bad certificate", "FORBIDDEN: TLS HANDSHAKE TIMEOUT",
		`pods "pod" not found: connection reset`, "UNEXPECTED EOF",
	}
	comparisons := 0
	check := func(t *testing.T, err error) {
		t.Helper()
		comparisons++
		assert.Equal(t, baselineIsTransientHostProcessAPIError(err), isTransientHostProcessAPIError(err))
	}
	check(t, nil)
	for _, message := range messages {
		t.Run("text/"+message, func(t *testing.T) { check(t, errors.New(message)) })
	}
	for _, base := range baseErrors {
		t.Run(base.name, func(t *testing.T) {
			check(t, base.err)
			for _, message := range messages {
				t.Run("message/"+message, func(t *testing.T) {
					wrapped := fmt.Errorf("%s: %w", message, base.err)
					check(t, wrapped)
					check(t, fmt.Errorf("outer: %w", wrapped))
					check(t, errors.Join(errors.New(message), base.err))
					check(t, errors.Join(base.err, errors.New(message)))
				})
			}
			for _, other := range baseErrors {
				t.Run("joined/"+other.name, func(t *testing.T) {
					check(t, errors.Join(base.err, other.err))
				})
			}
		})
	}
	t.Logf("baseline equivalence: %d comparisons, %d base errors, %d messages", comparisons, len(baseErrors), len(messages))
}

// baselineIsTransientHostProcessAPIError freezes bcb6a9183's classifier for differential regression checks.
// Its independent control flow preserves typed-status and terminal-text precedence for the test oracle.
func baselineIsTransientHostProcessAPIError(err error) bool {
	if err == nil || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) {
		return false
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	errorText := strings.ToLower(err.Error())
	for _, marker := range []string{"forbidden", "unauthorized"} {
		if strings.Contains(errorText, marker) {
			return false
		}
	}
	if baselineIsMissingHostProcessPodError(errorText) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	for _, marker := range []string{
		"connection reset", "connection refused", "tls handshake timeout",
		"http2: client connection lost", "goaway", "unexpected eof",
	} {
		if strings.Contains(errorText, marker) {
			return true
		}
	}
	return false
}

// baselineIsMissingHostProcessPodError keeps the frozen oracle's missing-pod text rules independent.
// Only pod-specific not-found forms qualify, matching the original classifier's helper.
func baselineIsMissingHostProcessPodError(errorText string) bool {
	if strings.Contains(errorText, "pod not found") {
		return true
	}
	for _, podResource := range []string{`pod "`, `pods "`} {
		if strings.Contains(errorText, podResource) && strings.Contains(errorText, `" not found`) {
			return true
		}
	}
	return false
}

// TestRetrieveHostProcessLogsRetriesTransientTLSFailure checks recovery from a TLS handshake timeout.
// The second read returns the expected output without exhausting the retry policy.
func TestRetrieveHostProcessLogsRetriesTransientTLSFailure(t *testing.T) {
	t.Parallel()

	attempts := 0
	output, err := retrieveHostProcessLogs(context.Background(), func(context.Context) ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("net/http: TLS handshake timeout")
		}
		return []byte("command output"), nil
	}, testHostProcessLogRetryConfig())

	require.NoError(t, err)
	assert.Equal(t, []byte("command output"), output)
	assert.Equal(t, 2, attempts)
}

// TestRetrieveHostProcessLogsRecovers checks recovery after repeated connection resets.
// Each read receives a deadline, and the third attempt returns the command output.
func TestRetrieveHostProcessLogsRecovers(t *testing.T) {
	t.Parallel()

	attempts := 0
	output, err := retrieveHostProcessLogs(context.Background(), func(ctx context.Context) ([]byte, error) {
		attempts++
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)
		if attempts < 3 {
			return nil, fmt.Errorf("temporary transport failure: %w", syscall.ECONNRESET)
		}
		return []byte("command output"), nil
	}, testHostProcessLogRetryConfig())

	require.NoError(t, err)
	assert.Equal(t, []byte("command output"), output)
	assert.Equal(t, 3, attempts)
}

// TestRetrieveHostProcessLogsExhaustsAttempts checks the finite retry-attempt limit.
// Exhaustion reports the attempt count while retaining the final connection-reset error for unwrapping.
func TestRetrieveHostProcessLogsExhaustsAttempts(t *testing.T) {
	t.Parallel()

	config := testHostProcessLogRetryConfig()
	config.maxAttempts = 3
	attempts := 0
	_, err := retrieveHostProcessLogs(context.Background(), func(context.Context) ([]byte, error) {
		attempts++
		return nil, fmt.Errorf("temporary transport failure: %w", syscall.ECONNRESET)
	}, config)

	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ECONNRESET)
	assert.Contains(t, err.Error(), "exhausted after 3 attempt(s)")
	assert.Equal(t, 3, attempts)
}

// TestRetrieveHostProcessLogsStopsOnTerminalErrors checks failures that must not be retried.
// Each terminal API, text, or permanent TLS error stops after one attempt and remains discoverable in the result.
func TestRetrieveHostProcessLogsStopsOnTerminalErrors(t *testing.T) {
	t.Parallel()

	pods := schema.GroupResource{Resource: "pods"}
	tests := []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: apierrors.NewForbidden(pods, "hostprocess-pod", errors.New("denied"))},
		{name: "unauthorized", err: apierrors.NewUnauthorized("credentials rejected")},
		{name: "missing pod", err: apierrors.NewNotFound(pods, "hostprocess-pod")},
		{name: "authorization takes precedence over transport", err: errors.New("Forbidden: connection reset by peer")},
		{name: "missing pod takes precedence over transport", err: errors.New("pod not found: TLS handshake timeout")},
		{name: "permanent remote TLS alert", err: errors.New("remote error: tls: bad certificate")},
		{name: "other nontransient", err: errors.New("certificate is invalid")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			_, err := retrieveHostProcessLogs(context.Background(), func(context.Context) ([]byte, error) {
				attempts++
				return nil, test.err
			}, testHostProcessLogRetryConfig())

			require.Error(t, err)
			assert.ErrorIs(t, err, test.err)
			assert.Contains(t, err.Error(), "failed on attempt 1")
			assert.Equal(t, 1, attempts)
		})
	}
}

// TestRetrieveHostProcessLogsHonorsPerRequestTimeout checks cooperative read-timeout handling.
// Stalled reads observe their contexts, and the configured attempt limit stops further requests.
func TestRetrieveHostProcessLogsHonorsPerRequestTimeout(t *testing.T) {
	config := testHostProcessLogRetryConfig()
	config.maxAttempts = 2
	config.perRequestTimeout = 20 * time.Millisecond
	attempts := 0

	_, err := retrieveHostProcessLogs(context.Background(), func(ctx context.Context) ([]byte, error) {
		attempts++
		<-ctx.Done()
		return nil, ctx.Err()
	}, config)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 2, attempts)
}

// TestRetrieveHostProcessLogsHonorsOverallDeadline checks expiration during the retry delay.
// The returned error retains both the deadline and the last transport failure after a single read.
func TestRetrieveHostProcessLogsHonorsOverallDeadline(t *testing.T) {
	config := testHostProcessLogRetryConfig()
	config.overallTimeout = 25 * time.Millisecond
	config.retryDelay = time.Second
	attempts := 0
	_, err := retrieveHostProcessLogs(context.Background(), func(context.Context) ([]byte, error) {
		attempts++
		return nil, fmt.Errorf("temporary transport failure: %w", syscall.ECONNRESET)
	}, config)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorIs(t, err, syscall.ECONNRESET)
	assert.Contains(t, err.Error(), "deadline exceeded")
	assert.Equal(t, 1, attempts)
}

// TestRetrieveHostProcessLogsRejectsLateSuccess checks a response arriving after its request deadline.
// A nominally successful late payload is discarded and the deadline error is returned.
func TestRetrieveHostProcessLogsRejectsLateSuccess(t *testing.T) {
	config := testHostProcessLogRetryConfig()
	config.maxAttempts = 1
	config.perRequestTimeout = 5 * time.Millisecond

	output, err := retrieveHostProcessLogs(context.Background(), func(ctx context.Context) ([]byte, error) {
		<-ctx.Done()
		return []byte("late output must not be accepted"), nil
	}, config)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Nil(t, output)
}

// TestRetrieveHostProcessLogsHonorsCallerCancellation checks cancellation during an active log read.
// The read observes the caller's cancellation and no second attempt starts.
func TestRetrieveHostProcessLogsHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	attempts := 0

	go func() {
		_, err := retrieveHostProcessLogs(ctx, func(requestCtx context.Context) ([]byte, error) {
			attempts++
			close(started)
			<-requestCtx.Done()
			return nil, requestCtx.Err()
		}, testHostProcessLogRetryConfig())
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, attempts)
	case <-time.After(time.Second):
		t.Fatal("log request did not observe caller cancellation")
	}
}

// TestFailedHostProcessCommandAfterLogRecovery checks command failure after transient log recovery.
// The failed phase and pod/node identity survive, while the failed command's payload stays out of the error.
func TestFailedHostProcessCommandAfterLogRecovery(t *testing.T) {
	t.Parallel()

	attempts := 0
	logOutput, logErr := retrieveHostProcessLogs(context.Background(), func(context.Context) ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("temporary transport failure: %w", syscall.ECONNRESET)
		}
		return []byte("PowerShell command failed with exit code 1"), nil
	}, testHostProcessLogRetryConfig())
	require.NoError(t, logErr)

	output, err := hostProcessCommandResult("windows-node", "hostprocess-pod", corev1.PodFailed, logOutput, logErr)

	require.Error(t, err)
	assert.Empty(t, output)
	assert.Equal(t, 2, attempts)
	assert.Contains(t, err.Error(), "hostprocess-pod")
	assert.Contains(t, err.Error(), "windows-node")
	assert.Contains(t, err.Error(), "phase=Failed")
	assert.Contains(t, err.Error(), "command output omitted")
	assert.NotContains(t, err.Error(), "PowerShell command failed")
}

// TestHostProcessCommandResultPreservesFailureWhenLogsFail checks simultaneous command and log failures.
// The result names the failed command's pod and node while retaining the underlying log error.
func TestHostProcessCommandResultPreservesFailureWhenLogsFail(t *testing.T) {
	t.Parallel()

	logErr := errors.New("log transport failed")
	_, err := hostProcessCommandResult("windows-node", "hostprocess-pod", corev1.PodFailed, nil, logErr)

	require.Error(t, err)
	assert.ErrorIs(t, err, logErr)
	assert.Contains(t, err.Error(), "command in pod hostprocess-pod on windows-node failed")
}

// TestHostProcessLifecycleLogsTerminationWithoutPayloads checks safe terminal lifecycle evidence.
// Exit code and deletion time are retained, but unsafe reasons, owner data, and free-form payloads are excluded.
func TestHostProcessLifecycleLogsTerminationWithoutPayloads(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, time.October, 4, 8, 0, 0, 0, time.UTC)
	deletedAt := metav1.NewTime(when.Add(3 * time.Second))
	pod := testHostProcessPod("failed-uid", corev1.PodFailed, when)
	pod.DeletionTimestamp = &deletedAt
	pod.OwnerReferences = []metav1.OwnerReference{{Kind: "unsafe kind", Name: "owner;secret", UID: "owner-uid"}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: pod.Name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 23, Reason: "unsafe reason; token=do-not-log", Message: "sensitive free-form payload",
			StartedAt: metav1.NewTime(when.Add(time.Second)), FinishedAt: metav1.NewTime(when.Add(2 * time.Second)),
		}},
	}}

	lines := (&hostProcessPodState{}).observe(pod, when.Add(4*time.Second))

	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "phase=Failed")
	assert.Contains(t, lines[0], "deletionTimestamp=2026-10-04T08:00:03Z")
	assert.Contains(t, lines[0], "state=terminated")
	assert.Contains(t, lines[0], "reason=redacted")
	assert.Contains(t, lines[0], "exitCode=23")
	assert.NotContains(t, lines[0], "sensitive free-form payload")
	assert.NotContains(t, lines[0], "do-not-log")
	assert.NotContains(t, lines[0], "owner;secret")
}

// TestHostProcessLifecycleSnapshotBoundsCollectionAndOutput checks oversized pod-status input.
// The snapshot reports omitted statuses and bounds its line without exposing owner or free-form message data.
func TestHostProcessLifecycleSnapshotBoundsCollectionAndOutput(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, time.October, 4, 8, 0, 0, 0, time.UTC)
	pod := testHostProcessPod(types.UID(strings.Repeat("uid-secret", 100)), corev1.PodFailed, when)
	pod.OwnerReferences = make([]metav1.OwnerReference, 1000)
	for i := range pod.OwnerReferences {
		pod.OwnerReferences[i] = metav1.OwnerReference{Name: fmt.Sprintf("owner-secret-%d", i)}
	}
	pod.Status.ContainerStatuses = append([]corev1.ContainerStatus{{
		Name: pod.Name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 42,
			Reason:   "Error",
			Message:  strings.Repeat("free-form-secret", 100),
		}},
	}}, make([]corev1.ContainerStatus, 1000)...)
	for i := 1; i < len(pod.Status.ContainerStatuses); i++ {
		pod.Status.ContainerStatuses[i] = corev1.ContainerStatus{
			Name: fmt.Sprintf("unrelated-secret-%d", i),
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  "Waiting",
				Message: strings.Repeat("unrelated-free-form-secret", 100),
			}},
		}
	}

	lines := (&hostProcessPodState{}).observe(pod, when.Add(time.Second))

	require.Len(t, lines, 1)
	assert.LessOrEqual(t, len(lines[0]), hostProcessSnapshotMaxBytes)
	assert.Contains(t, lines[0], "uid=redacted")
	assert.Contains(t, lines[0], "containerStatus={name=hostprocess-pod,state=terminated,reason=Error,exitCode=42}")
	assert.Contains(t, lines[0], "containerStatusesOmitted=1000")
	assert.Contains(t, lines[0], "containerStatusScanCapped=false")
	assert.NotContains(t, lines[0], "owner-secret")
	assert.NotContains(t, lines[0], "free-form-secret")
	assert.NotContains(t, lines[0], "unrelated-secret")
}

// TestHostProcessLifecycleSnapshotCapsStatusSearch checks the finite container-status scan.
// A matching container beyond the scan limit stays missing, with an explicit capped-search flag and omitted count.
func TestHostProcessLifecycleSnapshotCapsStatusSearch(t *testing.T) {
	t.Parallel()

	pod := testHostProcessPod("pod-uid", corev1.PodFailed, time.Now())
	pod.Status.ContainerStatuses = make([]corev1.ContainerStatus, hostProcessSnapshotStatusLimit+100)
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].Name = fmt.Sprintf("unrelated-%d", i)
	}
	pod.Status.ContainerStatuses[hostProcessSnapshotStatusLimit] = corev1.ContainerStatus{
		Name: pod.Name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 99,
			Reason:   "secret-beyond-finite-scan",
			Message:  "free-form-message-beyond-finite-scan",
		}},
	}

	line := formatHostProcessPodSnapshot("observed", newHostProcessPodSnapshot(pod, time.Now()))

	assert.LessOrEqual(t, len(line), hostProcessSnapshotMaxBytes)
	assert.Contains(t, line, "containerStatus=missing")
	assert.Contains(t, line, fmt.Sprintf("containerStatusesOmitted=%d", len(pod.Status.ContainerStatuses)))
	assert.Contains(t, line, "containerStatusScanCapped=true")
	assert.NotContains(t, line, "secret-beyond-finite-scan")
	assert.NotContains(t, line, "free-form-message-beyond-finite-scan")
}

// TestBoundHostProcessSnapshotOutputHasExactLimitAndMarker checks snapshot truncation boundaries.
// Oversized output must have exactly the configured byte length and end with the truncation marker.
func TestBoundHostProcessSnapshotOutputHasExactLimitAndMarker(t *testing.T) {
	t.Parallel()

	line := boundHostProcessSnapshotOutput(strings.Repeat("x", hostProcessSnapshotMaxBytes+100))

	assert.Len(t, line, hostProcessSnapshotMaxBytes)
	assert.True(t, strings.HasSuffix(line, " outputTruncated=true"))
}

// TestHostProcessLifecycleSuppressesRepeatedReadFailures checks category-based failure suppression.
// Repeated service-unavailable messages are quiet, but a change to cancellation emits a new safe category.
func TestHostProcessLifecycleSuppressesRepeatedReadFailures(t *testing.T) {
	t.Parallel()

	state := &hostProcessPodState{}
	first := state.observeReadFailure(apierrors.NewServiceUnavailable("free-form server response"))
	repeated := state.observeReadFailure(apierrors.NewServiceUnavailable("different server response"))
	canceled := state.observeReadFailure(context.Canceled)

	require.Len(t, first, 1)
	assert.Contains(t, first[0], "category=service-unavailable")
	assert.NotContains(t, first[0], "free-form server response")
	assert.Empty(t, repeated)
	require.Len(t, canceled, 1)
	assert.Contains(t, canceled[0], "category=canceled")
}

// TestCollectHostProcessEventDiagnosticsFiltersAndBoundsOutput checks event query and output limits.
// UID/namespace selectors, bounded summaries, and pagination caveats remain visible without free-form event messages.
func TestCollectHostProcessEventDiagnosticsFiltersAndBoundsOutput(t *testing.T) {
	t.Parallel()

	uid := types.UID("pod-uid")
	base := time.Date(2026, time.October, 4, 8, 0, 0, 0, time.UTC)
	events := make([]corev1.Event, 0, hostProcessDiagnosticMaxEvents+1)
	for i := 0; i < hostProcessDiagnosticMaxEvents+1; i++ {
		events = append(events, corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Second))},
			InvolvedObject: corev1.ObjectReference{UID: uid},
			Type:           corev1.EventTypeWarning,
			Reason:         fmt.Sprintf("Reason%d", i),
			Message:        "free-form event message must not be logged",
			Count:          int32(i + 1),
			Source:         corev1.EventSource{Component: "kubelet"},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostProcessDiagnosticTimeout)
	defer cancel()
	lines := collectHostProcessEventDiagnostics(ctx, "test-namespace", uid,
		func(ctx context.Context, options metav1.ListOptions) (*corev1.EventList, error) {
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			assert.Equal(t, "involvedObject.namespace=test-namespace,involvedObject.uid=pod-uid", options.FieldSelector)
			assert.Equal(t, int64(hostProcessDiagnosticMaxEvents+1), options.Limit)
			return &corev1.EventList{ListMeta: metav1.ListMeta{Continue: "next-page"}, Items: events}, nil
		})

	require.Len(t, lines, hostProcessDiagnosticMaxEvents+1)
	assert.Contains(t, lines[0], "received=9 shown=8 omittedFromPage=1 moreAvailable=true")
	assert.Contains(t, lines[0], "not guaranteed to contain newest events")
	assert.Contains(t, lines[0], "do not identify a deletion actor")
	assert.Contains(t, lines[1], "reason=Reason1")
	joined := strings.Join(lines, "\n")
	assert.NotContains(t, joined, "free-form event message")
}

// TestCollectHostProcessEventDiagnosticsRedactsUnsafeFields checks unsafe event-field handling.
// Type, reason, and source values are redacted instead of leaking delimiter-bearing or free-form payloads.
func TestCollectHostProcessEventDiagnosticsRedactsUnsafeFields(t *testing.T) {
	t.Parallel()

	uid := types.UID("pod-uid")
	lines := collectHostProcessEventDiagnostics(context.Background(), "test-namespace", uid,
		func(context.Context, metav1.ListOptions) (*corev1.EventList, error) {
			return &corev1.EventList{Items: []corev1.Event{{
				InvolvedObject: corev1.ObjectReference{UID: uid},
				Type:           "Warning;token=value",
				Reason:         "unsafe reason with spaces",
				Message:        "credential-like free-form payload",
				Source:         corev1.EventSource{Component: "source;credential"},
			}}}, nil
		})

	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "type=redacted")
	assert.Contains(t, joined, "reason=redacted")
	assert.Contains(t, joined, "source=redacted")
	assert.NotContains(t, joined, "token=value")
	assert.NotContains(t, joined, "credential-like")
}

// TestCollectHostProcessEventDiagnosticsReportsUnavailableAndEmpty checks missing event evidence.
// Forbidden, canceled, and empty results stay informational, preserve an unknown cause, and omit server details.
func TestCollectHostProcessEventDiagnosticsReportsUnavailableAndEmpty(t *testing.T) {
	t.Parallel()

	uid := types.UID("pod-uid")
	tests := []struct {
		name     string
		ctx      context.Context
		list     hostProcessEventLister
		expected string
	}{
		{
			name: "forbidden",
			ctx:  context.Background(),
			list: func(context.Context, metav1.ListOptions) (*corev1.EventList, error) {
				return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("denied detail"))
			},
			expected: "category=forbidden; cause remains unknown",
		},
		{
			name: "canceled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			list: func(ctx context.Context, _ metav1.ListOptions) (*corev1.EventList, error) {
				return nil, ctx.Err()
			},
			expected: "category=canceled; cause remains unknown",
		},
		{
			name: "empty",
			ctx:  context.Background(),
			list: func(context.Context, metav1.ListOptions) (*corev1.EventList, error) {
				return &corev1.EventList{}, nil
			},
			expected: "matched=0; cause remains unknown",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			lines := collectHostProcessEventDiagnostics(test.ctx, "test-namespace", uid, test.list)
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], test.expected)
			assert.NotContains(t, lines[0], "denied detail")
		})
	}
}

// TestCollectHostProcessEventDiagnosticsRejectsLateSuccess checks expired event-query responses.
// A successful response returned after context expiry is replaced by a timeout summary rather than displayed.
func TestCollectHostProcessEventDiagnosticsRejectsLateSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	lines := collectHostProcessEventDiagnostics(ctx, "test-namespace", "pod-uid",
		func(ctx context.Context, _ metav1.ListOptions) (*corev1.EventList, error) {
			<-ctx.Done()
			return &corev1.EventList{Items: []corev1.Event{{
				InvolvedObject: corev1.ObjectReference{UID: "pod-uid"}, Reason: "LateSuccess",
			}}}, nil
		})

	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "category=timeout")
	assert.NotContains(t, lines[0], "LateSuccess")
}

// TestHostProcessDiagnosticsDoNotMaskCompletionFailure checks secondary diagnostic failures.
// The original completion error remains unwrap-compatible and is not replaced by the event-query error.
func TestHostProcessDiagnosticsDoNotMaskCompletionFailure(t *testing.T) {
	t.Parallel()

	originalErr := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "hostprocess-pod", errors.New("denied"))
	var diagnosticLines []string
	_, waitErr := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "pod-uid",
		func(context.Context) (*corev1.Pod, error) { return nil, originalErr },
		&hostProcessPodState{}, nil, func(ctx context.Context, uid types.UID) {
			diagnosticLines = collectHostProcessEventDiagnostics(ctx, "test-namespace", uid,
				func(context.Context, metav1.ListOptions) (*corev1.EventList, error) {
					return nil, apierrors.NewServiceUnavailable("diagnostic service failed")
				})
		}, testHostProcessCompletionConfig())
	err := hostProcessCompletionError("hostprocess-pod", waitErr)

	require.ErrorIs(t, waitErr, originalErr)
	require.Len(t, diagnosticLines, 1)
	assert.Contains(t, diagnosticLines[0], "event-diagnostics unavailable")
	assert.ErrorIs(t, err, originalErr)
	assert.Contains(t, err.Error(), "HostProcess pod hostprocess-pod did not complete")
	assert.NotContains(t, err.Error(), "diagnostic service failed")
}

// testHostProcessCompletionConfig provides fast completion polling for unit tests.
// It shortens poll/read budgets and uses a three-error limit without changing the production defaults.
func testHostProcessCompletionConfig() hostProcessCompletionConfig {
	config := defaultHostProcessCompletionConfig()
	config.overallTimeout = 200 * time.Millisecond
	config.pollInterval = time.Millisecond
	config.perRequestTimeout = 20 * time.Millisecond
	config.maxConsecutiveReadErrors = 3
	return config
}

// TestHostProcessCompletionBounds checks production polling and operation limits.
// Completion, read, diagnostic, mutation, and consecutive-error settings must retain their expected values.
func TestHostProcessCompletionBounds(t *testing.T) {
	t.Parallel()

	config := defaultHostProcessCompletionConfig()
	assert.Equal(t, 10*time.Minute, config.overallTimeout)
	assert.Equal(t, time.Second, config.pollInterval)
	assert.Equal(t, 10*time.Second, config.perRequestTimeout)
	assert.Equal(t, 5, config.maxConsecutiveReadErrors)
	assert.Equal(t, 5*time.Second, hostProcessDiagnosticTimeout)
	assert.Equal(t, 30*time.Second, hostProcessMutationTimeout)
}

// TestWaitForHostProcessPodCompletionExitsWhenObservedPodDisappears checks a missing previously seen pod.
// The original UID and last running phase remain saved, with one diagnostic attempt and no further polling.
func TestWaitForHostProcessPodCompletionExitsWhenObservedPodDisappears(t *testing.T) {
	t.Parallel()

	pods := schema.GroupResource{Resource: "pods"}
	calls := 0
	diagnosticCalls := 0
	state := &hostProcessPodState{}
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "original-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			if calls == 1 {
				return testHostProcessPod("original-uid", corev1.PodRunning, time.Now()), nil
			}
			return nil, apierrors.NewNotFound(pods, "hostprocess-pod")
		}, state, nil, func(_ context.Context, uid types.UID) {
			diagnosticCalls++
			assert.Equal(t, types.UID("original-uid"), uid)
		}, testHostProcessCompletionConfig())

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
	assert.Contains(t, err.Error(), "uid original-uid disappeared")
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, diagnosticCalls)
	require.NotNil(t, state.last)
	assert.Equal(t, corev1.PodRunning, state.last.phase)
	assert.Equal(t, types.UID("original-uid"), state.last.uid)
}

// TestWaitForHostProcessPodCompletionFirstLookupMissing checks disappearance before any observation.
// The first not-found result reports the creation UID and never-observed state, then attempts diagnostics once.
func TestWaitForHostProcessPodCompletionFirstLookupMissing(t *testing.T) {
	t.Parallel()

	calls := 0
	diagnosticCalls := 0
	var logLines []string
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "created-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "hostprocess-pod")
		}, &hostProcessPodState{}, func(lines []string) {
			logLines = append(logLines, lines...)
		}, func(_ context.Context, uid types.UID) {
			diagnosticCalls++
			assert.Equal(t, types.UID("created-uid"), uid)
		}, testHostProcessCompletionConfig())

	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
	assert.Contains(t, err.Error(), "disappeared before its first status observation")
	assert.Equal(t, 1, calls)
	assert.Equal(t, 1, diagnosticCalls)
	assert.Contains(t, strings.Join(logLines, "\n"), "state=never-observed")
	assert.Contains(t, strings.Join(logLines, "\n"), "uid=created-uid")
}

// TestWaitForHostProcessPodCompletionRejectsChangedUID checks same-name pod replacement.
// A replacement's successful phase cannot overwrite the saved original UID or satisfy completion.
func TestWaitForHostProcessPodCompletionRejectsChangedUID(t *testing.T) {
	t.Parallel()

	calls := 0
	diagnosticCalls := 0
	state := &hostProcessPodState{}
	var logLines []string
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "original-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			if calls == 1 {
				return testHostProcessPod("original-uid", corev1.PodRunning, time.Now()), nil
			}
			return testHostProcessPod("replacement-uid", corev1.PodSucceeded, time.Now()), nil
		}, state, func(lines []string) {
			logLines = append(logLines, lines...)
		}, func(_ context.Context, uid types.UID) {
			diagnosticCalls++
			assert.Equal(t, types.UID("original-uid"), uid)
		}, testHostProcessCompletionConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "uid changed from original-uid to replacement-uid")
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, diagnosticCalls)
	require.NotNil(t, state.last)
	assert.Equal(t, types.UID("original-uid"), state.last.uid, "replacement must not become saved state")
	assert.Equal(t, corev1.PodRunning, state.last.phase)
	assert.Contains(t, strings.Join(logLines, "\n"), "event=replaced")
}

// TestWaitForHostProcessPodCompletionBoundsStalledRequest checks cooperative status-read timeouts.
// Read callbacks observe their context deadlines, and the consecutive-error limit stops further polling.
func TestWaitForHostProcessPodCompletionBoundsStalledRequest(t *testing.T) {
	t.Parallel()

	config := testHostProcessCompletionConfig()
	config.maxConsecutiveReadErrors = 2
	calls := 0
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "created-uid",
		func(ctx context.Context) (*corev1.Pod, error) {
			calls++
			<-ctx.Done()
			return nil, ctx.Err()
		}, &hostProcessPodState{}, nil, nil, config)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "2 consecutive times")
	assert.Equal(t, 2, calls)
}

// TestWaitForHostProcessPodCompletionRejectsLateSuccess checks a late successful status response.
// An expired read cannot establish a terminal phase, even when its callback returns a succeeded pod.
func TestWaitForHostProcessPodCompletionRejectsLateSuccess(t *testing.T) {
	config := testHostProcessCompletionConfig()
	config.maxConsecutiveReadErrors = 1
	config.perRequestTimeout = 5 * time.Millisecond
	calls := 0

	phase, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "created-uid",
		func(ctx context.Context) (*corev1.Pod, error) {
			calls++
			<-ctx.Done()
			return testHostProcessPod("created-uid", corev1.PodSucceeded, time.Now()), nil
		}, &hostProcessPodState{}, nil, nil, config)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, corev1.PodUnknown, phase)
	assert.Equal(t, 1, calls)
}

// TestWaitForHostProcessPodCompletionStopsAfterFiniteTransientErrors checks repeated transient failures.
// The test's three-error limit terminates polling instead of allowing an unbounded error sequence.
func TestWaitForHostProcessPodCompletionStopsAfterFiniteTransientErrors(t *testing.T) {
	t.Parallel()

	calls := 0
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "created-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			return nil, apierrors.NewServiceUnavailable("temporary failure")
		}, &hostProcessPodState{}, nil, nil, testHostProcessCompletionConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "3 consecutive times")
	assert.Equal(t, 3, calls)
}

// TestWaitForHostProcessPodCompletionResetsTransientErrorCount checks recovery between read failures.
// A successful running observation resets the consecutive-error count, allowing later completion.
func TestWaitForHostProcessPodCompletionResetsTransientErrorCount(t *testing.T) {
	t.Parallel()

	calls := 0
	phase, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "pod-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			switch calls {
			case 1, 2, 4, 5:
				return nil, apierrors.NewServiceUnavailable("temporary failure")
			case 3:
				return testHostProcessPod("pod-uid", corev1.PodRunning, time.Now()), nil
			default:
				return testHostProcessPod("pod-uid", corev1.PodSucceeded, time.Now()), nil
			}
		}, &hostProcessPodState{}, nil, nil, testHostProcessCompletionConfig())

	require.NoError(t, err)
	assert.Equal(t, corev1.PodSucceeded, phase)
	assert.Equal(t, 6, calls)
}

// TestWaitForHostProcessPodCompletionEnforcesOverallDeadline checks the completion polling budget.
// A persistently running pod ends with a deadline error and one UID-pinned, deadline-bearing diagnostic attempt.
func TestWaitForHostProcessPodCompletionEnforcesOverallDeadline(t *testing.T) {
	t.Parallel()

	config := testHostProcessCompletionConfig()
	config.overallTimeout = 30 * time.Millisecond
	diagnosticCalls := 0
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "pod-uid",
		func(context.Context) (*corev1.Pod, error) {
			return testHostProcessPod("pod-uid", corev1.PodRunning, time.Now()), nil
		}, &hostProcessPodState{}, nil, func(ctx context.Context, uid types.UID) {
			diagnosticCalls++
			assert.Equal(t, types.UID("pod-uid"), uid)
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
		}, config)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, diagnosticCalls)
}

// TestWaitForHostProcessPodCompletionHonorsCallerCancellation checks cancellation in a status read.
// The callback observes the canceled request context and the completion wait returns the caller's error.
func TestWaitForHostProcessPodCompletionHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestStarted := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		_, err := waitForHostProcessPodCompletion(ctx, "hostprocess-pod", "created-uid",
			func(requestCtx context.Context) (*corev1.Pod, error) {
				close(requestStarted)
				<-requestCtx.Done()
				return nil, requestCtx.Err()
			}, &hostProcessPodState{}, nil, nil, testHostProcessCompletionConfig())
		result <- err
	}()
	<-requestStarted
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("completion wait did not observe caller cancellation")
	}
}

// TestWaitForHostProcessPodCompletionFailsFastOnForbidden checks terminal authorization failure.
// A forbidden status read preserves the API error and stops after the first request.
func TestWaitForHostProcessPodCompletionFailsFastOnForbidden(t *testing.T) {
	t.Parallel()

	calls := 0
	_, err := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "created-uid",
		func(context.Context) (*corev1.Pod, error) {
			calls++
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "hostprocess-pod",
				errors.New("denied"))
		}, &hostProcessPodState{}, nil, nil, testHostProcessCompletionConfig())

	require.Error(t, err)
	assert.True(t, apierrors.IsForbidden(err))
	assert.Equal(t, 1, calls)
}

// TestFailedHostProcessCompletionPreservesCommandFailure checks a failed pod with unreadable logs.
// Observing the Failed phase still produces a command failure that wraps the log error.
func TestFailedHostProcessCompletionPreservesCommandFailure(t *testing.T) {
	t.Parallel()

	phase, waitErr := waitForHostProcessPodCompletion(context.Background(), "hostprocess-pod", "pod-uid",
		func(context.Context) (*corev1.Pod, error) {
			return testHostProcessPod("pod-uid", corev1.PodFailed, time.Now()), nil
		}, &hostProcessPodState{}, nil, nil, testHostProcessCompletionConfig())
	require.NoError(t, waitErr)

	logErr := errors.New("log retrieval failed")
	_, err := hostProcessCommandResult("windows-node", "hostprocess-pod", phase, nil, logErr)

	require.Error(t, err)
	assert.ErrorIs(t, err, logErr)
	assert.Contains(t, err.Error(), "command in pod hostprocess-pod on windows-node failed")
}

// TestNewHostProcessPodMatchesOcRunShape checks the typed pod's legacy command shape.
// Windows/SYSTEM security, networking, placement, toleration, and empty annotations match the expected specification.
func TestNewHostProcessPodMatchesOcRunShape(t *testing.T) {
	pod := newHostProcessPod("hostprocess-pod", "windows-node", "test-image", "Get-Service kubelet")

	assert.Equal(t, "hostprocess-pod", pod.Name)
	assert.Equal(t, wmcoNamespace, pod.Namespace)
	assert.Equal(t, map[string]string{"run": "hostprocess-pod"}, pod.Labels)
	assert.Empty(t, pod.Annotations)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, "hostprocess-pod", container.Name)
	assert.Equal(t, "test-image", container.Image)
	assert.Equal(t, []string{"powershell.exe", "-Command", "Get-Service kubelet"}, container.Command)
	assert.Equal(t, corev1.ResourceRequirements{}, container.Resources)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	assert.Equal(t, corev1.DNSClusterFirst, pod.Spec.DNSPolicy)
	assert.True(t, pod.Spec.HostNetwork)
	assert.Equal(t, map[string]string{"kubernetes.io/hostname": "windows-node"}, pod.Spec.NodeSelector)
	require.NotNil(t, pod.Spec.OS)
	assert.Equal(t, corev1.Windows, pod.Spec.OS.Name)
	require.NotNil(t, pod.Spec.SecurityContext.WindowsOptions)
	require.NotNil(t, container.SecurityContext.WindowsOptions)
	assert.True(t, *pod.Spec.SecurityContext.WindowsOptions.HostProcess)
	assert.Equal(t, "NT AUTHORITY\\SYSTEM", *pod.Spec.SecurityContext.WindowsOptions.RunAsUserName)
	assert.True(t, *container.SecurityContext.WindowsOptions.HostProcess)
	assert.Equal(t, "NT AUTHORITY\\SYSTEM", *container.SecurityContext.WindowsOptions.RunAsUserName)
	assert.Equal(t, []corev1.Toleration{{
		Key: "os", Operator: corev1.TolerationOpEqual, Value: "Windows", Effect: corev1.TaintEffectNoSchedule,
	}}, pod.Spec.Tolerations)
}

// TestCreateHostProcessPodPreservesUIDAndBoundsRequest checks the pod creation callback contract.
// The original pod object receives a deadline-bearing context, and the returned creation UID is retained.
func TestCreateHostProcessPodPreservesUIDAndBoundsRequest(t *testing.T) {
	wanted := newHostProcessPod("hostprocess-pod", "windows-node", "test-image", "safe-command")
	created, err := createHostProcessPod(context.Background(), wanted,
		func(ctx context.Context, received *corev1.Pod) (*corev1.Pod, error) {
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			assert.Same(t, wanted, received)
			result := received.DeepCopy()
			result.UID = "created-uid"
			return result, nil
		})

	require.NoError(t, err)
	assert.Equal(t, types.UID("created-uid"), created.UID)
}

// TestCreateHostProcessPodRejectsLateSuccessAfterCallerCancellation checks canceled creation responses.
// Cancellation remains the error, but a late returned UID stays available for cleanup.
func TestCreateHostProcessPodRejectsLateSuccessAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestStarted := make(chan struct{})
	type createResult struct {
		pod *corev1.Pod
		err error
	}
	result := make(chan createResult, 1)
	go func() {
		pod, err := createHostProcessPod(ctx, &corev1.Pod{}, func(requestCtx context.Context, _ *corev1.Pod) (*corev1.Pod, error) {
			close(requestStarted)
			<-requestCtx.Done()
			return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "late-uid"}}, nil
		})
		result <- createResult{pod: pod, err: err}
	}()
	<-requestStarted
	cancel()

	select {
	case result := <-result:
		require.ErrorIs(t, result.err, context.Canceled)
		require.NotNil(t, result.pod, "returned UID must remain available for cleanup")
		assert.Equal(t, types.UID("late-uid"), result.pod.UID)
	case <-time.After(time.Second):
		t.Fatal("typed create did not observe caller cancellation")
	}
}

// TestCreateHostProcessPodSanitizesAPIErrorsWithoutRetry checks ambiguous creation failure handling.
// Only one request is made, and the returned error exposes a safe category rather than free-form server details.
func TestCreateHostProcessPodSanitizesAPIErrorsWithoutRetry(t *testing.T) {
	calls := 0
	_, err := createHostProcessPod(context.Background(), &corev1.Pod{},
		func(context.Context, *corev1.Pod) (*corev1.Pod, error) {
			calls++
			return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "hostprocess-pod",
				errors.New("PowerShell secret output"))
		})

	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Contains(t, err.Error(), "category=forbidden")
	assert.NotContains(t, err.Error(), "PowerShell")
	assert.NotContains(t, err.Error(), "secret")
}

// TestCreateHostProcessPodPreservesUIDWithSanitizedAPIError checks an error with a returned pod.
// The created object and UID remain available while unsafe API error details are replaced by a category.
func TestCreateHostProcessPodPreservesUIDWithSanitizedAPIError(t *testing.T) {
	createdPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "created-uid"}}
	created, err := createHostProcessPod(context.Background(), &corev1.Pod{},
		func(context.Context, *corev1.Pod) (*corev1.Pod, error) {
			return createdPod, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "hostprocess-pod",
				errors.New("PowerShell secret output"))
		})

	require.Error(t, err)
	assert.Same(t, createdPod, created)
	assert.Equal(t, types.UID("created-uid"), created.UID)
	assert.Contains(t, err.Error(), "category=forbidden")
	assert.NotContains(t, err.Error(), "PowerShell")
	assert.NotContains(t, err.Error(), "secret")
}

// TestCleanupHostProcessPodUsesIndependentContextAndUIDPrecondition checks the independent delete request.
// One deadline-bearing cleanup uses the creation UID and background propagation despite caller cancellation.
func TestCleanupHostProcessPodUsesIndependentContextAndUIDPrecondition(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	require.ErrorIs(t, callerCtx.Err(), context.Canceled)

	calls := 0
	err := cleanupHostProcessPod("hostprocess-pod", "created-uid",
		func(ctx context.Context, name string, options metav1.DeleteOptions) error {
			calls++
			assert.NoError(t, ctx.Err(), "cleanup context must remain usable after caller cancellation")
			_, hasDeadline := ctx.Deadline()
			assert.True(t, hasDeadline)
			assert.Equal(t, "hostprocess-pod", name)
			require.NotNil(t, options.Preconditions)
			require.NotNil(t, options.Preconditions.UID)
			assert.Equal(t, types.UID("created-uid"), *options.Preconditions.UID)
			require.NotNil(t, options.PropagationPolicy)
			assert.Equal(t, metav1.DeletePropagationBackground, *options.PropagationPolicy)
			return nil
		})

	require.NoError(t, err)
	assert.Equal(t, 1, calls)
}

// TestCleanupHostProcessPodHandlesNotFoundAndRefusesUIDConflict checks delete outcome handling.
// An already absent pod is accepted, while a UID-precondition conflict remains an error.
func TestCleanupHostProcessPodHandlesNotFoundAndRefusesUIDConflict(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "hostprocess-pod")
	require.NoError(t, cleanupHostProcessPod("hostprocess-pod", "created-uid",
		func(context.Context, string, metav1.DeleteOptions) error { return notFound }))

	uidConflict := apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, "hostprocess-pod",
		errors.New("UID precondition failed"))
	err := cleanupHostProcessPod("hostprocess-pod", "created-uid",
		func(context.Context, string, metav1.DeleteOptions) error { return uidConflict })
	require.ErrorIs(t, err, uidConflict)
}

// TestGetHostProcessLogsForUIDRejectsReplacement checks identity rejection before log retrieval.
// A same-name replacement produces a UID error without invoking the log callback.
func TestGetHostProcessLogsForUIDRejectsReplacement(t *testing.T) {
	logCalls := 0
	_, err := getHostProcessLogsForUID(context.Background(), "hostprocess-pod", "created-uid",
		func(context.Context) (*corev1.Pod, error) {
			return testHostProcessPod("replacement-uid", corev1.PodSucceeded, time.Now()), nil
		}, func(context.Context) ([]byte, error) {
			logCalls++
			return []byte("replacement logs"), nil
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "uid changed from created-uid to replacement-uid")
	assert.Equal(t, 0, logCalls)
}

// TestGetHostProcessLogsForUIDChecksIdentityBeforeLogs checks ordering for a matching pod UID.
// The pod identity callback runs before the log callback, and successful original-pod output is preserved.
func TestGetHostProcessLogsForUIDChecksIdentityBeforeLogs(t *testing.T) {
	sequence := make([]string, 0, 2)
	output, err := getHostProcessLogsForUID(context.Background(), "hostprocess-pod", "created-uid",
		func(context.Context) (*corev1.Pod, error) {
			sequence = append(sequence, "get")
			return testHostProcessPod("created-uid", corev1.PodSucceeded, time.Now()), nil
		}, func(context.Context) ([]byte, error) {
			sequence = append(sequence, "logs")
			return []byte("original logs"), nil
		})

	require.NoError(t, err)
	assert.Equal(t, []byte("original logs"), output)
	assert.Equal(t, []string{"get", "logs"}, sequence)
}

// TestRetrieveHostProcessLogsRejectsReplacementBeforeSecondLogRequest checks identity across a log retry.
// After a transient first read, a replacement is rejected before a second name-addressed log request.
func TestRetrieveHostProcessLogsRejectsReplacementBeforeSecondLogRequest(t *testing.T) {
	getCalls := 0
	logCalls := 0
	_, err := retrieveHostProcessLogs(context.Background(), func(ctx context.Context) ([]byte, error) {
		return getHostProcessLogsForUID(ctx, "hostprocess-pod", "created-uid",
			func(context.Context) (*corev1.Pod, error) {
				getCalls++
				if getCalls == 1 {
					return testHostProcessPod("created-uid", corev1.PodSucceeded, time.Now()), nil
				}
				return testHostProcessPod("replacement-uid", corev1.PodSucceeded, time.Now()), nil
			}, func(context.Context) ([]byte, error) {
				logCalls++
				return nil, apierrors.NewServiceUnavailable("temporary log failure")
			})
	}, testHostProcessLogRetryConfig())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "uid changed from created-uid to replacement-uid")
	assert.Equal(t, 2, getCalls)
	assert.Equal(t, 1, logCalls, "replacement must be rejected before a second log request")
}
