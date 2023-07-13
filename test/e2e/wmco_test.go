package e2e

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/controllers"
)

var (
	nodeCreationTime  = time.Minute * 35
	nodeRetryInterval = time.Minute * 1
	// deploymentRetries is the amount of time to retry creating a Windows Server deployment, to compensate for the
	// time it takes to download the Server image to the node
	deploymentRetries = 10
)

// TestWMCO sets up the testing suite for WMCO.
func TestWMCO(t *testing.T) {
	gc.numberOfMachineNodes = int32(numberOfMachineNodes)
	gc.numberOfBYOHNodes = int32(numberOfBYOHNodes)
	require.NotEmpty(t, privateKeyPath, "private-key-path is not set")
	gc.privateKeyPath = privateKeyPath

	tc, err := NewTestContext()
	require.NoError(t, err)
	log.Printf("Testing against Windows Server %s\n", tc.windowsServerVersion)
	// Create the namespace test resources can be deployed in, as well as required resources within said namespace.
	require.NoError(t, tc.ensureNamespace(tc.workloadNamespace, tc.workloadNamespaceLabels), "error creating test namespace")
	require.NoError(t, tc.sshSetup(), "unable to setup SSH requirements")

	// When the upgrade test is run from CI, the namespace that gets created does not have the required monitoring
	// label, so we ensure that it gets applied and the WMCO deployment is restarted.
	require.NoError(t, tc.ensureMonitoringIsEnabled(), "error ensuring monitoring is enabled")

	// Attempt to collect all Node logs if tests fail
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		log.Printf("test failed, attempting to gather Node logs")
		tc.collectNodeLogs()
	})

	// test that the operator can deploy without the secret already created, we can later use a secret created by the
	// individual test suites after the operator is running
	t.Run("operator deployed without private key secret", testOperatorDeployed)
	t.Run("create", creationTestSuite)
	t.Run("network", testNetwork)
	t.Run("storage", testStorage)
	t.Run("service reconciliation", testDependentServiceChanges)
	t.Run("cluster-wide proxy", proxyTestSuite)
	t.Run("upgrade", upgradeTestSuite)
	// The reconfigurationTestSuite must be run directly before the deletionTestSuite. This is because we do not
	// currently wait for nodes to fully reconcile after changing the private key back to the valid key. Any tests
	// added/moved in between these two suites may fail.
	// This limitation will be removed with https://issues.redhat.com/browse/WINC-655
	t.Run("reconfigure", reconfigurationTestSuite)
	t.Run("destroy", deletionTestSuite)
}

// testOperatorDeployed tests that the operator pod is running
func testOperatorDeployed(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	deployment, err := tc.client.K8s.AppsV1().Deployments(wmcoNamespace).Get(context.TODO(),
		"windows-machine-config-operator", meta.GetOptions{})
	require.NoError(t, err, "could not get WMCO deployment")
	require.NotZerof(t, deployment.Status.AvailableReplicas, "WMCO deployment has no available replicas: %v", deployment)
}

// collectNodeLogs collects the logs from each node
func (tc *testContext) collectNodeLogs() {
	for _, node := range gc.allNodes() {
		addr, err := controllers.GetAddress(node.Status.Addresses)
		if err != nil {
			log.Printf("unable to get address for %s: %s", node.GetName(), err)
			continue
		}
		err = tc.nodelessLogCollection(node.GetName(), addr)
		if err != nil {
			log.Printf("unable to collect logs from %s: %s", node.GetName(), err)
		}
	}

}
