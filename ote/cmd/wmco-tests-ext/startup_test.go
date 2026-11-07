package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestStartupResultBoundary(t *testing.T) {
	for _, compatibilityFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("compatibilityFailure=%t", compatibilityFailure), func(t *testing.T) {
			compatibilityCalls, preparationCalls, bodyCalls := 0, 0, 0
			body := func(context.Context) *et.ExtensionTestResult {
				bodyCalls++
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			}
			specs := et.ExtensionTestSpecs{}
			for index := 0; index < 5; index++ {
				specs = append(specs, &et.ExtensionTestSpec{Name: fmt.Sprintf("[BYOHPool] case %d", index), Run: body, RunParallel: body})
			}
			configureStartup(specs, func() error {
				compatibilityCalls++
				if compatibilityFailure {
					return errors.New("untrusted-compatibility-detail")
				}
				return nil
			}, func() error {
				preparationCalls++
				return errors.New("retained preparation failure")
			})
			var protocol bytes.Buffer
			writer, err := et.NewJSONResultWriter(&protocol, et.JSON)
			if err != nil {
				t.Fatal(err)
			}
			results, runErr := specs.Run(context.Background(), writer, 1)
			if runErr == nil || len(results) != 5 || bodyCalls != 0 || compatibilityCalls != 1 || preparationCalls != map[bool]int{true: 0, false: 1}[compatibilityFailure] {
				t.Fatalf("startup failed to block all bodies: results=%d err=%v compat=%d prep=%d bodies=%d", len(results), runErr, compatibilityCalls, preparationCalls, bodyCalls)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(protocol.String(), "untrusted-compatibility-detail") {
				t.Fatal("compatibility error exposed untrusted details")
			}
			for _, result := range results {
				if result.Result != et.ResultFailed || result.StartTime == nil || result.EndTime == nil {
					t.Fatalf("missing structured failed result: %+v", result)
				}
			}
		})
	}
}

func TestStartupSuccessSequencing(t *testing.T) {
	var order []string
	body := func(context.Context) *et.ExtensionTestResult {
		order = append(order, "body")
		return &et.ExtensionTestResult{Result: et.ResultPassed}
	}
	ordinary := &et.ExtensionTestSpec{Name: "ordinary", Run: body}
	byoh := &et.ExtensionTestSpec{Name: "[BYOHPool]", Run: body}
	configureStartup(et.ExtensionTestSpecs{ordinary, byoh}, func() error {
		order = append(order, "compatibility")
		return nil
	}, func() error {
		order = append(order, "preparation")
		return nil
	})
	for _, spec := range []*et.ExtensionTestSpec{ordinary, byoh, byoh} {
		if _, err := (et.ExtensionTestSpecs{spec}).Run(context.Background(), et.NullResultWriter{}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(order, ",") != "compatibility,body,preparation,body,body" {
		t.Fatalf("startup sequencing changed: %v", order)
	}
}

func TestBYOHStartupProtocol(t *testing.T) {
	if os.Getenv("WMCO_STARTUP_PROTOCOL_CHILD") == "true" {
		if os.Getenv("WMCO_STARTUP_BAD_CONFIG") == "true" {
			if err := os.Setenv("KUBECONFIG", os.Getenv("KUBECONFIG")+".missing"); err != nil {
				t.Fatal(err)
			}
		}
		os.Args = []string{os.Args[0], "run-test", os.Args[len(os.Args)-1]}
		main()
		t.Fatal("failed startup unexpectedly returned success")
	}
	specs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		t.Fatal(err)
	}
	selected := specs.Select(func(spec *et.ExtensionTestSpec) bool { return strings.Contains(spec.Name, "[BYOHPool]") })
	if len(selected) != 5 {
		t.Fatalf("expected exactly five actual BYOH specs, got %d", len(selected))
	}
	var mutex sync.Mutex
	var session *corev1.ConfigMap
	creates, preflights, failureWrites := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/namespaces/kube-system":
			_ = json.NewEncoder(response).Encode(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}})
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/windows-node-pool"):
			if session != nil && strings.Contains(session.Annotations["windowsmachineconfig.openshift.io/byoh-session"], `"Phase":"Initializing"`) {
				preflights++
			}
			_ = json.NewEncoder(response).Encode(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "windows-node-pool", UID: "pool", ResourceVersion: "1"}, Data: map[string]string{"192.0.2.1": "malformed"}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces/openshift-windows-machine-config-operator/configmaps":
			if session != nil {
				response.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(response).Encode(&metav1.Status{Status: "Failure", Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			created := &corev1.ConfigMap{}
			body, readErr := io.ReadAll(request.Body)
			if err := errors.Join(readErr, runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), body, created)); err != nil {
				t.Error(err)
				response.WriteHeader(400)
				return
			}
			created.UID, created.ResourceVersion = "session", "1"
			session = created
			creates++
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(session)
		case session != nil && request.URL.Path == "/api/v1/namespaces/openshift-windows-machine-config-operator/configmaps/"+session.Name && request.Method == http.MethodGet:
			_ = json.NewEncoder(response).Encode(session)
		case session != nil && request.URL.Path == "/api/v1/namespaces/openshift-windows-machine-config-operator/configmaps/"+session.Name && request.Method == http.MethodPut:
			updated := &corev1.ConfigMap{}
			body, readErr := io.ReadAll(request.Body)
			if err := errors.Join(readErr, runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), body, updated)); err != nil {
				t.Error(err)
				response.WriteHeader(400)
				return
			}
			session = updated
			failureWrites++
			_ = json.NewEncoder(response).Encode(session)
		default:
			t.Errorf("unexpected request: body or unsafe preparation ran: %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	config := filepath.Join(t.TempDir(), "config")
	content := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: local\n  cluster:\n    server: %s\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: local\ncurrent-context: local\nusers:\n- name: local\n  user: {}\n", server.URL)
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var originalCause string
	attempts := append(et.ExtensionTestSpecs{selected[0]}, selected...)
	attempts = append(attempts, selected[0])
	for index, spec := range attempts {
		if index == 6 {
			mutex.Lock()
			var retained map[string]interface{}
			if err := json.Unmarshal([]byte(session.Annotations["windowsmachineconfig.openshift.io/byoh-session"]), &retained); err != nil {
				t.Fatal(err)
			}
			retained["Failure"] = map[string]interface{}{"Phase": "validation", "Host": 999, "Stage": "JSON-decode\nprivate-sentinel", "Fact": strings.Repeat("private-sentinel", 2048), "Field": "private-sentinel", "Adoption": "private-sentinel", "arbitrary": "private-sentinel"}
			encoded, err := json.Marshal(retained)
			if err != nil {
				t.Fatal(err)
			}
			session.Annotations["windowsmachineconfig.openshift.io/byoh-session"] = string(encoded)
			mutex.Unlock()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBYOHStartupProtocol$", "--", spec.Name)
		command.Env = append(os.Environ(), "WMCO_STARTUP_PROTOCOL_CHILD=true", "WMCO_BYOH_NODE_POOL_REQUIRED=true", "KUBECONFIG="+config, `JOB_SPEC={"prowjobid":"18d74027-cdc4-4347-8b32-81dd83a83001"}`)
		if index == 0 {
			command.Env = append(command.Env, "WMCO_STARTUP_BAD_CONFIG=true")
		}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("expected CLI exit 1: %v stderr=%s", err, stderr.String())
		}
		var results []*et.ExtensionTestResult
		if err := json.Unmarshal(stdout.Bytes(), &results); err != nil || len(results) != 1 {
			t.Fatalf("actual run-test failed to serialize result: %v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
		}
		expected := "restart prohibited by retained session"
		if index == 1 {
			expected = "stage=inventory fact=inventory"
		} else if index == 0 {
			expected = "BYOH compatibility initialization failed"
		}
		result := results[0]
		if result.Name != spec.Name || result.Result != et.ResultFailed || result.Lifecycle != et.LifecycleBlocking || result.StartTime == nil || result.EndTime == nil || !strings.Contains(result.Error, expected) {
			t.Fatalf("incorrect actual CLI result: %+v", result)
		}
		if index == 1 {
			originalCause = strings.TrimPrefix(result.Error, "BYOH preflight/preparation failed: ")
			mutex.Lock()
			var stored struct {
				Failure struct{ Stage, Fact, SessionCreate, SessionUpdate string }
			}
			err := json.Unmarshal([]byte(session.Annotations["windowsmachineconfig.openshift.io/byoh-session"]), &stored)
			mutex.Unlock()
			if err != nil || stored.Failure.Stage != "inventory" || stored.Failure.Fact != "inventory" || stored.Failure.SessionCreate != "confirmed" || stored.Failure.SessionUpdate != "attempted" {
				t.Fatal("first Failed record did not retain original safe cause/checkpoint")
			}
		}
		if index > 1 && index < 6 && result.Error != "preparation previously failed: "+originalCause+"; restart prohibited by retained session" {
			t.Fatalf("later actual process lost original safe cause: %s", result.Error)
		}
		if strings.Contains(result.Error+stderr.String()+stdout.String(), "private-sentinel") || len(result.Error) > 512 || strings.ContainsAny(result.Error, "\r\n\x00") {
			t.Fatal("untrusted persisted cause leaked through actual process protocol")
		}
		t.Logf("actual run-test exit=1 result=failed case=%s error=%s", spec.Name, result.Error)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if creates != 1 || preflights != 1 || failureWrites != 1 || session == nil || !strings.Contains(session.Annotations["windowsmachineconfig.openshift.io/byoh-session"], `"Phase":"Failed"`) {
		t.Fatalf("Failed session restarted or lost: creates=%d preflights=%d writes=%d", creates, preflights, failureWrites)
	}
	t.Log("five actual specs plus initialization and malicious-cause attempts: one preparation, retained Failed session, no body requests, seven structured failures")
}
