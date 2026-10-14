package winc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandOutputWithContext(t *testing.T) {
	startCommand := func(command string, startedCommand **exec.Cmd) cliCommandStarter {
		return func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			cmd := exec.Command("sh", "-c", command)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			if startedCommand != nil {
				*startedCommand = cmd
			}
			return cmd, stdout, stderr, cmd.Start()
		}
	}

	t.Run("successful JSON output excludes stderr warnings", func(t *testing.T) {
		output, err := commandOutputWithContext(context.Background(),
			startCommand(`printf '{"status":"ok"}'; printf 'warning from stderr' >&2`, nil))
		if err != nil {
			t.Fatalf("expected command to succeed, got %v", err)
		}
		var result struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("expected parseable JSON stdout, got %q: %v", output, err)
		}
		if result.Status != "ok" {
			t.Fatalf("expected JSON status ok, got %q", result.Status)
		}
		if strings.Contains(output, "warning from stderr") {
			t.Fatalf("successful output included stderr warning: %q", output)
		}
	})

	t.Run("failed command includes stderr diagnostics", func(t *testing.T) {
		output, err := commandOutputWithContext(context.Background(),
			startCommand(`printf 'partial stdout'; printf 'failure detail' >&2; exit 7`, nil))
		if err == nil {
			t.Fatal("expected command to fail")
		}
		if output != "partial stdout" {
			t.Fatalf("expected only stdout to be returned, got %q", output)
		}
		if !strings.Contains(err.Error(), "stdout: partial stdout") ||
			!strings.Contains(err.Error(), "stderr: failure detail") {
			t.Fatalf("expected stdout and stderr failure diagnostics, got %v", err)
		}
	})

	t.Run("cancellation includes stderr and reaps command", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		var command *exec.Cmd
		output, err := commandOutputWithContext(ctx,
			startCommand(`printf 'cancellation detail' >&2; exec sleep 30`, &command))
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected command cancellation, got output=%q err=%v", output, err)
		}
		if !strings.Contains(err.Error(), "stderr: cancellation detail") {
			t.Fatalf("expected cancellation stderr diagnostics, got %v", err)
		}
		if command == nil || command.ProcessState == nil {
			t.Fatal("expected canceled command to be waited on and reaped")
		}
	})
}

func TestClusterOperatorsSettled(t *testing.T) {
	operatorJSON := `{
  "items": [
    {"metadata":{"name":"authentication"},"status":{"conditions":[
      {"type":"Available","status":"True"},{"type":"Progressing","status":"False"},{"type":"Degraded","status":"False"}]}},
    {"metadata":{"name":"kube-apiserver"},"status":{"conditions":[
      {"type":"Available","status":"True"},{"type":"Progressing","status":"False"},{"type":"Degraded","status":"False"}]}},
    {"metadata":{"name":"openshift-apiserver"},"status":{"conditions":[
      {"type":"Available","status":"True"},{"type":"Progressing","status":"False"},{"type":"Degraded","status":"False"}]}}
  ]
}`

	settled, state, err := clusterOperatorsSettled(operatorJSON, tlsRecoveryClusterOperators)
	if err != nil {
		t.Fatalf("expected valid operator JSON, got %v", err)
	}
	if !settled || state != "" {
		t.Fatalf("expected operators to be settled, got settled=%t state=%q", settled, state)
	}

	progressingJSON := strings.Replace(operatorJSON,
		`{"type":"Progressing","status":"False"}`, `{"type":"Progressing","status":"True"}`, 1)
	settled, state, err = clusterOperatorsSettled(progressingJSON, tlsRecoveryClusterOperators)
	if err != nil {
		t.Fatalf("expected valid operator JSON, got %v", err)
	}
	if settled || !strings.Contains(state, "Progressing=True") {
		t.Fatalf("expected progressing operator to be unsettled, got settled=%t state=%q", settled, state)
	}

	settled, state, err = clusterOperatorsSettled(operatorJSON, append(tlsRecoveryClusterOperators, "missing"))
	if err != nil {
		t.Fatalf("expected valid operator JSON, got %v", err)
	}
	if settled || !strings.Contains(state, "missing was not returned") {
		t.Fatalf("expected missing operator to be unsettled, got settled=%t state=%q", settled, state)
	}

	if _, _, err := clusterOperatorsSettled("{", tlsRecoveryClusterOperators); err == nil {
		t.Fatal("expected malformed operator JSON to fail")
	}
}

func TestPollForTLSRecovery(t *testing.T) {
	const cliRefusal = "The connection to the server api.example:6443 was refused - did you specify the right host or port?"
	t.Run("CLI refusal recovers only after continuous healthy polls", func(t *testing.T) {
		const stablePeriod = 8 * time.Millisecond
		attempts := 0
		var stableSince time.Time
		err := pollForTLSRecovery(2*time.Millisecond, 100*time.Millisecond, stablePeriod,
			func() (bool, string, error) {
				attempts++
				if attempts <= 2 {
					return false, "", fmt.Errorf("querying the OpenShift user API: CLI command failed: stderr: %s: exit status 1", cliRefusal)
				}
				if stableSince.IsZero() {
					stableSince = time.Now()
				}
				return true, "", nil
			})
		if err != nil || attempts < 3 || stableSince.IsZero() || time.Since(stableSince) < stablePeriod {
			t.Fatalf("CLI refusal must recover only after continuous healthy checks: attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("persistent CLI refusal fails at bounded deadline", func(t *testing.T) {
		attempts := 0
		start := time.Now()
		err := pollForTLSRecovery(time.Millisecond, 15*time.Millisecond, time.Millisecond,
			func() (bool, string, error) {
				attempts++
				return false, "", errors.New(cliRefusal)
			})
		if err == nil || attempts < 2 || time.Since(start) > time.Second ||
			!strings.Contains(err.Error(), "did not recover") {
			t.Fatalf("persistent refusal must time out without passing: attempts=%d err=%v", attempts, err)
		}
	})

	for _, failure := range []string{
		"Forbidden: user cannot get apiserver", "Unauthorized: invalid token",
		"parsing cluster operator status: invalid JSON", "TLS certificate verification failure",
		"invalid tlsSecurityProfile configuration", "TLS configuration refused due to invalid cipher",
	} {
		t.Run("permanent "+failure, func(t *testing.T) {
			attempts := 0
			err := pollForTLSRecovery(time.Millisecond, 100*time.Millisecond, time.Millisecond,
				func() (bool, string, error) {
					attempts++
					return false, "", errors.New(failure)
				})
			if attempts != 1 || err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("permanent failure must fail immediately: attempts=%d err=%v", attempts, err)
			}
		})
	}

	t.Run("continuous stability resets after a transient error", func(t *testing.T) {
		const stablePeriod = 5 * time.Millisecond
		attempts := 0
		var stableAfterReset time.Time
		err := pollForTLSRecovery(2*time.Millisecond, 100*time.Millisecond, stablePeriod,
			func() (bool, string, error) {
				attempts++
				switch attempts {
				case 2:
					return false, "", errors.New("service unavailable")
				default:
					if attempts == 3 {
						stableAfterReset = time.Now()
					}
					return true, "", nil
				}
			})
		if err != nil {
			t.Fatalf("expected recovery, got %v", err)
		}
		if stableAfterReset.IsZero() || time.Since(stableAfterReset) < stablePeriod {
			t.Fatalf("recovery completed before a full stable period elapsed after the error")
		}
	})

	t.Run("continuous stability resets after an unsettled result", func(t *testing.T) {
		const stablePeriod = 5 * time.Millisecond
		attempts := 0
		var stableAfterReset time.Time
		err := pollForTLSRecovery(2*time.Millisecond, 100*time.Millisecond, stablePeriod,
			func() (bool, string, error) {
				attempts++
				if attempts == 2 {
					return false, "kube-apiserver Progressing=True", nil
				}
				if attempts == 3 {
					stableAfterReset = time.Now()
				}
				return true, "", nil
			})
		if err != nil {
			t.Fatalf("expected recovery, got %v", err)
		}
		if stableAfterReset.IsZero() || time.Since(stableAfterReset) < stablePeriod {
			t.Fatalf("recovery completed before a full stable period elapsed after an unsettled result")
		}
	})

	t.Run("persistent API outage fails", func(t *testing.T) {
		err := pollForTLSRecovery(time.Millisecond, 10*time.Millisecond, time.Millisecond, func() (bool, string, error) {
			return false, "", errors.New("service unavailable")
		})
		if err == nil || !strings.Contains(err.Error(), "service unavailable") {
			t.Fatalf("expected persistent API error, got %v", err)
		}
	})

	t.Run("unsettled operator fails", func(t *testing.T) {
		err := pollForTLSRecovery(time.Millisecond, 10*time.Millisecond, time.Millisecond, func() (bool, string, error) {
			return false, "openshift-apiserver Degraded=True", nil
		})
		if err == nil || !strings.Contains(err.Error(), "openshift-apiserver Degraded=True") {
			t.Fatalf("expected unsettled operator error, got %v", err)
		}
	})

	t.Run("authorization error fails immediately", func(t *testing.T) {
		attempts := 0
		err := pollForTLSRecovery(time.Millisecond, 100*time.Millisecond, time.Millisecond,
			func() (bool, string, error) {
				attempts++
				return false, "", errors.New("forbidden: user cannot get users/~")
			})
		if err == nil || attempts != 1 || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("expected immediate authorization failure, got attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("malformed operator response fails immediately", func(t *testing.T) {
		attempts := 0
		err := pollForTLSRecovery(time.Millisecond, 100*time.Millisecond, time.Millisecond,
			func() (bool, string, error) {
				attempts++
				ready, state, err := clusterOperatorsSettled("{", tlsRecoveryClusterOperators)
				return ready, state, err
			})
		if err == nil || attempts != 1 || !strings.Contains(err.Error(), "parsing cluster operator status") {
			t.Fatalf("expected immediate schema failure, got attempts=%d err=%v", attempts, err)
		}
	})
}

func TestTLSRecoveryRefusalClassification(t *testing.T) {
	const cliRefusal = "The connection to the server api.example:6443 was refused - did you specify the right host or port?"
	wrapper := fmt.Errorf("querying the OpenShift user API: CLI command failed: stderr: %s: exit status 1", cliRefusal)
	urlRefusal := fmt.Errorf("querying the OpenShift user API: %w", &url.Error{
		Op: "Get", URL: "https://api.example:6443/",
		Err: fmt.Errorf("dial tcp 192.0.2.1:6443: connect: %w", syscall.ECONNREFUSED),
	})
	for _, test := range []struct {
		name      string
		err       error
		transient bool
	}{
		{"exact CLI refusal", errors.New(cliRefusal), true},
		{"wrapped CLI refusal from logs", wrapper, true},
		{"wrapped Go URL dial TCP refused", urlRefusal, true},
		{"unrelated refused", errors.New("TLS configuration refused due to invalid cipher"), false},
		{"incomplete CLI wording", errors.New("The connection to the server api.example:6443 was refused for invalid credentials"), false},
		{"Forbidden", errors.New("Forbidden: user cannot get apiserver"), false},
		{"Unauthorized", errors.New("Unauthorized: invalid token"), false},
		{"schema", errors.New("parsing cluster operator status: invalid JSON"), false},
		{"TLS", errors.New("TLS certificate verification failure"), false},
		{"configuration", errors.New("invalid tlsSecurityProfile configuration"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isTransientTLSRecoveryError(test.err); got != test.transient {
				t.Fatalf("transient=%t, want %t for %v", got, test.transient, test.err)
			}
		})
	}
}

func TestTLSRecoveryAfterRestore(t *testing.T) {
	if tlsRestorationTimeout != 22*time.Minute {
		t.Fatalf("expected shared TLS restoration deadline to be 22m, got %v", tlsRestorationTimeout)
	}

	t.Run("waits for rollout before control-plane stability", func(t *testing.T) {
		var calls []string
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 2,
			tlsRecoveryWaitOperations{
				waitForRestart: func(context.Context, time.Duration) (bool, error) {
					calls = append(calls, "restart")
					return true, nil
				},
				waitForDeployment: func(context.Context, time.Duration) error {
					calls = append(calls, "deployment")
					return nil
				},
				waitForWindowsNodes: func(context.Context, time.Duration) error {
					calls = append(calls, "nodes")
					return nil
				},
				waitForStability: func(context.Context, time.Duration) error {
					calls = append(calls, "stability")
					return nil
				},
			})
		if err != nil {
			t.Fatalf("expected recovery, got %v", err)
		}
		if got, want := strings.Join(calls, ","), "restart,deployment,nodes,stability"; got != want {
			t.Fatalf("unexpected recovery order: got %q, want %q", got, want)
		}
	})

	t.Run("keeps deployment error and checks stability", func(t *testing.T) {
		stabilityCalled := false
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 0,
			tlsRecoveryWaitOperations{
				waitForRestart: func(context.Context, time.Duration) (bool, error) { return true, nil },
				waitForDeployment: func(context.Context, time.Duration) error {
					return errors.New("deployment failed")
				},
				waitForStability: func(context.Context, time.Duration) error {
					stabilityCalled = true
					return nil
				},
			})
		if err == nil || !strings.Contains(err.Error(), "deployment failed") || !stabilityCalled {
			t.Fatalf("expected deployment failure retained with stability check, got stabilityCalled=%t err=%v",
				stabilityCalled, err)
		}
	})

	t.Run("shared deadline caps each phase", func(t *testing.T) {
		deadline := time.Now().Add(50 * time.Millisecond)
		timeout, err := tlsRecoveryPhaseTimeout(deadline, time.Minute)
		if err != nil {
			t.Fatalf("expected remaining shared budget, got %v", err)
		}
		if timeout <= 0 || timeout > 50*time.Millisecond {
			t.Fatalf("expected phase timeout capped by shared deadline, got %v", timeout)
		}

		if _, err := tlsRecoveryPhaseTimeout(time.Now().Add(-time.Millisecond), time.Minute); err == nil {
			t.Fatal("expected expired shared deadline to fail")
		}
	})
	t.Run("expired shared deadline calls no recovery phase", func(t *testing.T) {
		calls := 0
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(-time.Second), 0,
			tlsRecoveryWaitOperations{
				waitForRestart:   func(context.Context, time.Duration) (bool, error) { calls++; return true, nil },
				waitForStability: func(context.Context, time.Duration) error { calls++; return nil },
			})
		if err == nil || calls != 0 {
			t.Fatalf("expired deadline must make no calls: calls=%d err=%v", calls, err)
		}
	})

	t.Run("stops when restart is not observed", func(t *testing.T) {
		deploymentCalled := false
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 0,
			tlsRecoveryWaitOperations{
				waitForRestart: func(context.Context, time.Duration) (bool, error) { return false, nil },
				waitForDeployment: func(context.Context, time.Duration) error {
					deploymentCalled = true
					return nil
				},
				waitForStability: func(context.Context, time.Duration) error { return nil },
			})
		if err == nil || !strings.Contains(err.Error(), "did not restart") || deploymentCalled {
			t.Fatalf("expected missing restart to stop recovery, got deploymentCalled=%t err=%v",
				deploymentCalled, err)
		}
	})

	t.Run("reserved deadline cancels and reaps a blocked command", func(t *testing.T) {
		var blockedCommand *exec.Cmd
		deploymentCalled := false
		start := time.Now()
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(tlsRecoveryAPITimeout+150*time.Millisecond), 0,
			tlsRecoveryWaitOperations{
				waitForRestart: func(ctx context.Context, _ time.Duration) (bool, error) {
					return false, func() error {
						_, err := commandOutputWithContext(ctx, func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
							stdout := &bytes.Buffer{}
							stderr := &bytes.Buffer{}
							blockedCommand = exec.Command("sleep", "30")
							blockedCommand.Stdout = stdout
							blockedCommand.Stderr = stderr
							return blockedCommand, stdout, stderr, blockedCommand.Start()
						})
						return err
					}()
				},
				waitForDeployment: func(context.Context, time.Duration) error {
					deploymentCalled = true
					return nil
				},
				waitForStability: func(context.Context, time.Duration) error { return nil },
			})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected blocked command to be canceled by the shared deadline, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("blocked command outlived the shared deadline: %v", elapsed)
		}
		if blockedCommand == nil || blockedCommand.ProcessState == nil {
			t.Fatal("expected blocked command to be reaped")
		}
		if deploymentCalled {
			t.Fatal("expected recovery to stop after the canceled restart check")
		}
	})
}

func TestWMCOTLSEndpointReadiness(t *testing.T) {
	t.Run("refused then ready refreshes address", func(t *testing.T) {
		addresses := []string{"10.0.0.1", "10.0.0.2"}
		calls := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		address, err := waitForWMCOTLSEndpoint(ctx, time.Millisecond,
			func(context.Context) (string, error) {
				result := addresses[calls]
				calls++
				return result, nil
			}, func(_ context.Context, address string) (string, error) {
				if address == addresses[0] {
					return "BIO_connect:Connection refused; connect:errno=111", nil
				}
				return "CONNECTED(00000003)\nCipher: ECDHE-RSA-AES128-GCM-SHA256", nil
			})
		if err != nil || address != addresses[1] || calls != 2 {
			t.Fatalf("expected refreshed ready endpoint, address=%q calls=%d error=%v", address, calls, err)
		}
	})
	t.Run("API transport EOF during pod lookup retries", func(t *testing.T) {
		calls := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := waitForWMCOTLSEndpoint(ctx, time.Millisecond,
			func(context.Context) (string, error) {
				calls++
				if calls == 1 {
					return "", errors.New("pod GET: unexpected EOF")
				}
				return "10.0.0.2", nil
			}, func(context.Context, string) (string, error) { return "CONNECTED(1)", nil })
		if err != nil || calls != 2 {
			t.Fatalf("EOF from pod lookup should retry: calls=%d err=%v", calls, err)
		}
	})
	for _, message := range []string{"Unauthorized", "malformed JSON"} {
		t.Run(message, func(t *testing.T) {
			calls := 0
			_, err := waitForWMCOTLSEndpoint(context.Background(), time.Millisecond,
				func(context.Context) (string, error) { calls++; return "", errors.New(message) },
				func(context.Context, string) (string, error) { t.Fatal("unexpected probe"); return "", nil })
			if err == nil || calls != 1 {
				t.Fatalf("permanent error must fail immediately: calls=%d error=%v", calls, err)
			}
		})
	}
	t.Run("probe errors never expose openssl output", func(t *testing.T) {
		_, err := waitForWMCOTLSEndpoint(context.Background(), time.Millisecond,
			func(context.Context) (string, error) { return "10.0.0.1", nil },
			func(context.Context, string) (string, error) {
				return "", errors.New("certificate PRIVATE-CONTENT: Unauthorized")
			})
		if err == nil || !strings.Contains(err.Error(), "10.0.0.1") ||
			strings.Contains(err.Error(), "PRIVATE-CONTENT") {
			t.Fatalf("probe error must include endpoint and category but not output: %v", err)
		}
	})
	t.Run("malformed probe result fails without retry", func(t *testing.T) {
		calls := 0
		_, err := waitForWMCOTLSEndpoint(context.Background(), time.Millisecond,
			func(context.Context) (string, error) { return "10.0.0.1", nil },
			func(context.Context, string) (string, error) { calls++; return "certificate PRIVATE-CONTENT", nil })
		if err == nil || calls != 1 || strings.Contains(err.Error(), "PRIVATE-CONTENT") {
			t.Fatalf("malformed probe must fail immediately: calls=%d err=%v", calls, err)
		}
	})
	t.Run("refusal cannot count as negative TLS result", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		calls := 0
		_, err := waitForWMCOTLSEndpoint(ctx, time.Millisecond,
			func(context.Context) (string, error) { return "10.0.0.1", nil },
			func(context.Context, string) (string, error) {
				calls++
				return "connect:errno=111 connection refused", nil
			})
		if err == nil || calls < 2 {
			t.Fatalf("refusal must remain unavailable: calls=%d error=%v", calls, err)
		}
	})
	for _, output := range []string{"CONNECTED(1)\nSSL routines:alert protocol version", "CONNECTED(1)\nCipher: WRONG"} {
		t.Run(output, func(t *testing.T) {
			calls := 0
			_, err := waitForWMCOTLSEndpoint(context.Background(), time.Millisecond,
				func(context.Context) (string, error) { return "10.0.0.1", nil },
				func(context.Context, string) (string, error) { calls++; return output, nil })
			if err != nil || calls != 1 {
				t.Fatalf("completed TLS response must not be retried: calls=%d error=%v", calls, err)
			}
		})
	}
	if isAllowedCustomTLSCipher("CONNECTED(1)\nCipher: WRONG") ||
		!isAllowedCustomTLSCipher("CONNECTED(1)\nCipher: ECDHE-RSA-AES128-GCM-SHA256") {
		t.Fatal("a completed handshake with the wrong cipher must fail immediately")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := waitForWMCOTLSEndpoint(ctx, time.Millisecond,
		func(context.Context) (string, error) { calls++; return "", nil }, nil)
	if err == nil || calls != 0 {
		t.Fatalf("canceled readiness should make no calls: calls=%d error=%v", calls, err)
	}
}

func TestRestoreTLSConfigurationAfterTransientReads(t *testing.T) {
	reads, baselineReads, patches := 0, 0, 0
	baseline, changed, err := restoreTLSConfigurationWithContext(context.Background(), time.Now().Add(tlsRestorationTimeout),
		"StrictAllComponents", `{"type":"Intermediate"}`,
		func(context.Context) (string, string, error) {
			reads++
			if reads == 1 {
				return "", "", errors.New("connection refused")
			}
			return `{"type":"Custom"}`, "StrictAllComponents", nil
		}, func(context.Context) (string, error) {
			baselineReads++
			if baselineReads == 1 {
				return "", errors.New("connection refused")
			}
			return "podUID=original", nil
		}, func(context.Context) error { patches++; return nil })
	if err != nil || !changed || baseline != "podUID=original" || patches != 1 {
		t.Fatalf("expected restoration after transient reads: baseline=%q changed=%t patches=%d err=%v",
			baseline, changed, patches, err)
	}
}

func TestRestoreTLSConfigurationErrors(t *testing.T) {
	if isRetryableTLSRestorationError(errors.Join(errors.New("connection refused"), errors.New("Unauthorized"))) {
		t.Fatal("permanent field error must not be retried despite another transient field error")
	}
	t.Run("permanent read error still attempts known restoration", func(t *testing.T) {
		patches := 0
		_, _, err := restoreTLSConfigurationWithContext(context.Background(), time.Now().Add(tlsRestorationTimeout),
			"StrictAllComponents", `{"type":"Intermediate"}`,
			func(context.Context) (string, string, error) { return "", "", errors.New("Unauthorized") },
			func(context.Context) (string, error) { return "baseline", nil },
			func(context.Context) error { patches++; return nil })
		if err == nil || !strings.Contains(err.Error(), "Unauthorized") || patches != 1 {
			t.Fatalf("read error must be retained after restoration: patches=%d err=%v", patches, err)
		}
	})
	t.Run("baseline error cannot prevent restoration", func(t *testing.T) {
		patches := 0
		baseline, changed, err := restoreTLSConfigurationWithContext(context.Background(), time.Now().Add(tlsRestorationTimeout),
			"StrictAllComponents", `{"type":"Intermediate"}`,
			func(context.Context) (string, string, error) { return `{"type":"Custom"}`, "StrictAllComponents", nil },
			func(context.Context) (string, error) { return "", errors.New("Forbidden") },
			func(context.Context) error { patches++; return nil })
		if err == nil || !strings.Contains(err.Error(), "Forbidden") || !changed || baseline != "" || patches != 1 {
			t.Fatalf("baseline failure must preserve error and restore: baseline=%q changed=%t patches=%d err=%v",
				baseline, changed, patches, err)
		}
	})
	t.Run("transient patch error retries restoration", func(t *testing.T) {
		patches := 0
		_, changed, err := restoreTLSConfigurationWithContext(context.Background(), time.Now().Add(tlsRestorationTimeout),
			"StrictAllComponents", `{"type":"Intermediate"}`,
			func(context.Context) (string, string, error) { return `{"type":"Custom"}`, "StrictAllComponents", nil },
			func(context.Context) (string, error) { return "baseline", nil },
			func(context.Context) error {
				patches++
				if patches == 1 {
					return errors.Join(errors.New("tlsAdherence: connection refused"), errors.New("tlsSecurityProfile: connection refused"))
				}
				return nil
			})
		if err != nil || !changed || patches != 2 {
			t.Fatalf("transient patch should be retried: changed=%t patches=%d err=%v", changed, patches, err)
		}
	})
	t.Run("partial permanent patch failure retains both fields and final gate", func(t *testing.T) {
		fieldErrors := errors.Join(errors.New("tlsAdherence: forbidden"), errors.New("tlsSecurityProfile: invalid"))
		patches := 0
		baseline, attempted, err := restoreTLSConfigurationWithContext(context.Background(),
			time.Now().Add(tlsRestorationTimeout), "StrictAllComponents", `{"type":"Intermediate"}`,
			func(context.Context) (string, string, error) { return "", "", nil },
			func(context.Context) (string, error) { return "baseline", nil },
			func(context.Context) error { patches++; return fieldErrors })
		if err == nil || !attempted || baseline != "baseline" || patches != 1 ||
			!strings.Contains(err.Error(), "tlsAdherence") || !strings.Contains(err.Error(), "tlsSecurityProfile") {
			t.Fatalf("retain both patch errors without claiming success: attempted=%t baseline=%q patches=%d err=%v",
				attempted, baseline, patches, err)
		}
		gateCalls := 0
		recoveryErr := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 0,
			tlsRecoveryWaitOperations{
				waitForRestart:   func(context.Context, time.Duration) (bool, error) { return false, nil },
				waitForStability: func(context.Context, time.Duration) error { gateCalls++; return nil },
			})
		if gateCalls != 1 || recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "did not restart") {
			t.Fatalf("partial restore must retain missing restart and run final gate: calls=%d err=%v", gateCalls, recoveryErr)
		}
	})
	t.Run("expired deadline makes no calls", func(t *testing.T) {
		calls := 0
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, _, err := restoreTLSConfigurationWithContext(ctx, time.Now().Add(-time.Second), "", "",
			func(context.Context) (string, string, error) { calls++; return "", "", nil },
			func(context.Context) (string, error) { calls++; return "", nil },
			func(context.Context) error { calls++; return nil })
		if err == nil || calls != 0 {
			t.Fatalf("expired deadline must make no calls: calls=%d err=%v", calls, err)
		}
	})
	t.Run("expired patch allocation cannot report success", func(t *testing.T) {
		calls := 0
		_, _, err := restoreTLSConfigurationWithContext(context.Background(), time.Now().Add(time.Second), "original", "profile",
			func(context.Context) (string, string, error) { calls++; return "", "", nil },
			func(context.Context) (string, error) { calls++; return "", nil },
			func(context.Context) error { calls++; return nil })
		if err == nil || calls != 0 {
			t.Fatalf("expired patch allocation cannot report success: calls=%d err=%v", calls, err)
		}
	})
	t.Run("restart failure keeps final stability failure", func(t *testing.T) {
		apiCalls := 0
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 0,
			tlsRecoveryWaitOperations{
				waitForRestart: func(context.Context, time.Duration) (bool, error) {
					return false, errors.New("restart query unauthorized")
				},
				waitForStability: func(context.Context, time.Duration) error {
					apiCalls++
					return errors.New("API unavailable")
				},
			})
		if apiCalls != 1 || err == nil || !strings.Contains(err.Error(), "restart query unauthorized") ||
			!strings.Contains(err.Error(), "API unavailable") {
			t.Fatalf("expected both errors and stability call: apiCalls=%d err=%v", apiCalls, err)
		}
	})
	t.Run("Windows node error still checks stability", func(t *testing.T) {
		apiCalls := 0
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(6*time.Minute), 1,
			tlsRecoveryWaitOperations{
				waitForRestart:      func(context.Context, time.Duration) (bool, error) { return true, nil },
				waitForDeployment:   func(context.Context, time.Duration) error { return nil },
				waitForWindowsNodes: func(context.Context, time.Duration) error { return errors.New("node not ready") },
				waitForStability:    func(context.Context, time.Duration) error { apiCalls++; return nil },
			})
		if apiCalls != 1 || err == nil || !strings.Contains(err.Error(), "node not ready") {
			t.Fatalf("node error must survive final stability check: apiCalls=%d err=%v", apiCalls, err)
		}
	})
}

func TestTLSRestorationInflightPhaseDeadlines(t *testing.T) {
	startBlockedCommand := func(ctx context.Context, command **exec.Cmd) error {
		_, err := commandOutputWithContext(ctx, func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			*command = exec.Command("sleep", "30")
			(*command).Stdout = stdout
			(*command).Stderr = stderr
			return *command, stdout, stderr, (*command).Start()
		})
		return err
	}
	for _, test := range []struct {
		name     string
		deadline time.Duration
		block    string
	}{
		{"read phase", tlsRecoveryAPITimeout + tlsRecoveryRestartTimeout +
			tlsRecoveryDeploymentTimeout + tlsRecoveryWindowsNodesTimeout + 300*time.Millisecond, "read"},
		{"baseline phase", tlsRecoveryAPITimeout + tlsRecoveryRestartTimeout +
			tlsRecoveryDeploymentTimeout + tlsRecoveryWindowsNodesTimeout + 300*time.Millisecond, "baseline"},
		{"patch phase", tlsRecoveryAPITimeout + 300*time.Millisecond, "patch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			deadline := time.Now().Add(test.deadline)
			var blockedCommand *exec.Cmd
			patchCalls, stabilityCalls := 0, 0
			start := time.Now()
			_, attempted, err := restoreTLSConfigurationWithContext(context.Background(), deadline,
				"StrictAllComponents", `{"type":"Intermediate"}`,
				func(ctx context.Context) (string, string, error) {
					if test.block == "read" {
						return "", "", startBlockedCommand(ctx, &blockedCommand)
					}
					return `{"type":"Custom"}`, "StrictAllComponents", nil
				}, func(ctx context.Context) (string, error) {
					if test.block == "baseline" {
						return "", startBlockedCommand(ctx, &blockedCommand)
					}
					return "baseline", nil
				},
				func(ctx context.Context) error {
					patchCalls++
					if test.block == "patch" {
						return startBlockedCommand(ctx, &blockedCommand)
					}
					return nil
				})
			if err == nil || !attempted || time.Since(start) > 3*time.Second ||
				blockedCommand == nil || blockedCommand.ProcessState == nil {
				t.Fatalf("blocked %s must be canceled and reaped within phase: attempted=%t err=%v process=%v",
					test.block, attempted, err, blockedCommand)
			}
			if (test.block == "read" || test.block == "baseline") && patchCalls != 1 {
				t.Fatalf("failed in-flight read must still restore known values: patches=%d", patchCalls)
			}
			if test.block == "patch" && patchCalls != 1 {
				t.Fatalf("failed in-flight patch must not be retried after phase deadline: patches=%d", patchCalls)
			}
			recoveryErr := waitForTLSRecoveryAfterRestore(context.Background(), deadline, 0,
				tlsRecoveryWaitOperations{
					waitForRestart: func(context.Context, time.Duration) (bool, error) {
						return false, errors.New("restart not detected")
					},
					waitForStability: func(context.Context, time.Duration) error {
						stabilityCalls++
						return errors.New("API not stable")
					},
				})
			restartFailure := "restart not detected"
			if test.block == "patch" {
				restartFailure = "no time for restart recovery"
			}
			if stabilityCalls != 1 || recoveryErr == nil ||
				!strings.Contains(recoveryErr.Error(), restartFailure) ||
				!strings.Contains(recoveryErr.Error(), "API not stable") {
				t.Fatalf("final gate must run and preserve failures: calls=%d err=%v", stabilityCalls, recoveryErr)
			}
		})
	}
}

func TestReadyWMCOPodIP(t *testing.T) {
	input := `{"items":[
		{"metadata":{"name":"old","creationTimestamp":"2026-01-01T00:00:00Z"},"status":{"phase":"Running","podIP":"10.0.0.1","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"manager","ready":true}]}},
		{"metadata":{"name":"new","creationTimestamp":"2026-01-02T00:00:00Z"},"status":{"phase":"Running","podIP":"10.0.0.2","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"manager","ready":true}]}},
		{"metadata":{"name":"terminating","deletionTimestamp":"2026-01-04T00:00:00Z","creationTimestamp":"2026-01-03T00:00:00Z"},"status":{"phase":"Running","podIP":"10.0.0.3","conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"manager","ready":true}]}}]}`
	address, err := readyWMCOPodIP(input)
	if err != nil || address != "10.0.0.2" {
		t.Fatalf("newest ready nonterminating address: %q %v", address, err)
	}
	olderAddress, err := readyWMCOPodIP(strings.Replace(input, `"podIP":"10.0.0.2"`, `"podIP":"invalid"`, 1))
	if err != nil || olderAddress != "10.0.0.1" {
		t.Fatalf("older ready pod should remain reachable: %q %v", olderAddress, err)
	}
}

func TestReadyWMCOPodName(t *testing.T) {
	podJSON := `{
  "items": [
    {"metadata":{"name":"old-ready","creationTimestamp":"2026-09-29T00:00:00Z"},"status":{"phase":"Running",
      "conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"manager","ready":true}]}},
    {"metadata":{"name":"new-ready","creationTimestamp":"2026-09-29T00:01:00Z"},"status":{"phase":"Running",
      "conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"manager","ready":true}]}},
    {"metadata":{"name":"terminating","creationTimestamp":"2026-09-29T00:02:00Z","deletionTimestamp":"2026-09-29T00:03:00Z"},
      "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],
      "containerStatuses":[{"name":"manager","ready":true}]}},
    {"metadata":{"name":"not-ready","creationTimestamp":"2026-09-29T00:04:00Z"},"status":{"phase":"Running",
      "conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"manager","ready":false}]}}
  ]
}`

	podName, err := readyWMCOPodName(podJSON)
	if err != nil {
		t.Fatalf("expected ready pod, got %v", err)
	}
	if podName != "new-ready" {
		t.Fatalf("expected newest ready pod, got %q", podName)
	}

	if _, err := readyWMCOPodName(`{"items":[]}`); !errors.Is(err, errNoReadyWMCOPod) {
		t.Fatalf("expected no-ready-pod error, got %v", err)
	}

	for _, malformed := range []string{`{`, `{"items":{}}`, `{}`} {
		if _, err := readyWMCOPodName(malformed); err == nil || errors.Is(err, errNoReadyWMCOPod) {
			t.Fatalf("expected malformed pod list %q to fail permanently, got %v", malformed, err)
		}
	}
}

func TestPollForWMCOManagerTLSLogs(t *testing.T) {
	t.Run("matches only the expected missing pod log resource", func(t *testing.T) {
		const podName = "windows-machine-config-operator-7477bf7f4-g2f9x"
		tests := []struct {
			name      string
			message   string
			transient bool
		}{
			{
				name: "verbatim unquoted Prow error",
				message: "Error from server (NotFound): the server could not find the requested resource " +
					"( pods/log windows-machine-config-operator-7477bf7f4-g2f9x)",
				transient: true,
			},
			{
				name:      "quoted pod log resource",
				message:   `Error from server (NotFound): pods/log "windows-machine-config-operator-7477bf7f4-g2f9x" not found`,
				transient: true,
			},
			{
				name: "different pod",
				message: "Error from server (NotFound): the server could not find the requested resource " +
					"( pods/log windows-machine-config-operator-7477bf7f4-other)",
			},
			{
				name:    "permanent namespace error",
				message: `Error from server (NotFound): namespaces "openshift-windows-machine-config-operator" not found`,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if got := isExpectedPodLogNotFound(errors.New(test.message), podName); got != test.transient {
					t.Fatalf("expected transient=%t, got %t for %q", test.transient, got, test.message)
				}
			})
		}
	})

	t.Run("retries transient and incomplete logs", func(t *testing.T) {
		attempts := 0
		logs, err := pollForWMCOManagerTLSLogs(time.Millisecond, 100*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				attempts++
				switch attempts {
				case 1:
					return "", fmt.Errorf("%w: pods \"wmco-old\" not found", errWMCOPodDisappeared)
				case 2:
					return "TLS configuration loaded with VersionTLS12", nil
				default:
					return "TLS configuration loaded with VersionTLS13", nil
				}
			})
		if err != nil {
			t.Fatalf("expected log retrieval to recover, got %v", err)
		}
		if attempts != 3 || !strings.Contains(logs, "VersionTLS13") {
			t.Fatalf("expected VersionTLS13 on third attempt, got attempts=%d logs=%q", attempts, logs)
		}
	})

	t.Run("malformed pod response fails immediately", func(t *testing.T) {
		attempts := 0
		_, err := pollForWMCOManagerTLSLogs(time.Millisecond, 100*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				attempts++
				_, err := readyWMCOPodName("{")
				return "", err
			})
		if err == nil || attempts != 1 || !strings.Contains(err.Error(), "parsing WMCO pod list") {
			t.Fatalf("expected immediate malformed pod-list failure, got attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("permanent not found error fails immediately", func(t *testing.T) {
		attempts := 0
		_, err := pollForWMCOManagerTLSLogs(time.Millisecond, 100*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				attempts++
				return "", errors.New(`namespaces "openshift-windows-machine-config-operator" not found`)
			})
		if err == nil || attempts != 1 || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("expected immediate permanent not-found failure, got attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("persistent unavailable error fails", func(t *testing.T) {
		_, err := pollForWMCOManagerTLSLogs(time.Millisecond, 10*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				return "", errors.New("the server is currently unable to handle the request")
			})
		if err == nil || !strings.Contains(err.Error(), "server is currently unable") {
			t.Fatalf("expected final unavailable error, got %v", err)
		}
	})

	t.Run("missing expected profile fails", func(t *testing.T) {
		_, err := pollForWMCOManagerTLSLogs(time.Millisecond, 10*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				return "TLS configuration loaded with VersionTLS12", nil
			})
		if err == nil || !strings.Contains(err.Error(), "VersionTLS13") {
			t.Fatalf("expected missing TLS profile error, got %v", err)
		}
	})

	t.Run("non-transient error fails immediately", func(t *testing.T) {
		attempts := 0
		_, err := pollForWMCOManagerTLSLogs(time.Millisecond, 100*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				attempts++
				return "", errors.New("forbidden")
			})
		if err == nil || attempts != 1 || !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("expected immediate forbidden error, got attempts=%d err=%v", attempts, err)
		}
	})
}
