package winc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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

func TestTLSRecoveryAfterRestore(t *testing.T) {
	if tlsRestorationTimeout != 22*time.Minute {
		t.Fatalf("expected shared TLS restoration deadline to be 22m, got %v", tlsRestorationTimeout)
	}

	t.Run("waits for rollout before control-plane stability", func(t *testing.T) {
		var calls []string
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(time.Minute), 2,
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

	t.Run("stops after a fatal rollout error", func(t *testing.T) {
		stabilityCalled := false
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(time.Minute), 0,
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
		if err == nil || !strings.Contains(err.Error(), "deployment failed") || stabilityCalled {
			t.Fatalf("expected deployment failure to stop recovery, got stabilityCalled=%t err=%v",
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

	t.Run("stops when restart is not observed", func(t *testing.T) {
		deploymentCalled := false
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(time.Minute), 0,
			tlsRecoveryWaitOperations{
				waitForRestart: func(context.Context, time.Duration) (bool, error) { return false, nil },
				waitForDeployment: func(context.Context, time.Duration) error {
					deploymentCalled = true
					return nil
				},
			})
		if err == nil || !strings.Contains(err.Error(), "did not restart") || deploymentCalled {
			t.Fatalf("expected missing restart to stop recovery, got deploymentCalled=%t err=%v",
				deploymentCalled, err)
		}
	})

	t.Run("shared deadline cancels and reaps a blocked command", func(t *testing.T) {
		var blockedCommand *exec.Cmd
		deploymentCalled := false
		start := time.Now()
		err := waitForTLSRecoveryAfterRestore(context.Background(), time.Now().Add(30*time.Millisecond), 0,
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
			})
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected blocked command to be canceled by the shared deadline, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("blocked command outlived the shared deadline: %v", elapsed)
		}
		if blockedCommand == nil || blockedCommand.ProcessState == nil {
			t.Fatal("expected blocked command to be waited on and reaped")
		}
		if deploymentCalled {
			t.Fatal("expected recovery to stop after the canceled restart check")
		}
	})
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
