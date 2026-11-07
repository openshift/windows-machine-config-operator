package winc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var (
	errNoReadyWMCOPod     = errors.New("no ready WMCO manager pod found")
	errWMCOPodDisappeared = errors.New("ready WMCO pod disappeared before its logs could be read")
)

type cliCommandStarter func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error)

func cliOutputWithContext(ctx context.Context, command *exutil.CLI) (string, error) {
	return commandOutputWithContext(ctx, command.Background)
}

func joinTLSRestorationErrors(errs ...error) error {
	return errors.Join(errs...)
}

// commandOutputWithContext runs an OTE CLI command until it completes or the
// supplied context expires. On cancellation it kills the in-flight process and
// waits for it to be reaped before returning.
func commandOutputWithContext(ctx context.Context, start cliCommandStarter) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("CLI command canceled before start: %w", err)
	}

	cmd, stdout, stderr, err := start()
	if err != nil {
		return "", fmt.Errorf("starting CLI command: %w", err)
	}
	waitResult := make(chan error, 1)
	go func() {
		waitResult <- cmd.Wait()
	}()

	stdoutOutput := func() string {
		return strings.TrimSpace(stdout.String())
	}
	commandError := func(message string, err error) error {
		stdoutText := stdoutOutput()
		stderrText := strings.TrimSpace(stderr.String())
		switch {
		case stdoutText != "" && stderrText != "":
			return fmt.Errorf("%s: stdout: %s; stderr: %s: %w", message, stdoutText, stderrText, err)
		case stderrText != "":
			return fmt.Errorf("%s: stderr: %s: %w", message, stderrText, err)
		case stdoutText != "":
			return fmt.Errorf("%s: stdout: %s: %w", message, stdoutText, err)
		default:
			return fmt.Errorf("%s: %w", message, err)
		}
	}
	select {
	case err := <-waitResult:
		output := stdoutOutput()
		if err == nil {
			return output, nil
		}
		return output, commandError("CLI command failed", err)
	case <-ctx.Done():
		killErr := cmd.Process.Kill()
		<-waitResult
		output := stdoutOutput()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return output, commandError(fmt.Sprintf("killing CLI command after cancellation: %v", killErr), ctx.Err())
		}
		return output, commandError("CLI command canceled", ctx.Err())
	}
}

func pollForTLSRecoveryWithContext(ctx context.Context, interval, timeout,
	minimumStablePeriod time.Duration, check tlsRecoveryContextCheck) error {
	var lastErr error
	var lastState string
	var stableSince time.Time
	pollErr := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		ready, state, err := check(ctx)
		if err != nil {
			if !isTransientTLSRecoveryError(err) {
				return false, err
			}
			lastErr = err
			lastState = ""
			stableSince = time.Time{}
			return false, nil
		}
		lastErr = nil
		lastState = state
		if !ready {
			stableSince = time.Time{}
			return false, nil
		}
		if stableSince.IsZero() {
			stableSince = time.Now()
		}
		return time.Since(stableSince) >= minimumStablePeriod, nil
	})
	if pollErr == nil {
		return nil
	}
	if !errors.Is(pollErr, context.DeadlineExceeded) && !errors.Is(pollErr, context.Canceled) {
		return fmt.Errorf("checking OpenShift API recovery: %w", pollErr)
	}
	if lastErr != nil {
		return fmt.Errorf("OpenShift API did not recover within %v: %w", timeout, lastErr)
	}
	if lastState != "" {
		return fmt.Errorf("OpenShift control plane did not settle within %v: %s", timeout, lastState)
	}
	return fmt.Errorf("OpenShift API and control plane did not recover within %v", timeout)
}

func isTransientWMCOLogError(err error) bool {
	if errors.Is(err, errNoReadyWMCOPod) || errors.Is(err, errWMCOPodDisappeared) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, transientMessage := range []string{
		"server is currently unable to handle the request",
		"service unavailable",
		"connection refused",
		"connection reset by peer",
		"tls handshake timeout",
	} {
		if strings.Contains(message, transientMessage) {
			return true
		}
	}
	return false
}

// waitForWMCOTLSEndpoint waits for a transport connection, not a particular
// TLS version. A completed handshake rejection is still a reachable endpoint.
func waitForWMCOTLSEndpoint(ctx context.Context, interval time.Duration,
	endpoint func(context.Context) (string, error), probe func(context.Context, string) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("waiting for WMCO TLS endpoint: %w", err)
	}
	var address string
	var lastErr error
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var fetchErr error
		address, fetchErr = endpoint(ctx)
		if fetchErr != nil {
			if !isTransientWMCOTLSTransportError(fetchErr) {
				return false, fmt.Errorf("WMCO TLS pod lookup: %s", tlsReadinessErrorCategory(fetchErr))
			}
			lastErr = fmt.Errorf("pod lookup transport unavailable")
			return false, nil
		}
		if address == "" {
			lastErr = errNoReadyWMCOPod
			return false, nil
		}
		output, probeErr := probe(ctx, address)
		if probeErr != nil {
			if !isTransientWMCOTLSTransportError(probeErr) {
				return false, fmt.Errorf("WMCO TLS endpoint %s probe: %s", address, tlsReadinessErrorCategory(probeErr))
			}
			lastErr = fmt.Errorf("endpoint %s probe transport unavailable", address)
			return false, nil
		}
		if isTLSTransportFailure(output) {
			lastErr = fmt.Errorf("WMCO TLS endpoint %s transport unavailable", address)
			return false, nil
		}
		if !strings.Contains(output, "CONNECTED(") && !strings.Contains(output, "CONNECTED\n") {
			return false, fmt.Errorf("WMCO TLS endpoint %s probe did not reach a TLS handshake", address)
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("waiting for WMCO TLS endpoint: %w (last transport error: %v)", err, lastErr)
	}
	return address, nil
}

func tlsReadinessErrorCategory(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled or timed out"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unauthorized") || strings.Contains(message, "forbidden") {
		return "authentication or authorization failure"
	}
	if strings.Contains(message, "json") || strings.Contains(message, "schema") {
		return "invalid pod response"
	}
	return "permanent transport or configuration failure"
}

func isTLSTransportFailure(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "connection refused") || strings.Contains(lower, "connect:errno=") ||
		strings.Contains(lower, "connection reset by peer")
}

func isTransientWMCOTLSTransportError(err error) bool {
	return isTransientTLSRecoveryError(err) || strings.Contains(strings.ToLower(err.Error()), "unexpected eof")
}

func isAllowedCustomTLSCipher(output string) bool {
	for _, cipher := range []string{
		"ECDHE-RSA-AES128-GCM-SHA256", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"ECDHE-ECDSA-AES128-GCM-SHA256", "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
	} {
		if strings.Contains(output, cipher) {
			return true
		}
	}
	return false
}

func pollForWMCOManagerTLSLogs(interval, timeout time.Duration, expectedTLSVersion string,
	fetch wmcoLogFetch) (string, error) {
	var logs string
	var lastErr error
	missingExpectedLog := false
	pollErr := wait.Poll(interval, timeout, func() (bool, error) {
		var err error
		logs, err = fetch()
		if err != nil {
			if !isTransientWMCOLogError(err) {
				return false, err
			}
			lastErr = err
			missingExpectedLog = false
			return false, nil
		}
		lastErr = nil
		missingExpectedLog = !strings.Contains(logs, "TLS configuration loaded") ||
			!strings.Contains(logs, expectedTLSVersion)
		return !missingExpectedLog, nil
	})
	if pollErr == nil {
		return logs, nil
	}
	if !errors.Is(pollErr, wait.ErrWaitTimeout) {
		return "", fmt.Errorf("fetching current WMCO manager TLS logs: %w", pollErr)
	}
	if lastErr != nil {
		return "", fmt.Errorf("timed out after %v waiting for current WMCO manager logs containing %s: %w",
			timeout, expectedTLSVersion, lastErr)
	}
	if missingExpectedLog {
		return "", fmt.Errorf("timed out after %v waiting for current WMCO manager logs containing both %q and %q",
			timeout, "TLS configuration loaded", expectedTLSVersion)
	}
	return "", fmt.Errorf("timed out after %v waiting for current WMCO manager TLS logs", timeout)
}

func checkWMCORestartedWithinContext(ctx context.Context, oc *exutil.CLI, initialRestartState string,
	timeout time.Duration) (bool, error) {
	if initialRestartState == "" {
		return false, fmt.Errorf("empty restart baseline: must capture WMCO restart state before triggering restart")
	}
	var restartDetected bool
	pollErr := wait.PollUntilContextCancel(ctx, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		actualRestartState, err := getWMCORestartStateWithContext(ctx, oc)
		if err != nil {
			if !isTransientTLSRecoveryError(err) {
				return false, err
			}
			e2e.Logf("WMCO restart state unavailable: %v", err)
			return false, nil
		}
		if actualRestartState == "" {
			e2e.Logf("WMCO restart state unavailable (pod transitioning), waiting...")
			return false, nil
		}
		if initialRestartState != actualRestartState {
			e2e.Logf("WMCO restarted (old: %s, new: %s)", initialRestartState, actualRestartState)
			restartDetected = true
			return true, nil
		}
		e2e.Logf("WMCO did not restart yet, waiting...")
		return false, nil
	})
	if pollErr != nil {
		if errors.Is(pollErr, context.DeadlineExceeded) || errors.Is(pollErr, context.Canceled) {
			e2e.Logf("WMCO did not restart within %v; continuing after the restart wait timed out", timeout)
			return false, nil
		}
		return false, fmt.Errorf("error checking WMCO restart: %w", pollErr)
	}
	return restartDetected, nil
}
