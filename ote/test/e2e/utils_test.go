package winc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type hostProcessOperationsFixture struct {
	createCalls      int
	waitCalls        int
	logCalls         int
	diagnosticsCalls int
	cleanupCalls     int

	createErr          error
	wait               func(context.Context) (string, error)
	retrieve           hostProcessLogRunner
	terminationDetails func(context.Context) (string, string)
}

func newHostProcessOperationsFixture() *hostProcessOperationsFixture {
	return &hostProcessOperationsFixture{
		wait: func(context.Context) (string, error) { return "Succeeded", nil },
		retrieve: func(context.Context, string) (string, error) {
			return "PowerShell completed", nil
		},
		terminationDetails: func(context.Context) (string, string) { return "0", "Completed" },
	}
}

func (f *hostProcessOperationsFixture) operations() hostProcessPodOperations {
	return hostProcessPodOperations{
		create: func() error {
			f.createCalls++
			return f.createErr
		},
		cleanup: func() { f.cleanupCalls++ },
		waitForTerminalPhase: func(ctx context.Context) (string, error) {
			f.waitCalls++
			return f.wait(ctx)
		},
		retrieveLogs: func(ctx context.Context, podName string) (string, error) {
			f.logCalls++
			return f.retrieve(ctx, podName)
		},
		terminationDetails: func(ctx context.Context) (string, string) {
			f.diagnosticsCalls++
			return f.terminationDetails(ctx)
		},
	}
}

func testHostProcessLogRetryConfig() hostProcessLogRetryConfig {
	config := defaultHostProcessLogRetryConfig()
	config.overallTimeout = time.Second
	config.perRequestTimeout = 100 * time.Millisecond
	config.retryDelay = 0
	return config
}

func TestHostProcessLogRetryBounds(t *testing.T) {
	t.Parallel()

	config := defaultHostProcessLogRetryConfig()
	assert.Equal(t, 5, config.maxAttempts)
	assert.Equal(t, 90*time.Second, config.overallTimeout)
	assert.Equal(t, 10*time.Second, config.perRequestTimeout)
}

func TestSafeHostProcessStatusFields(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Succeeded", safeHostProcessPhase("Succeeded"))
	assert.Equal(t, "unknown", safeHostProcessPhase("Succeeded secret-node.internal"))
	assert.Equal(t, "137", safeHostProcessExitCode("137"))
	assert.Equal(t, "unknown", safeHostProcessExitCode("137 secret-token"))
	assert.Equal(t, "OOMKilled", safeHostProcessTerminationReason("OOMKilled"))
	assert.Equal(t, "unknown", safeHostProcessTerminationReason("Error: password=top-secret"))
}

var transientHostProcessErrors = []string{
	"read tcp: connection reset by peer",
	"connect: connection refused",
	"net/http: TLS handshake timeout",
	"remote error: tls: internal error",
	"http2: client connection lost",
	"server sent GOAWAY",
	"Error from server (ServiceUnavailable)",
	"Error from server (InternalError)",
	"Error from server (Timeout)",
	"Error from server (TooManyRequests)",
	"unexpected EOF",
	"request timeout",
	"request timed out",
	"timeout awaiting response headers",
	"Client.Timeout exceeded",
	"context deadline exceeded",
	"i/o timeout",
}

func TestRetrieveHostProcessLogsRetriesEachTransientError(t *testing.T) {
	t.Parallel()

	for _, transientError := range transientHostProcessErrors {
		transientError := transientError
		t.Run(transientError, func(t *testing.T) {
			t.Parallel()
			attempts := 0
			output, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod",
				func(context.Context, string) (string, error) {
					attempts++
					if attempts == 1 {
						return "", errors.New(transientError)
					}
					return "command output", nil
				}, testHostProcessLogRetryConfig())

			require.NoError(t, err)
			assert.Equal(t, "command output", output)
			assert.Equal(t, 2, attempts)
		})
	}
}

func TestRetrieveHostProcessLogsRetriesResetThenSucceeds(t *testing.T) {
	t.Parallel()

	attempts := 0
	runner := func(ctx context.Context, podName string) (string, error) {
		attempts++
		assert.Equal(t, "hostprocess-pod", podName)
		_, hasDeadline := ctx.Deadline()
		assert.True(t, hasDeadline)
		if attempts == 1 {
			return "", errors.New("read tcp: connection reset by peer")
		}
		return "command output", nil
	}

	output, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod", runner,
		testHostProcessLogRetryConfig())
	require.NoError(t, err)
	assert.Equal(t, "command output", output)
	assert.Equal(t, 2, attempts)
}

func TestRetrieveHostProcessLogsTerminalErrorsTakePrecedence(t *testing.T) {
	t.Parallel()

	terminalErrors := []string{
		"Error from server (Forbidden)",
		"Error from server (Unauthorized)",
		"Error from server (NotFound)",
		`pods "hostprocess-pod" not found`,
	}
	for _, terminalError := range terminalErrors {
		for _, transientError := range transientHostProcessErrors {
			name := terminalError + " with " + transientError
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				attempts := 0
				_, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod",
					func(context.Context, string) (string, error) {
						attempts++
						return "", errors.New(terminalError + ": transport also reported " + transientError)
					}, testHostProcessLogRetryConfig())

				require.Error(t, err)
				assert.Equal(t, 1, attempts)
				var retrievalErr *hostProcessLogRetrievalError
				require.ErrorAs(t, err, &retrievalErr)
				assert.False(t, retrievalErr.exhausted)
			})
		}
	}
}

func TestRetrieveHostProcessLogsRequestDeadlineCancelsBlockedRunner(t *testing.T) {
	config := testHostProcessLogRetryConfig()
	config.maxAttempts = 1
	config.perRequestTimeout = 60 * time.Millisecond
	config.overallTimeout = time.Second
	started := time.Now()
	var requestDeadline time.Time

	_, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod",
		func(ctx context.Context, _ string) (string, error) {
			var ok bool
			requestDeadline, ok = ctx.Deadline()
			require.True(t, ok)
			<-ctx.Done()
			return "", ctx.Err()
		}, config)

	require.Error(t, err)
	assert.WithinDuration(t, started.Add(config.perRequestTimeout), requestDeadline, 25*time.Millisecond)
	assert.GreaterOrEqual(t, time.Since(started), config.perRequestTimeout-20*time.Millisecond)
}

func TestRetrieveHostProcessLogsOverallDeadlineWins(t *testing.T) {
	config := testHostProcessLogRetryConfig()
	config.perRequestTimeout = time.Second
	config.overallTimeout = 60 * time.Millisecond
	started := time.Now()
	var requestDeadline time.Time
	attempts := 0

	_, err := retrieveHostProcessLogs(context.Background(), "hostprocess-pod",
		func(ctx context.Context, _ string) (string, error) {
			attempts++
			requestDeadline, _ = ctx.Deadline()
			<-ctx.Done()
			return "", ctx.Err()
		}, config)

	require.Error(t, err)
	assert.WithinDuration(t, started.Add(config.overallTimeout), requestDeadline, 25*time.Millisecond)
	assert.Equal(t, 1, attempts)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
}

func TestRetrieveHostProcessLogsParentCancellationStopsBlockedRunner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	attempts := 0

	go func() {
		_, err := retrieveHostProcessLogs(ctx, "hostprocess-pod",
			func(requestCtx context.Context, _ string) (string, error) {
				attempts++
				close(started)
				<-requestCtx.Done()
				return "", requestCtx.Err()
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
		t.Fatal("blocked log runner did not observe parent cancellation")
	}
}

func TestExecuteHostProcessPodReportsSafeLogRetryExhaustion(t *testing.T) {
	t.Parallel()

	fixture := newHostProcessOperationsFixture()
	fixture.terminationDetails = func(context.Context) (string, string) {
		return "0 secret-session-id", "Completed private-host.internal"
	}
	fixture.retrieve = func(context.Context, string) (string, error) {
		return "retrieved-log-content-must-not-leak", errors.New(
			"password=top-secret token=private-session node=private-host.internal unexpected EOF")
	}

	_, err := executeHostProcessPod(context.Background(), "hostprocess-pod", true,
		fixture.operations(), testHostProcessLogRetryConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phase=Succeeded")
	assert.Contains(t, err.Error(), "containerExitCode=unknown")
	assert.Contains(t, err.Error(), "containerReason=unknown")
	assert.Contains(t, err.Error(), "attempts=5")
	assert.Contains(t, err.Error(), "lastErrorCategory=unexpected_eof")
	assert.NotContains(t, err.Error(), "top-secret")
	assert.NotContains(t, err.Error(), "private-session")
	assert.NotContains(t, err.Error(), "private-host.internal")
	assert.NotContains(t, err.Error(), "retrieved-log-content-must-not-leak")
	assert.NotContains(t, err.Error(), "\n")
	assert.Equal(t, 5, fixture.logCalls)
	assert.Equal(t, 1, fixture.diagnosticsCalls)
	assert.Equal(t, 1, fixture.cleanupCalls)
}

func TestExecuteHostProcessPodFireAndForgetCleansUpAfterCompletion(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cleaned := make(chan struct{})
	var cleanupCalls atomic.Int32
	operations := hostProcessPodOperations{
		create: func() error { return nil },
		cleanup: func() {
			if cleanupCalls.Add(1) == 1 {
				close(cleaned)
			}
		},
		waitForTerminalPhase: func(ctx context.Context) (string, error) {
			close(started)
			select {
			case <-release:
				return "Succeeded", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())

	_, err := executeHostProcessPod(ctx, "hostprocess-pod", false,
		operations, testHostProcessLogRetryConfig())
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("asynchronous lifecycle watcher did not start")
	}
	select {
	case <-cleaned:
		t.Fatal("pod was cleaned up before the command completed")
	default:
	}

	cancel()
	select {
	case <-cleaned:
		t.Fatal("parent cancellation cleaned up the pod before the command completed")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("pod was not cleaned up after the command completed")
	}
	assert.Equal(t, int32(1), cleanupCalls.Load())
}

func TestExecuteHostProcessPodFireAndForgetEventuallyCleansUp(t *testing.T) {
	cleaned := make(chan struct{})
	operations := hostProcessPodOperations{
		create:  func() error { return nil },
		cleanup: func() { close(cleaned) },
		waitForTerminalPhase: func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
		asyncCleanupTimeout: 25 * time.Millisecond,
	}

	_, err := executeHostProcessPod(context.Background(), "hostprocess-pod", false,
		operations, testHostProcessLogRetryConfig())
	require.NoError(t, err)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("pod was not cleaned up after the asynchronous lifecycle timeout")
	}
}

func TestExecuteHostProcessPodCancellationDuringPhaseWaitCleansUpOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	fixture := newHostProcessOperationsFixture()
	fixture.wait = func(waitCtx context.Context) (string, error) {
		close(started)
		<-waitCtx.Done()
		return "", waitCtx.Err()
	}
	result := make(chan error, 1)
	go func() {
		_, err := executeHostProcessPod(ctx, "hostprocess-pod", true,
			fixture.operations(), testHostProcessLogRetryConfig())
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, fixture.createCalls)
		assert.Equal(t, 1, fixture.waitCalls)
		assert.Equal(t, 0, fixture.logCalls)
		assert.Equal(t, 1, fixture.cleanupCalls)
	case <-time.After(time.Second):
		t.Fatal("phase wait did not observe cancellation")
	}
}

type testCLIBackgrounder struct {
	cmd *exec.Cmd
}

func (b *testCLIBackgrounder) Background() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	b.cmd.Stdout = stdout
	b.cmd.Stderr = stderr
	return b.cmd, stdout, stderr, b.cmd.Start()
}

type fixedCLIBackgrounder struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	err    error
}

func (b *fixedCLIBackgrounder) Background() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
	return b.cmd, b.stdout, b.stderr, b.err
}

func TestRunCLICommandWithContextValidatesCommandResults(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		background cliBackgrounder
	}{
		{name: "nil backgrounder"},
		{name: "nil command", background: &fixedCLIBackgrounder{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}},
		{name: "nil stdout", background: &fixedCLIBackgrounder{cmd: &exec.Cmd{}, stderr: &bytes.Buffer{}}},
		{name: "nil stderr", background: &fixedCLIBackgrounder{cmd: &exec.Cmd{}, stdout: &bytes.Buffer{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCLICommandWithContext(context.Background(), test.background)
			require.ErrorIs(t, err, errInvalidCLICommandResult)
		})
	}
}

func TestRunCLICommandWithContextDoesNotExposeStderr(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestRunCLICommandHelperProcess")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HOSTPROCESS_HELPER_PROCESS=stderr",
		"HOSTPROCESS_HELPER_STDERR=connect: connection refused host=private-node.internal token=top-secret")

	_, err := runCLICommandWithContext(context.Background(), &testCLIBackgrounder{cmd: cmd})
	require.Error(t, err)
	assert.Equal(t, "CLI command failed: category=connection_refused", err.Error())
	assert.NotContains(t, err.Error(), "private-node.internal")
	assert.NotContains(t, err.Error(), "top-secret")
}

func TestRunCLICommandWithContextKillsAndReapsSubprocess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestRunCLICommandHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HOSTPROCESS_HELPER_PROCESS=sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := runCLICommandWithContext(ctx, &testCLIBackgrounder{cmd: cmd})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotNil(t, cmd.ProcessState, "Wait must reap the killed subprocess")
	assert.False(t, cmd.ProcessState.Success())
}

func TestRunCLICommandHelperProcess(t *testing.T) {
	switch os.Getenv("GO_WANT_HOSTPROCESS_HELPER_PROCESS") {
	case "":
		return
	case "stderr":
		_, _ = os.Stderr.WriteString(os.Getenv("HOSTPROCESS_HELPER_STDERR"))
		os.Exit(2)
	case "sleep":
		time.Sleep(time.Hour)
	default:
		os.Exit(3)
	}
}

func TestExecuteHostProcessPodPreservesFailedPhase(t *testing.T) {
	t.Parallel()

	fixture := newHostProcessOperationsFixture()
	fixture.wait = func(context.Context) (string, error) { return "Failed", nil }

	_, err := executeHostProcessPod(context.Background(), "hostprocess-pod", true,
		fixture.operations(), testHostProcessLogRetryConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "phase=Failed")
	assert.Equal(t, 1, fixture.createCalls)
	assert.Equal(t, 1, fixture.cleanupCalls)
	assert.Equal(t, 0, fixture.logCalls)
	assert.Equal(t, 0, fixture.diagnosticsCalls)
}

func TestExecuteHostProcessPodCreatesOnlyOnceDuringLogRetries(t *testing.T) {
	t.Parallel()

	fixture := newHostProcessOperationsFixture()
	fixture.retrieve = func(context.Context, string) (string, error) {
		if fixture.logCalls < 3 {
			return "", errors.New("request timeout")
		}
		return "PowerShell completed", nil
	}

	output, err := executeHostProcessPod(context.Background(), "hostprocess-pod", true,
		fixture.operations(), testHostProcessLogRetryConfig())
	require.NoError(t, err)
	assert.Equal(t, "PowerShell completed", output)
	assert.Equal(t, 1, fixture.createCalls)
	assert.Equal(t, 1, fixture.waitCalls)
	assert.Equal(t, 3, fixture.logCalls)
	assert.Equal(t, 1, fixture.cleanupCalls)
	assert.Equal(t, 0, fixture.diagnosticsCalls)
}

func TestExecuteHostProcessPodReturnsSemanticOutputUnchanged(t *testing.T) {
	t.Parallel()

	expected := "NO_PROXY=api.example.test,10.0.0.0/8"
	fixture := newHostProcessOperationsFixture()
	fixture.retrieve = func(context.Context, string) (string, error) { return expected, nil }

	output, err := executeHostProcessPod(context.Background(), "hostprocess-pod", true,
		fixture.operations(), testHostProcessLogRetryConfig())
	require.NoError(t, err)
	assert.Equal(t, expected, output)
}
