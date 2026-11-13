package e2e

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	prometheusRule      = "windows-prometheus-k8s-rules"
	monitoringNamespace = "openshift-monitoring"
	windowsRuleName     = "windows.rules"
)

func (tc *testContext) testMetrics(t *testing.T) {
	t.Run("Windows Exporter configuration validation", tc.testWindowsExporter)
	t.Run("Prometheus configuration validation", tc.testPrometheus)
	t.Run("Windows Prometheus rules validation", tc.testWindowsPrometheusRules)
	t.Run("Windows Node resource usage info validation", tc.testNodeResourceUsage)
	t.Run("WebConfig TLS adherence", tc.testWebConfigTLSAdherence)
}

// testWindowsExporter deploys Linux pod and tests that it can communicate with Windows node's metrics port
func (tc *testContext) testWindowsExporter(t *testing.T) {
	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")

	for _, winNode := range gc.allNodes() {
		t.Run(winNode.Name, func(t *testing.T) {
			// Get the node internal IP so we can curl it
			winNodeInternalIP := ""
			for _, address := range winNode.Status.Addresses {
				if address.Type == core.NodeInternalIP {
					winNodeInternalIP = address.Address
				}
			}
			require.Greaterf(t, len(winNodeInternalIP), 0, "test requires Windows node %s to have internal IP",
				winNode.Name)

			linuxCurlerJob, err := tc.createLinuxCurlerJob(strings.ToLower(winNode.Status.NodeInfo.MachineID),
				fmt.Sprintf("http://%s:%d/%s", winNodeInternalIP, int(metrics.Port), metrics.PortName), false)
			require.NoError(t, err, "could not create Linux job")
			// delete the job created
			defer tc.deleteJob(linuxCurlerJob.Name)

			_, err = tc.waitUntilJobSucceeds(linuxCurlerJob.Name)
			assert.NoError(t, err, "could not curl the Windows VM metrics endpoint from a linux container")
		})
	}
}

// testPrometheus tests if Prometheus is configured to scrape metrics endpoints
func (tc *testContext) testPrometheus(t *testing.T) {
	// check that service exists
	_, err := tc.client.K8s.CoreV1().Services(wmcoNamespace).Get(context.TODO(),
		metrics.WindowsMetricsResource, metav1.GetOptions{})
	require.NoError(t, err)

	// check that SM existS
	_, err = tc.client.Monitoring.ServiceMonitors(wmcoNamespace).Get(context.TODO(),
		metrics.WindowsMetricsResource, metav1.GetOptions{})
	require.NoError(t, err, "error getting service monitor")

}

// testPrometheusEndpointSliceCleanup verifies that no Windows nodes remain in kubelet EndpointSlices
func (tc *testContext) testPrometheusEndpointSliceCleanup(t *testing.T) {
	// List all EndpointSlices for the kubelet service in kube-system namespace
	endpointSlices, err := tc.client.K8s.DiscoveryV1().EndpointSlices("kube-system").List(
		context.TODO(),
		metav1.ListOptions{
			LabelSelector: "kubernetes.io/service-name=kubelet",
		},
	)
	require.NoError(t, err, "error listing EndpointSlices for kubelet service")

	// Verify that no Windows node addresses appear in any EndpointSlice
	var foundWindowsNodes []string
	for _, slice := range endpointSlices.Items {
		for _, endpoint := range slice.Endpoints {
			// Check if this endpoint references a node
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Node" {
				continue
			}
			// Try to get the node to check its OS
			node, err := tc.client.K8s.CoreV1().Nodes().Get(context.Background(),
				endpoint.TargetRef.Name, metav1.GetOptions{})
			require.NoError(t, err, "node %s referenced in EndpointSlice not found - EndpointSlice not synced properly",
				endpoint.TargetRef.Name)
			// Check if this is a Windows node
			if nodeOS, exists := node.Labels["kubernetes.io/os"]; exists && nodeOS == "windows" {
				foundWindowsNodes = append(foundWindowsNodes, node.Name)
			}
		}
	}

	require.Empty(t, foundWindowsNodes,
		"Found Windows nodes in kubelet EndpointSlices after deletion: %v", foundWindowsNodes)
}

// PrometheusQuery defines the result of the /query request
// Example Reference of Prometheus Query Response: https://prometheus.io/docs/prometheus/latest/querying/api/
type PrometheusQuery struct {
	Status string `json:"status"`
	Data   Data   `json:"data"`
}

// Data defines the response content of the prometheus server to the given request
type Data struct {
	Result []Result `json:"result"`
}

// Result specifies information about the metric in the query and the resulting value
type Result struct {
	Metric Metric        `json:"metric"`
	Value  []interface{} `json:"value"`
}

// Metric holds information regarding the metric defined in the query
type Metric struct {
	Instance string `json:"instance"`
}

// testWindowsPrometheusRules tests if prometheus rules specific to Windows are defined by WMCO
// It also tests the Prometheus queries by sending http requests to Prometheus server.
func (tc *testContext) testWindowsPrometheusRules(t *testing.T) {
	// test if PrometheusRule object exists in WMCO repo
	promRule, err := tc.client.Monitoring.PrometheusRules(wmcoNamespace).Get(context.TODO(), prometheusRule, metav1.GetOptions{})
	require.NoError(t, err)
	// test if rules specific to windows exist
	require.Equal(t, windowsRuleName, promRule.Spec.Groups[0].Name)

	// get route to access Prometheus server instance in monitoring namespace
	prometheusRoute, err := tc.client.Route.Routes(monitoringNamespace).Get(context.TODO(), "prometheus-k8s", metav1.GetOptions{})
	require.NoError(t, err, "error getting route")

	prometheusToken, err := tc.getPrometheusToken()
	require.NoError(t, err)

	// Following tests carry out api calls to Prometheus server with all the queries defined in the Windows PrometheusRule
	// object. The tests check if the query results have instances corresponding to configured Windows Nodes.
	// They also test if the metrics returned have a non-zero value.
	for _, rules := range promRule.Spec.Groups {
		if rules.Name != windowsRuleName {
			continue
		}
		for _, winRules := range rules.Rules {
			t.Run("Query: "+winRules.Record, func(t *testing.T) {
				queryResult, err := makePrometheusQuery(prometheusRoute.Spec.Host, winRules.Record, prometheusToken)
				require.NoError(t, err)

				// test query result against every Windows node
				for _, node := range gc.allNodes() {
					t.Run(node.Name, func(t *testing.T) {
						for _, metric := range queryResult {
							if metric.Metric.Instance == node.Name {
								// metricValue is of format : [unixTime, "value"]. Convert string value to float
								metricValue, err := strconv.ParseFloat(metric.Value[1].(string), 64)
								require.NoError(t, err)
								// test if the metric value is non-zero
								require.Truef(t, metricValue > float64(0), "expected a non zero metric value for "+
									"metric %v for instance %v", winRules.Record, node.Name)
							}
						}

					})
				}
			})
		}

	}
}

// getPrometheusToken returns authorization token required to access Prometheus server, the default ExpirationSeconds
// value is 1 hour
func (tc *testContext) getPrometheusToken() (string, error) {
	tokenRequest := &auth.TokenRequest{
		Spec: auth.TokenRequestSpec{},
	}
	token, err := tc.client.K8s.CoreV1().ServiceAccounts(monitoringNamespace).CreateToken(context.TODO(),
		"prometheus-k8s", tokenRequest, metav1.CreateOptions{})

	if err != nil {
		return "", fmt.Errorf("could not get bearer token for service account %s: %w", "prometheus-k8s", err)
	}
	return token.Status.Token, nil
}

// ensureMonitoringIsEnabled adds the "openshift.io/cluster-monitoring:"true"" label to the
// openshift-windows-machine-config-operator namespace if it is not present. If the label is applied, it restarts the
// WMCO deployment so that WMCO is aware that monitoring is enabled.
func (tc *testContext) ensureMonitoringIsEnabled() error {
	namespace, err := tc.client.K8s.CoreV1().Namespaces().Get(context.TODO(), wmcoNamespace, metav1.GetOptions{})
	if err != nil {
		return err
	}

	monitoringLabel := "openshift.io/cluster-monitoring"
	value, ok := namespace.GetLabels()[monitoringLabel]
	if !ok || value != "true" {
		if _, err = tc.client.K8s.CoreV1().Namespaces().Patch(context.TODO(), wmcoNamespace, types.MergePatchType,
			[]byte(fmt.Sprintf(`{"metadata":{"labels":{"%s":"%s"}}}`, monitoringLabel, "true")),
			metav1.PatchOptions{}); err != nil {
			return err
		}
		// Scale down the WMCO deployment to 0
		if err = tc.scaleWMCODeployment(0); err != nil {
			return err
		}
		// Scale up the WMCO deployment to 1, so that WMCO is aware that monitoring is enabled
		if err = tc.scaleWMCODeployment(1); err != nil {
			return err
		}
	}

	return nil
}

// testNodeResourceUsage ensures information on available resources is retrievable from Windows nodes
func (tc *testContext) testNodeResourceUsage(t *testing.T) {
	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")

	for _, winNode := range gc.allNodes() {
		t.Run(winNode.Name, func(t *testing.T) {
			// check available CPU, memory, and emphemeral local storage
			nodeCPU := winNode.Status.Allocatable.Cpu().AsApproximateFloat64()
			assert.Truef(t, nodeCPU > float64(0), "expected strictly positive CPU value but got %f", nodeCPU)

			nodeMemory, isValidAmount := winNode.Status.Allocatable.Memory().AsInt64()
			assert.Truef(t, isValidAmount, "expected numeric quantity (bytes) for node %s memory", winNode.Name)
			assert.Truef(t, nodeMemory > 0, "expected strictly positive memory value but got %d", nodeMemory)

			nodeStorage, isValidAmount := winNode.Status.Allocatable.StorageEphemeral().AsInt64()
			assert.Truef(t, isValidAmount, "expected numeric quantity for node %s filesystem storage", winNode.Name)
			assert.Truef(t, nodeStorage > 0, "expected strictly positive storage value but got %d", nodeStorage)
		})
	}
}

// testPodMetrics asserts that the expected metrics exist for a Windows pod
func (tc *testContext) testPodMetrics(t *testing.T, podName string) {
	// get route to access Prometheus server instance in monitoring namespace
	prometheusRoute, err := tc.client.Route.Routes(monitoringNamespace).Get(context.TODO(), "prometheus-k8s", metav1.GetOptions{})
	require.NoError(t, err, "error getting route")

	prometheusToken, err := tc.getPrometheusToken()
	require.NoError(t, err)

	// The queries here should mirror those made in the OCP console
	queries := []string{
		fmt.Sprintf("pod:container_cpu_usage:sum{pod='%s',namespace='%s'}", podName, tc.workloadNamespace),
		fmt.Sprintf("sum(container_memory_working_set_bytes{pod='%s',namespace='%s',container='',}) BY (pod, namespace)",
			podName, tc.workloadNamespace),
		fmt.Sprintf("pod_interface_network:container_network_receive_bytes:irate5m{pod='%s',namespace='%s'}", podName, tc.workloadNamespace),
		fmt.Sprintf("pod_interface_network:container_network_transmit_bytes_total:irate5m{pod='%s',namespace='%s'}", podName, tc.workloadNamespace),
	}
	// Use extended timeout to account for EndpointSlice propagation, metric scraping,
	// and recording rule evaluation when using EndpointSlice-based service discovery
	podMetricsTimeout := 5 * time.Minute
	for i, query := range queries {
		t.Run("query "+strconv.Itoa(i), func(t *testing.T) {
			err := wait.PollUntilContextTimeout(context.TODO(), retry.Interval, podMetricsTimeout, true,
				func(ctx context.Context) (done bool, err error) {
					results, err := makePrometheusQuery(prometheusRoute.Spec.Host, query, prometheusToken)
					if err != nil {
						log.Printf("error making prometheus query, retrying: %s", err)
						return false, nil
					}
					return len(results) == 1, nil
				})
			assert.NoError(t, err)
		})
	}
}

// makePrometheusQuery runs the given query against the prometheus server at the given address
func makePrometheusQuery(address, query, token string) ([]Result, error) {
	escapedQuery := url.QueryEscape(query)
	url := "https://" + address + "/api/v1/query?query=" + escapedQuery
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error forming GET request %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	tr := &http.Transport{
		// InsecureSkipVerify is required to avoid errors due to bad certificate
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making GET request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request returned status code %d", resp.StatusCode)
	}
	var promQuery *PrometheusQuery
	body, _ := io.ReadAll(resp.Body)
	if err = json.Unmarshal(body, &promQuery); err != nil {
		return nil, fmt.Errorf("error unmarshalling response %s: %w", string(body), err)
	}
	if promQuery.Status != "success" {
		return nil, fmt.Errorf("query had status %s, expected 'success'", promQuery.Status)
	}
	return promQuery.Data.Result, nil
}

// tlsVersionToGoVersion maps OpenShift TLSProtocolVersion to Go crypto/tls version constants
var tlsVersionToGoVersion = map[config.TLSProtocolVersion]uint16{
	config.VersionTLS10: tls.VersionTLS10,
	config.VersionTLS11: tls.VersionTLS11,
	config.VersionTLS12: tls.VersionTLS12,
	config.VersionTLS13: tls.VersionTLS13,
}

// getEffectiveTLSProfile fetches the cluster APIServer resource and resolves the effective TLS profile spec
func (tc *testContext) getEffectiveTLSProfile() (*config.TLSSecurityProfile, config.TLSProfileSpec, error) {
	apiServer, err := tc.client.Config.ConfigV1().APIServers().Get(context.TODO(), "cluster", metav1.GetOptions{})
	if err != nil {
		return nil, config.TLSProfileSpec{}, fmt.Errorf("error getting APIServer resource: %w", err)
	}
	profileSpec, err := tlspkg.GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
	if err != nil {
		return nil, config.TLSProfileSpec{}, fmt.Errorf("error resolving TLS profile spec: %w", err)
	}
	return apiServer.Spec.TLSSecurityProfile, profileSpec, nil
}

// getWebConfigContent retrieves the content of the windows-exporter webconfig YAML file from a Windows node
func (tc *testContext) getWebConfigContent(addr string) (string, error) {
	return tc.getWebConfigContentWithContext(context.Background(), addr)
}

func (tc *testContext) getWebConfigContentWithContext(ctx context.Context, addr string) (string, error) {
	command := fmt.Sprintf("[Convert]::ToBase64String([IO.File]::ReadAllBytes(\\\"%s\\\"))", windows.TLSConfPath)
	out, err := tc.runPowerShellSSHJobWithContext(ctx, "webconfig-content", command, addr)
	if err != nil {
		return "", fmt.Errorf("error running SSH job to get webconfig: %w", err)
	}
	content, err := extractBase64Payload(out)
	if err != nil {
		return "", fmt.Errorf("error extracting webconfig content: %w", err)
	}
	return string(content), nil
}

// extractBase64Payload extracts and decodes a base64-encoded payload from SSH
// job output. The output format is "<pod-name>: <stdout>\n" where stdout may
// contain noisy preamble from the SSH shell-detection probe (e.g. Get-Help
// output) before the actual base64 payload line. This function strips the pod
// name prefix, then walks lines from the end to find the last non-empty line,
// which is where the command's payload appears.
func extractBase64Payload(out string) ([]byte, error) {
	_, after, found := strings.Cut(out, ": ")
	if !found {
		return nil, fmt.Errorf("missing pod name prefix in job output")
	}
	lines := strings.Split(strings.TrimSpace(after), "\n")
	// Walk backwards to find the last non-empty line — the actual payload.
	// Earlier lines may contain noisy shell-detection output (e.g. Get-Help).
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("error decoding base64 payload: %w", err)
		}
		return decoded, nil
	}
	return nil, fmt.Errorf("no payload line found in job output")
}

// testWebConfigTLSAdherence is the top-level test group for webconfig TLS profile tests.
// All sub-tests run serially and the original TLS profile is restored in cleanup.
func (tc *testContext) testWebConfigTLSAdherence(t *testing.T) {
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node")

	apiServer, err := tc.client.Config.ConfigV1().APIServers().Get(context.TODO(), "cluster", metav1.GetOptions{})
	require.NoError(t, err, "error getting APIServer resource")
	if !libgocrypto.ShouldHonorClusterTLSProfile(apiServer.Spec.TLSAdherence) {
		t.Skip("WMCO does not honor the cluster TLS profile under the current adherence policy")
	}

	if !t.Run("TLS profile reflected in webconfig", tc.testWebConfigMatchesTLSProfile) {
		return
	}
	t.Run("TLS profile change updates node without reconfigure", tc.testWebConfigProfileChangeWithoutReconfigure)
}

// testWebConfigMatchesTLSProfile verifies that each Windows node's webconfig file matches
// the cluster's effective TLS profile and that the SHA annotation matches the file content.
func (tc *testContext) testWebConfigMatchesTLSProfile(t *testing.T) {
	_, profileSpec, err := tc.getEffectiveTLSProfile()
	require.NoError(t, err)

	// Generate the expected webconfig from the effective TLS profile.
	// honorTLSProfile is true because we are testing the TLS-aware webconfig.
	expectedContent, _, err := payload.GenerateWebConfig(profileSpec, true)
	require.NoError(t, err)
	expectedSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(expectedContent)))

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "error getting node address")

			t.Run("webconfig content matches TLS profile", func(t *testing.T) {
				err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
					content, err := tc.getWebConfigContent(addr)
					if err != nil {
						log.Printf("error fetching webconfig from node %s: %v, retrying...",
							node.GetName(), err)
						return false, nil
					}
					if content != expectedContent {
						log.Printf("webconfig mismatch on node %s, retrying...", node.GetName())
						return false, nil
					}
					return true, nil
				})
				assert.NoError(t, err, "webconfig content does not match expected TLS profile on node %s",
					node.GetName())
			})

			t.Run("SHA annotation matches file content", func(t *testing.T) {
				err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
					// Re-fetch node to get the latest annotations
					freshNode, err := tc.client.K8s.CoreV1().Nodes().Get(context.TODO(),
						node.GetName(), metav1.GetOptions{})
					if err != nil {
						log.Printf("error fetching node %s: %v, retrying...", node.GetName(), err)
						return false, nil
					}
					content, err := tc.getWebConfigContent(addr)
					if err != nil {
						log.Printf("error fetching webconfig from node %s: %v, retrying...",
							node.GetName(), err)
						return false, nil
					}
					actualSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
					annotationSHA, exists := freshNode.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation]
					if !exists {
						log.Printf("node %s missing webconfig SHA annotation, retrying...",
							node.GetName())
						return false, nil
					}
					if annotationSHA != actualSHA {
						log.Printf("SHA mismatch on node %s: annotation=%s, actual=%s, retrying...",
							node.GetName(), annotationSHA, actualSHA)
						return false, nil
					}
					if annotationSHA != expectedSHA {
						log.Printf("SHA does not match expected value on node %s: annotation=%s, expected=%s, retrying...",
							node.GetName(), annotationSHA, expectedSHA)
						return false, nil
					}
					return true, nil
				})
				assert.NoError(t, err, "webconfig SHA annotation mismatch on node %s", node.GetName())
			})
		})
	}
}

// testWebConfigProfileChangeWithoutReconfigure verifies that changing the cluster TLS profile
// updates the webconfig on Windows nodes via lightweight reconciliation (without a full
// deconfigure+reconfigure cycle). This is confirmed by verifying the WMCO version annotation
// remains unchanged.
func (tc *testContext) testWebConfigProfileChangeWithoutReconfigure(t *testing.T) {
	nodes, err := tc.listFullyConfiguredWindowsNodes(false)
	require.NoError(t, err)
	if len(nodes) == 0 {
		t.Skip("test requires a configured Machine-API Windows node")
	}
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err)
	var node *core.Node
	for _, candidate := range nodes {
		if candidate.Annotations[metadata.VersionAnnotation] == operatorVersion {
			node = candidate.DeepCopy()
			break
		}
	}
	if node == nil {
		t.Skip("test requires a current-version Machine-API Windows node")
	}
	addr, err := controllers.GetAddress(node.Status.Addresses)
	require.NoError(t, err, "error getting node address")
	originalProfile, originalSpec, err := tc.getEffectiveTLSProfile()
	require.NoError(t, err)
	originalContent, _, err := payload.GenerateWebConfig(originalSpec, true)
	require.NoError(t, err)
	originalSHA := sha256.Sum256([]byte(originalContent))
	if node.Status.NodeInfo.BootID == "" {
		t.Log("node does not report a boot ID; only UID and WMCO version can be checked")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*retry.Timeout)
		defer cancel()
		err := tc.updateWebConfigTLSProfile(ctx, originalProfile)
		if originalProfile == nil && apierrors.IsInvalid(err) {
			t.Log("API requires an explicit TLS profile; restoring Intermediate, the effective default for an unset profile")
			err = tc.updateWebConfigTLSProfile(ctx, &config.TLSSecurityProfile{
				Type: config.TLSProfileIntermediateType, Intermediate: &config.IntermediateTLSProfile{},
			})
		}
		require.NoError(t, err, "cleanup: failed to restore original effective TLS profile")
		require.NoError(t, tc.waitForWebConfig(ctx, *node, addr, originalContent),
			"cleanup: original webconfig and SHA did not converge on the same node")
	})
	previousContent := originalContent
	for _, profileType := range []config.TLSProfileType{config.TLSProfileIntermediateType, config.TLSProfileModernType} {
		profile := &config.TLSSecurityProfile{Type: profileType}
		if profileType == config.TLSProfileIntermediateType {
			profile.Intermediate = &config.IntermediateTLSProfile{}
		} else {
			profile.Modern = &config.ModernTLSProfile{}
		}
		profileSpec, err := tlspkg.GetTLSProfileSpec(profile)
		require.NoError(t, err)
		expectedContent, _, err := payload.GenerateWebConfig(profileSpec, true)
		require.NoError(t, err)
		if profileSpec.MinTLSVersion != originalSpec.MinTLSVersion {
			require.NotEqual(t, originalContent, expectedContent)
			require.NotEqual(t, originalSHA, sha256.Sum256([]byte(expectedContent)))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*retry.Timeout)
		if expectedContent != previousContent {
			require.NotEqual(t, sha256.Sum256([]byte(previousContent)), sha256.Sum256([]byte(expectedContent)))
			err = tc.updateWebConfigTLSProfile(ctx, profile)
		}
		if err == nil {
			err = tc.waitForWebConfig(ctx, *node, addr, expectedContent)
		}
		cancel()
		require.NoError(t, err, "webconfig did not converge to %s on the same current-version node", profileType)
		if !t.Run(string(profileType)+" TLS handshakes", func(t *testing.T) {
			tc.testWebConfigTLSHandshakeEnforcement(t, *node, profileSpec.MinTLSVersion)
		}) {
			return
		}
		previousContent = expectedContent
	}
}

func (tc *testContext) updateWebConfigTLSProfile(ctx context.Context, profile *config.TLSSecurityProfile) error {
	return wait.PollUntilContextCancel(ctx, retry.Interval, true, func(ctx context.Context) (bool, error) {
		apiServer, err := tc.client.Config.ConfigV1().APIServers().Get(ctx, "cluster", metav1.GetOptions{})
		if err == nil {
			apiServer.Spec.TLSSecurityProfile = profile
			_, err = tc.client.Config.ConfigV1().APIServers().Update(ctx, apiServer, metav1.UpdateOptions{})
		}
		if apierrors.IsInvalid(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return false, err
		}
		if err != nil {
			log.Printf("error updating TLS security profile, retrying: %v", err)
		}
		return err == nil, nil
	})
}

func (tc *testContext) waitForWebConfig(ctx context.Context, originalNode core.Node, addr, expectedContent string) error {
	expectedSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(expectedContent)))
	return wait.PollUntilContextCancel(ctx, retry.Interval, true, func(ctx context.Context) (bool, error) {
		node, err := tc.client.K8s.CoreV1().Nodes().Get(ctx, originalNode.Name, metav1.GetOptions{})
		if err != nil {
			log.Printf("error fetching node %s, retrying: %v", originalNode.Name, err)
			return false, nil
		}
		if node.UID != originalNode.UID || node.Annotations[metadata.VersionAnnotation] != originalNode.Annotations[metadata.VersionAnnotation] ||
			(originalNode.Status.NodeInfo.BootID != "" && node.Status.NodeInfo.BootID != originalNode.Status.NodeInfo.BootID) {
			return false, fmt.Errorf("node %s UID, boot ID, or WMCO version changed", node.Name)
		}
		content, err := tc.getWebConfigContentWithContext(ctx, addr)
		if err != nil {
			log.Printf("error reading webconfig on %s, retrying: %v", node.Name, err)
			return false, nil
		}
		return content == expectedContent && node.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation] == expectedSHA, nil
	})
}

// testWebConfigTLSHandshakeEnforcement verifies that the windows-exporter actually enforces
// the TLS profile during real TLS handshakes: connections with allowed TLS versions succeed,
// connections with disallowed versions fail, and valid HTTPS requests return metrics.
func (tc *testContext) testWebConfigTLSHandshakeEnforcement(t *testing.T, node core.Node, minTLSVersion config.TLSProtocolVersion) {
	nodeIP := ""
	for _, address := range node.Status.Addresses {
		if address.Type == core.NodeInternalIP {
			nodeIP = address.Address
		}
	}
	require.NotEmpty(t, nodeIP, "test requires Windows node %s to have internal IP", node.GetName())

	exporterAddr := net.JoinHostPort(nodeIP, strconv.Itoa(int(metrics.Port)))
	minVersion, supported := tlsVersionToGoVersion[minTLSVersion]
	require.True(t, supported, "unsupported TLS version %s", minTLSVersion)

	t.Run("allowed TLS version succeeds", func(t *testing.T) {
		err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
			err := dialWebConfigTLS(exporterAddr, minVersion)
			if err != nil {
				log.Printf("TLS handshake with allowed version failed on %s: %v, retrying...",
					node.GetName(), err)
				return false, nil
			}
			return true, nil
		})
		assert.NoError(t, err,
			"TLS handshake with allowed version (0x%04x) should succeed on node %s",
			minVersion, node.GetName())
	})

	t.Run("disallowed TLS version fails", func(t *testing.T) {
		if minVersion != tls.VersionTLS13 {
			t.Skip("TLS 1.2 is the compatibility control; rejection is tested after switching to TLS 1.3")
		}
		disallowedVersion := uint16(tls.VersionTLS12)
		err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
			err := dialWebConfigTLS(exporterAddr, disallowedVersion)
			if err != nil {
				if isWebConfigProtocolRejection(err) {
					return true, nil
				}
				return false, fmt.Errorf("expected remote protocol-version rejection, got: %w", err)
			}
			log.Printf("TLS handshake with disallowed version unexpectedly succeeded on %s, retrying...",
				node.GetName())
			return false, nil
		})
		assert.NoError(t, err,
			"TLS handshake with disallowed version (0x%04x) should fail on node %s",
			disallowedVersion, node.GetName())
	})

	t.Run("HTTPS metrics request succeeds", func(t *testing.T) {
		err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{
					// InsecureSkipVerify: windows-exporter uses a self-signed certificate
					InsecureSkipVerify: true,
					MinVersion:         minVersion,
				},
			}
			client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
			defer tr.CloseIdleConnections()
			metricsURL := fmt.Sprintf("https://%s/%s", exporterAddr, metrics.PortName)
			resp, err := client.Get(metricsURL)
			if err != nil {
				log.Printf("HTTPS metrics request failed on %s: %v, retrying...",
					node.GetName(), err)
				return false, nil
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Printf("HTTPS metrics request returned status %d on %s, retrying...",
					resp.StatusCode, node.GetName())
				return false, nil
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("error reading metrics response body from %s: %v, retrying...",
					node.GetName(), err)
				return false, nil
			}
			// Verify the response contains Prometheus-style metrics
			if !strings.Contains(string(body), "# HELP") || !strings.Contains(string(body), "# TYPE") {
				log.Printf("metrics response does not contain valid Prometheus metrics on %s, retrying...",
					node.GetName())
				return false, nil
			}
			return true, nil
		})
		assert.NoError(t, err, "HTTPS metrics request should succeed on node %s", node.GetName())
	})
}

func dialWebConfigTLS(address string, version uint16) error {
	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", address,
		&tls.Config{InsecureSkipVerify: true, MinVersion: version, MaxVersion: version})
	if err != nil {
		return err
	}
	return connection.Close()
}

func isWebConfigProtocolRejection(err error) bool {
	var operationError *net.OpError
	return errors.As(err, &operationError) && operationError.Op == "remote error" && operationError.Err != nil &&
		operationError.Err.Error() == "tls: protocol version not supported"
}

func TestWebConfigTLSHelpers(t *testing.T) {
	for _, minimumVersion := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(fmt.Sprintf("TLS_%x", minimumVersion), func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			server.TLS = &tls.Config{MinVersion: minimumVersion, MaxVersion: minimumVersion}
			server.StartTLS()
			defer server.Close()
			require.NoError(t, dialWebConfigTLS(server.Listener.Addr().String(), minimumVersion))
			err := dialWebConfigTLS(server.Listener.Addr().String(), tls.VersionTLS12)
			if minimumVersion == tls.VersionTLS12 {
				require.NoError(t, err, "TLS 1.2 compatibility control must succeed")
			} else {
				require.Error(t, err)
				assert.True(t, isWebConfigProtocolRejection(err), "unexpected rejection: %v", err)
			}
		})
	}
	for _, err := range []error{nil, context.DeadlineExceeded, io.EOF,
		&net.OpError{Op: "dial", Err: errors.New("connection refused")},
		&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")},
		&net.OpError{Op: "local error", Err: errors.New("tls: protocol version not supported")}} {
		assert.False(t, isWebConfigProtocolRejection(err), "must not accept unrelated failure: %v", err)
	}
	intermediate, err := tlspkg.GetTLSProfileSpec(&config.TLSSecurityProfile{Type: config.TLSProfileIntermediateType})
	require.NoError(t, err)
	modern, err := tlspkg.GetTLSProfileSpec(&config.TLSSecurityProfile{Type: config.TLSProfileModernType})
	require.NoError(t, err)
	require.NotEqual(t, intermediate.MinTLSVersion, modern.MinTLSVersion)
	intermediateContent, _, err := payload.GenerateWebConfig(intermediate, true)
	require.NoError(t, err)
	modernContent, _, err := payload.GenerateWebConfig(modern, true)
	require.NoError(t, err)
	assert.NotEqual(t, intermediateContent, modernContent)
	assert.NotEqual(t, sha256.Sum256([]byte(intermediateContent)), sha256.Sum256([]byte(modernContent)))
}
