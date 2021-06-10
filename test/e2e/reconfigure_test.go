package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	nc "github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
)

// reconfigurationTest tests that the correct behavior occurs when a previously configured Machine is configured
// again. In practice, this exact scenario should not happen, however it simulates a similar scenario where a Machine
// was almost completely configured, an error occurred, and the Machine is requeued. This is a scenario that should be
// expected to be ran into often enough, for reasons such as network instability. For that reason this test is warranted.
func reconfigurationTest(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	nodes, err := testCtx.listFullyConfiguredWindowsNodes(false)
	require.NoError(t, err)

	// Remove the version annotation of one of the nodes
	// Forward slash within a path is escaped as '~1'
	escapedVersionAnnotation := strings.Replace(nc.VersionAnnotation, "/", "~1", -1)
	patchData := fmt.Sprintf("[{\"op\": \"remove\", \"path\": \"/metadata/annotations/%s\"}]", escapedVersionAnnotation)
	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), nodes[0].Name, types.JSONPatchType,
		[]byte(patchData), metav1.PatchOptions{})
	require.NoError(t, err)

	// The Windows node should eventually be returned to the state we expect it to be in
	err = testCtx.waitForWindowsNodes(gc.numberOfMachineNodes, false, true, false)
	assert.NoError(t, err, "error waiting for Windows nodes to be reconfigured")
}
