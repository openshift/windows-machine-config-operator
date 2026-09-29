package winc

import (
	"errors"
	"fmt"
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
	t.Run("waits for rollout before control-plane stability", func(t *testing.T) {
		var calls []string
		err := waitForTLSRecoveryAfterRestore(time.Now().Add(time.Minute), 2, tlsRecoveryWaitOperations{
			waitForRestart: func(timeout time.Duration) (bool, error) {
				calls = append(calls, "restart")
				return true, nil
			},
			waitForDeployment: func(timeout time.Duration) error {
				calls = append(calls, "deployment")
				return nil
			},
			waitForWindowsNodes: func(timeout time.Duration) error {
				calls = append(calls, "nodes")
				return nil
			},
			waitForStability: func(timeout time.Duration) error {
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
		err := waitForTLSRecoveryAfterRestore(time.Now().Add(time.Minute), 0, tlsRecoveryWaitOperations{
			waitForRestart:    func(timeout time.Duration) (bool, error) { return true, nil },
			waitForDeployment: func(timeout time.Duration) error { return errors.New("deployment failed") },
			waitForStability: func(timeout time.Duration) error {
				stabilityCalled = true
				return nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "deployment failed") || stabilityCalled {
			t.Fatalf("expected deployment failure to stop recovery, got stabilityCalled=%t err=%v",
				stabilityCalled, err)
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
}

func TestPollForWMCOManagerTLSLogs(t *testing.T) {
	t.Run("retries transient and incomplete logs", func(t *testing.T) {
		attempts := 0
		logs, err := pollForWMCOManagerTLSLogs(time.Millisecond, 100*time.Millisecond, "VersionTLS13",
			func() (string, error) {
				attempts++
				switch attempts {
				case 1:
					return "", fmt.Errorf("pods \"wmco-old\" not found")
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
