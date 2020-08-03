package e2e

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testNodeLogs ensures that all required log files were created, and copies them to the test's artifact directory
// It also tests that 'oc adm node-logs' works with the nodes created by WMCO.
func testNodeLogs(t *testing.T) {
	// All these paths are relative to /var/log/
	logFiles := []string{
		"kube-proxy/kube-proxy.exe.INFO",
		"kube-proxy/kube-proxy.exe.ERROR",
		"kube-proxy/kube-proxy.exe.WARNING",
		"hybrid-overlay/hybrid-overlay.log",
		"kubelet/kubelet.log",
	}

	nodeArtifacts := filepath.Join(os.Getenv("ARTIFACT_DIR"), "nodes")
	for _, node := range gc.nodes {
		nodeDir := filepath.Join(nodeArtifacts, node.Name)
		for _, file := range logFiles {
			// A subtest is useful here to attempt to get all the logs and not bail on the first error
			t.Run(node.Name+"/"+file, func(t *testing.T) {
				// Use oc to read the expected log files, using this instead of API calls as we are supporting compatibility
				// with 'oc adm node-logs', so we should test directly with the oc tool.
				cmd := exec.Command("oc", "adm", "node-logs", "--path="+file, node.Name)
				out, err := cmd.Output()
				require.NoErrorf(t, err, "failed to get file %s from node %s: %s", file, node.Name, out)

				// Save log files to the artifact directory
				splitPath := strings.Split(file, "/")
				require.Greater(t, len(splitPath), 1)
				err = os.MkdirAll(filepath.Join(nodeDir, splitPath[0]), os.ModePerm)
				require.NoError(t, err, "failed to create log directory")
				outputFile := filepath.Join(nodeDir, splitPath[0], filepath.Base(file))
				err = ioutil.WriteFile(outputFile, out, os.ModePerm)
				// This doesn't actually indicate an error, but because the artifacts are important for debugging issues
				// lets make sure we know if there's an issue saving them, in a non-ignorable way
				assert.NoErrorf(t, err, "error writing to %s", outputFile)
			})
		}
	}
}
