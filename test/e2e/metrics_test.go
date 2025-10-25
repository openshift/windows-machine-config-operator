package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	auth "k8s.io/api/authentication/v1"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
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
	for i, query := range queries {
		t.Run("query "+strconv.Itoa(i), func(t *testing.T) {
			err := wait.PollUntilContextTimeout(context.TODO(), retry.Interval, retry.ResourceChangeTimeout, true,
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
