package e2e

import (
	"testing"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/windows"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testNetwork runs all the cluster and node network tests
func testNetwork(t *testing.T) {
	t.Run("Hybrid overlay running", testHybridOverlayRunning)
	t.Run("OpenShift HNS networks", testHNSNetworksCreated)
}

// testHNSNetworksCreated tests that the required HNS Networks have been created on the bootstrapped nodes
func testHNSNetworksCreated(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	for _, vm := range gc.windowsVMs {
		// We don't need to retry as we are waiting long enough for the secrets to be created which implies that the
		// network setup has succeeded.
		stdout, _, err := vm.Run("Get-HnsNetwork", true)
		require.NoError(t, err, "Could not run Get-HnsNetwork command")
		assert.Contains(t, stdout, "BaseOpenShiftNetwork",
			"Could not find BaseOpenShiftNetwork in %s", vm.GetCredentials().GetInstanceId())
		assert.Contains(t, stdout, "OpenShiftNetwork",
			"Could not find OpenShiftNetwork in %s", vm.GetCredentials().GetInstanceId())
	}
}

// testHybridOverlayRunning checks if the hybrid-overlay process is running on all the bootstrapped nodes
func testHybridOverlayRunning(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	for _, vm := range gc.windowsVMs {
		_, stderr, err := vm.Run("Get-Process -Name \""+windows.HybridOverlayProcess+"\"", true)
		require.NoError(t, err, "Could not run Get-Process command")
		// stderr being empty implies that hybrid-overlay was running.
		assert.Equal(t, "", stderr, "hybrid-overlay was not running in %s",
			vm.GetCredentials().GetInstanceId())
	}
}
