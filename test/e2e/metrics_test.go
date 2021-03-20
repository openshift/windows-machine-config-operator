package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	prometheusRule      = "windows-prometheus-k8s-rules"
	monitoringNamespace = "openshift-monitoring"
	windowsRuleName     = "windows.rules"
)

func testMetrics(t *testing.T) {
	t.Run("Windows Exporter configuration validation", testWindowsExporter)
	t.Run("Prometheus configuration validation", testPrometheus)
	t.Run("Windows Prometheus rules validation", testWindowsPrometheusRules)
}

// testWindowsExporter deploys Linux pod and tests that it can communicate with Windows node's metrics port
func testWindowsExporter(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.Greater(t, len(gc.nodes), 0, "test requires at least one Windows node to run")

	for _, winNode := range gc.nodes {
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

			// This will curl the windows server. curl must be present in the container image.
			linuxCurlerCommand := []string{"bash", "-c", "curl http://" + winNodeInternalIP + ":" +
				strconv.Itoa(int(metrics.Port)) + "/" + metrics.PortName}
			linuxCurlerJob, err := testCtx.createLinuxJob("linux-curler-"+strings.ToLower(winNode.Status.NodeInfo.MachineID),
				linuxCurlerCommand)
			require.NoError(t, err, "could not create Linux job")
			// delete the job created
			defer testCtx.deleteJob(linuxCurlerJob.Name)

			err = testCtx.waitUntilJobSucceeds(linuxCurlerJob.Name)
			assert.NoError(t, err, "could not curl the Windows VM metrics endpoint from a linux container")
		})
	}
}

// testPrometheus tests if Prometheus is configured to scrape metrics endpoints
func testPrometheus(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	// check that service exists
	_, err = testCtx.client.K8s.CoreV1().Services("openshift-windows-machine-config-operator").Get(context.TODO(),
		metrics.WindowsMetricsResource, metav1.GetOptions{})
	require.NoError(t, err)

	// check that SM existS
	_, err = testCtx.client.Monitoring.ServiceMonitors(testCtx.namespace).Get(context.TODO(),
		metrics.WindowsMetricsResource, metav1.GetOptions{})
	require.NoError(t, err, "error getting service monitor")

	// check that endpoints exists
	windowsEndpoints, err := testCtx.client.K8s.CoreV1().Endpoints(
		"openshift-windows-machine-config-operator").Get(context.TODO(),
		metrics.WindowsMetricsResource, metav1.GetOptions{})
	require.NoError(t, err)

	if gc.numberOfNodes == 0 {
		// check if all entries in subset are deleted when there are no Windows Nodes
		require.Equal(t, 0, len(windowsEndpoints.Subsets))
	} else {
		// Total length of list for subsets is always equal to the list of Windows Nodes.
		require.Equal(t, gc.numberOfNodes, int32(len(windowsEndpoints.Subsets[0].Addresses)))

		// check Nodes in the targetRef of Endpoints are same as the Windows Nodes bootstrapped using WMCO
		err = checkTargetNodes(windowsEndpoints)
		require.NoError(t, err)

		// check Port name matches
		require.Equal(t, windowsEndpoints.Subsets[0].Ports[0].Name, metrics.PortName)
		// check Port matches the defined port
		require.Equal(t, windowsEndpoints.Subsets[0].Ports[0].Port, metrics.Port)
	}

}

// checkTargetNodes checks if nodes in the targetRef of Endpoints are same as the Windows Nodes bootstrapped using WMCO
func checkTargetNodes(windowsEndpoints *core.Endpoints) error {
	for _, node := range gc.nodes {
		foundNode := false
		for _, endpointAddress := range windowsEndpoints.Subsets[0].Addresses {
			if node.Name == endpointAddress.TargetRef.Name {
				foundNode = true
				break
			}
		}
		if !foundNode {
			return errors.New("target node not found in Endpoints object ")
		}
	}

	return nil
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
func testWindowsPrometheusRules(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	// test if PrometheusRule object exists in WMCO repo
	promRule, err := tc.client.Monitoring.PrometheusRules(tc.namespace).Get(context.TODO(), prometheusRule, metav1.GetOptions{})
	require.NoError(t, err)
	// test if rules specific to windows exist
	require.Equal(t, windowsRuleName, promRule.Spec.Groups[0].Name)

	// get route to access Prometheus server instance in monitoring namespace
	prometheusRoute, err := tc.client.Route.Routes(monitoringNamespace).Get(context.TODO(), "prometheus-k8s", metav1.GetOptions{})
	require.NoError(t, err, "error getting route")

	// get Authorization token
	prometheusToken, err := tc.getPrometheusToken()
	require.NoError(t, err, "Error getting Prometheus token")
	// define authorization token required to call Prometheus API
	var bearer = "Bearer " + prometheusToken
	// InsecureSkipVerify is required to avoid errors due to bad certificate
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}

	// Following tests carry out api calls to Prometheus server with all the queries defined in the Windows PrometheusRule
	// object. The tests check if the query results have instances corresponding to configured Windows Nodes.
	// They also test if the metrics returned have a non-zero value.
	for _, rules := range promRule.Spec.Groups {
		if rules.Name != windowsRuleName {
			continue
		}
		for _, winRules := range rules.Rules {
			t.Run("Query: "+winRules.Record, func(t *testing.T) {
				// url consists of prometheus host, appended with a Record defined in the Windows PrometheusRuleObject
				url := "https://" + prometheusRoute.Spec.Host + "/api/v1/query?query=" + winRules.Record
				// create a Get request
				req, err := http.NewRequest("GET", url, nil)
				require.NoError(t, err)
				// add Authorization Header to get access Prometheus server
				req.Header.Set("Authorization", bearer)

				resp, err := client.Do(req)
				require.NoError(t, err)
				// Convert the request response as a data type.
				body, _ := ioutil.ReadAll(resp.Body)
				var promQuery = new(PrometheusQuery)
				err = json.Unmarshal(body, &promQuery)
				require.NoError(t, err)

				// test if query status is successful
				require.Equal(t, "success", promQuery.Status)
				queryResult := promQuery.Data.Result

				// test query result against every Windows node
				for _, node := range gc.nodes {
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

// getPrometheusToken returns authorization token required to access Prometheus server
func (tc *testContext) getPrometheusToken() (string, error) {
	// get secrets from monitoring namespace
	monitoringSecrets, err := tc.client.K8s.CoreV1().Secrets(monitoringNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return "", err
	}

	// access prometheus-k8s-token-* secret
	var secretName string
	for _, secret := range monitoringSecrets.Items {
		if strings.Contains(secret.Name, "prometheus-k8s-token") {
			secretName = secret.Name
			break
		}
	}

	if len(secretName) == 0 {
		return "", fmt.Errorf("could not get 'prometheus-k8s-token' secret")
	}

	secret, err := tc.client.K8s.CoreV1().Secrets(monitoringNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	token, ok := secret.Data["token"]

	if !ok {
		return "", fmt.Errorf("could not get bearer token for secret %v", secretName)
	}
	return string(token), nil
}
