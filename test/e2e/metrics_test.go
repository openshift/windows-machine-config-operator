package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"

	monitoringv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/metrics"
)

func testMetrics(t *testing.T) {
	t.Run("Windows Exporter configuration validation", testWindowsExporter)
	t.Run("Prometheus configuration validation", testPrometheus)
}

// testWindowsExporter deploys Linux pod and tests that it can communicate with Windows node's metrics port
func testWindowsExporter(t *testing.T) {
	testCtx, err := NewTestContext(t)
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
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	err = framework.AddToFrameworkScheme(monitoringv1.AddToScheme, &monitoringv1.ServiceMonitor{})
	require.NoError(t, err)

	// check that service exists
	_, err = testCtx.kubeclient.CoreV1().Services("openshift-windows-machine-config-operator").Get(context.TODO(),
		"windows-machine-config-operator-metrics", metav1.GetOptions{})
	require.NoError(t, err)

	// check that SM existS
	windowsServiceMonitor := &monitoringv1.ServiceMonitor{}
	err = framework.Global.Client.Get(context.TODO(),
		types.NamespacedName{Namespace: "openshift-windows-machine-config-operator",
			Name: "windows-machine-config-operator-metrics"}, windowsServiceMonitor)
	require.NoError(t, err)

	// check that endpoints exists
	windowsEndpoints, err := testCtx.kubeclient.CoreV1().Endpoints(
		"openshift-windows-machine-config-operator").Get(context.TODO(),
		"windows-machine-config-operator-metrics", metav1.GetOptions{})
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
