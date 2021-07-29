package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// reconfigurationTest tests that the correct behavior occurs when a previously configured instance is configured
// again. In practice, this exact scenario should not happen, however it simulates a similar scenario where an instance
// was almost completely configured, an error occurred, and the instance is requeued. This is a scenario that should be
// expected to be ran into often enough, for reasons such as network instability. For that reason this test is warranted.
func reconfigurationTest(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)

	machineNodes, err := testCtx.listFullyConfiguredWindowsNodes(false)
	require.NoError(t, err)
	byohNodes, err := testCtx.listFullyConfiguredWindowsNodes(true)
	require.NoError(t, err)

	// Remove the version annotation of one of each type of node
	// Forward slash within a path is escaped as '~1'
	escapedVersionAnnotation := strings.Replace(metadata.VersionAnnotation, "/", "~1", -1)
	patchData := fmt.Sprintf("[{\"op\": \"remove\", \"path\": \"/metadata/annotations/%s\"}]", escapedVersionAnnotation)
	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), machineNodes[0].Name, types.JSONPatchType,
		[]byte(patchData), metav1.PatchOptions{})
	require.NoError(t, err)
	_, err = testCtx.client.K8s.CoreV1().Nodes().Patch(context.TODO(), byohNodes[0].Name, types.JSONPatchType,
		[]byte(patchData), metav1.PatchOptions{})
	require.NoError(t, err)

	// The Windows nodes should eventually be returned to the state we expect them to be in
	err = testCtx.waitForWindowsNodes(gc.numberOfMachineNodes, false, true, false)
	assert.NoError(t, err, "error waiting for Windows Machine nodes to be reconfigured")

	err = testCtx.waitForWindowsNodes(gc.numberOfBYOHNodes, false, true, true)
	assert.NoError(t, err, "error waiting for Windows BYOH nodes to be reconfigured")
}
