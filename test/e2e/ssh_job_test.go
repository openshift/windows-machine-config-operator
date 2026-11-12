package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers"
)

type sshJobTestProvider struct {
	providers.CloudProvider
}

func (sshJobTestProvider) GetType() config.PlatformType {
	return config.AWSPlatformType
}

func TestWebConfigReadContext(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		stage    string
		deadline bool
	}{
		{name: "cancel job creation", stage: "create"},
		{name: "cancel job request", stage: "get"},
		{name: "cancel polling sleep", stage: "poll"},
		{name: "cancel pod listing", stage: "list"},
		{name: "cancel log stream", stage: "logs"},
		{name: "cancel log body", stage: "log-body"},
		{name: "deadline during job request", stage: "get", deadline: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reached := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				stage := "get"
				switch {
				case request.Method == http.MethodPost:
					stage = "create"
				case strings.HasSuffix(request.URL.Path, "/pods"):
					stage = "list"
				case strings.HasSuffix(request.URL.Path, "/log"):
					stage = "logs"
					if testCase.stage == "log-body" {
						stage = "log-body"
						writer.Header().Set("Content-Type", "text/plain")
						writer.WriteHeader(http.StatusOK)
						writer.(http.Flusher).Flush()
					}
				}
				if stage == testCase.stage {
					close(reached)
					select {
					case <-request.Context().Done():
					case <-release:
					}
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				switch stage {
				case "create":
					fmt.Fprint(writer, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ssh-job"}}`)
				case "get":
					if testCase.stage == "poll" {
						fmt.Fprint(writer, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ssh-job"}}`)
						close(reached)
					} else {
						fmt.Fprint(writer, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ssh-job"},"status":{"conditions":[{"type":"Complete","status":"True"},{"type":"SuccessCriteriaMet","status":"True"}]}}`)
					}
				case "list":
					fmt.Fprint(writer, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"ssh-pod"},"status":{"startTime":"2026-01-01T00:00:00Z"}}]}`)
				}
			}))
			defer server.Close()
			defer close(release)
			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			require.NoError(t, err)
			tc := &testContext{
				client: &clusterinfo.OpenShift{K8s: client}, CloudProvider: sshJobTestProvider{},
				workloadNamespace: "test", artifactDir: t.TempDir(),
			}
			ctx, cancel := context.WithCancel(context.Background())
			if testCase.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			}
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := tc.getWebConfigContentWithContext(ctx, "192.0.2.1")
				result <- err
			}()
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				cancel()
				t.Fatal("file-read path did not reach expected stage")
			}
			if !testCase.deadline {
				if testCase.stage == "poll" {
					time.Sleep(20 * time.Millisecond)
				}
				cancel()
			}
			resultTimeout := time.Second
			if testCase.deadline {
				resultTimeout = 10 * time.Second
			}
			select {
			case err := <-result:
				if testCase.deadline {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(resultTimeout):
				cancel()
				t.Fatal("file-read path did not stop promptly")
			}
		})
	}
}

func TestSSHJobPreCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tc := &testContext{}
	_, err := tc.waitUntilJobSucceedsWithContext(ctx, "ssh-job")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestWebConfigReadSuccess(t *testing.T) {
	content := "tls_server_config:\r\n  min_version: TLS13\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost:
			fmt.Fprint(writer, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ssh-job"}}`)
		case strings.HasSuffix(request.URL.Path, "/pods"):
			fmt.Fprint(writer, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"ssh-pod"},"status":{"startTime":"2026-01-01T00:00:00Z"}}]}`)
		case strings.HasSuffix(request.URL.Path, "/log"):
			writer.Header().Set("Content-Type", "text/plain")
			fmt.Fprintln(writer, base64.StdEncoding.EncodeToString([]byte(content)))
		default:
			fmt.Fprint(writer, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"ssh-job"},"status":{"conditions":[{"type":"Complete","status":"True"},{"type":"SuccessCriteriaMet","status":"True"}]}}`)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	require.NoError(t, err)
	tc := &testContext{
		client: &clusterinfo.OpenShift{K8s: client}, CloudProvider: sshJobTestProvider{},
		workloadNamespace: "test", artifactDir: t.TempDir(),
	}
	for _, contextual := range []bool{false, true} {
		t.Run(fmt.Sprintf("context=%t", contextual), func(t *testing.T) {
			var actual string
			var err error
			if contextual {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				actual, err = tc.getWebConfigContentWithContext(ctx, "192.0.2.1")
			} else {
				actual, err = tc.getWebConfigContent("192.0.2.1")
			}
			require.NoError(t, err)
			assert.Equal(t, content, actual)
		})
	}
}
