package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

// testWindowsExporter deploys a Linux pod and tests that it can communicate with the Windows node's metrics port
func testWindowsExporter(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// Need at least one Windows node to run these tests, throwing error if this condition is not met
	require.GreaterOrEqualf(t, len(gc.nodes), 1, "insufficient number of Windows nodes to run tests across"+
		" nodes, Minimum node count: 1, Current node count: %d", len(gc.nodes))

	for _, winNode := range gc.nodes {
		t.Run(winNode.Name, func(t *testing.T) {
			// Get the node internal IP so we can curl it
			var winNodeInternalIP string
			for _, address := range winNode.Status.Addresses {
				if address.Type == core.NodeInternalIP {
					winNodeInternalIP = address.Address
				}
			}

			// This will curl the windows server. curl must be present in the container image.
			linuxCurlerCommand := []string{"bash", "-c", "curl http://" + winNodeInternalIP + ":9182/metrics"}
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
