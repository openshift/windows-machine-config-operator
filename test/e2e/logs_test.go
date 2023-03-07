package e2e

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

// testNodeLogs ensures that all required log files were created, and copies them to the test's artifact directory
// It also tests that 'oc adm node-logs' works with the nodes created by WMCO.
func testNodeLogs(t *testing.T) {
	// All these paths are relative to /var/log/
	mandatoryLogs := []string{
		"kube-proxy/kube-proxy.exe.INFO",
		"hybrid-overlay/hybrid-overlay.log",
		"kubelet/kubelet.log",
		"containerd/containerd.log",
	}
	optionalLogs := []string{
		"kube-proxy/kube-proxy.exe.ERROR",
		"kube-proxy/kube-proxy.exe.WARNING",
	}

	nodeArtifacts := filepath.Join(os.Getenv("ARTIFACT_DIR"), "nodes")
	for _, node := range gc.allNodes() {
		nodeDir := filepath.Join(nodeArtifacts, node.Name)
		for _, file := range mandatoryLogs {
			// A subtest is useful here to attempt to get all the logs and not bail on the first error
			t.Run(node.Name+"/"+file, func(t *testing.T) {
				err := wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
					err := retrieveLog(node.GetName(), file, nodeDir)
					if err != nil {
						log.Printf("unable to retrieve log %s from node %s: %s", file, node.GetName(), err)
						return false, nil
					}
					logInfo, err := os.Stat(filepath.Join(nodeDir, file))
					if err != nil {
						log.Printf("unable to get info for retrieved log %s from node %s: %s",
							file, node.GetName(), err)
						return false, nil
					}
					if logInfo.Size() == 0 {
						return true, errors.Wrap(err, "log file should not be empty")
					}
					return true, nil
				})
				assert.NoError(t, err)
			})
		}
		// Grab the optional logs for debugging purposes
		for _, file := range optionalLogs {
			// These logs aren't guaranteed to exist, so its better to ignore any error
			_ = wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
				err := retrieveLog(node.GetName(), file, nodeDir)
				if err != nil {
					log.Printf("unable to retrieve log %s from node %s: %s", file, node.GetName(), err)
					return false, nil
				}
				return true, nil
			})
		}
	}
}

// retrieveLog grabs the log specified by the given srcPath from the given node, and writes it to the local destination
// directory
func retrieveLog(nodeName, srcPath, destDir string) error {
	cmd := exec.Command("oc", "adm", "node-logs", "--path="+srcPath, nodeName)
	out, err := cmd.Output()
	if err != nil {
		var exitError *exec.ExitError
		stderr := ""
		if errors.As(err, exitError) {
			stderr = string(exitError.Stderr)
		}
		return fmt.Errorf("oc adm node-logs failed with exit code %s and output: %s: %s", err, string(out), stderr)
	}
	// Save log files to the artifact directory
	splitPath := strings.Split(srcPath, "/")
	if len(splitPath) < 2 {
		return fmt.Errorf("unexpected format for path %s", srcPath)
	}
	err = os.MkdirAll(filepath.Join(destDir, splitPath[0]), os.ModePerm)
	if err != nil {
		return errors.Wrap(err, "failed to create log directory")
	}
	outputFile := filepath.Join(destDir, splitPath[0], filepath.Base(srcPath))
	return ioutil.WriteFile(outputFile, out, os.ModePerm)
}
