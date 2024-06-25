package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	testMirrorSetName               = "e2e-powershell-mirror"
	upstreamPowershellImageLocation = "mcr.microsoft.com/powershell"
	badPowershellImageLocation      = "test.registry.io/powershell"
)

// numBaseRegistryConfigFiles represents the number of registry config files that exist before this test suite's steps run
var numBaseRegistryConfigFiles int

func testImageMirroring(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	require.NoError(t, tc.loadExistingNodes(), "error getting the current Windows nodes in the cluster")

	require.NoError(t, tc.setNumBaseRegistryConfigFiles(), "error getting base number of registry config files")

	if !t.Run("Mirror settings applied to nodes", tc.testMirrorSettingsApplied) {
		// No point in running the rest of the tests if settings aren't applied
		return
	}
	t.Run("Workload using a mirrored container image", tc.testWorkloadWithMirroredImage)
}

// setNumBaseRegistryConfigFiles updates the numBaseRegistryConfigFiles variable
func (tc *testContext) setNumBaseRegistryConfigFiles() error {
	winNodes := gc.allNodes()
	if len(winNodes) == 0 {
		return fmt.Errorf("test requires at least one Windows node to run")
	}
	addr, err := controllers.GetAddress(winNodes[0].Status.Addresses)
	if err != nil {
		return fmt.Errorf("unable to get node address: %w", err)
	}
	numBaseRegistryConfigFiles, err = tc.countItemsInDir(windows.ContainerdConfigDir, addr)
	if err != nil {
		return fmt.Errorf("error counting items in dir %s on node %s: %w", windows.ContainerdConfigDir, addr, err)
	}
	return nil
}

// testMirrorSettingsApplied tests that the containerd registry configuration directory is populated
func (tc *testContext) testMirrorSettingsApplied(t *testing.T) {
	// enables this test suite to run independently of the creation tests
	itms, err := tc.ensureMirrorSetExists()
	require.NoErrorf(t, err, "error ensuring mirror set %s exists", testMirrorSetName)
	// expected one config file per source entry
	expectedNumConfigFiles := len(itms.Spec.ImageTagMirrors) + numBaseRegistryConfigFiles

	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			// retry to give time for registry controller to transfer generated config to all nodes
			err = wait.PollUntilContextTimeout(context.TODO(), retry.Interval, 5*time.Minute, true,
				func(ctx context.Context) (done bool, err error) {
					count, err := tc.countItemsInDir(windows.ContainerdConfigDir, addr)
					require.NoErrorf(t, err, "error counting items in dir %s on node %s", windows.ContainerdConfigDir, addr)
					return count == expectedNumConfigFiles, nil
				})
			assert.NoError(t, err, "error waiting for mirror settings to be applied")
		})
	}
}

// testMirrorSettingsCleared tests that the containerd registry config directory does not contain any config settings
func (tc *testContext) testMirrorSettingsCleared(t *testing.T) {
	require.NoError(t, tc.safeDeleteMirrorSet())

	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			// retry to give time for registry controller to clear settings from all nodes
			err = wait.PollUntilContextTimeout(context.TODO(), retry.Interval, 5*time.Minute, true,
				func(ctx context.Context) (done bool, err error) {
					count, err := tc.countItemsInDir(windows.ContainerdConfigDir, addr)
					require.NoErrorf(t, err, "error counting items in dir %s on node %s", windows.ContainerdConfigDir, addr)
					return count == numBaseRegistryConfigFiles, nil
				})
			assert.NoError(t, err, "error waiting for mirror settings to be cleared")
		})
	}
}

// safeDeleteMirrorSet deletes the test suite's ITMS resource if it exists, and waits for deletion to complete
func (tc *testContext) safeDeleteMirrorSet() error {
	// Check if mirror set exists, do nothing if it already doesn't exist
	_, err := tc.client.Config.ConfigV1().ImageTagMirrorSets().Get(context.TODO(), testMirrorSetName, meta.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	err = tc.client.Config.ConfigV1().ImageTagMirrorSets().Delete(context.TODO(), testMirrorSetName, meta.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("error deleting %s: %w", testMirrorSetName, err)
	}
	return tc.waitForMirrorSetDeletion()
}

// waitForMirrorSetDeletion waits for a test ITMS to deleted. Returns an error if it is still present at the time limit.
func (tc *testContext) waitForMirrorSetDeletion() error {
	err := wait.PollUntilContextTimeout(context.TODO(), retry.Interval, retry.ResourceChangeTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := tc.client.Config.ConfigV1().ImageTagMirrorSets().Get(context.TODO(), testMirrorSetName, meta.GetOptions{})
			if err == nil {
				// Retry if the resource is found
				return false, nil
			}
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("error retrieving ITMS: %s: %w", testMirrorSetName, err)
		})
	if err != nil {
		return fmt.Errorf("error waiting for ITMS deletion %s: %w", testMirrorSetName, err)
	}
	return nil
}

// countItemsInDir runs a job on the given instance address to count the number of items in the given directory
func (tc *testContext) countItemsInDir(remoteDir, address string) (int, error) {
	countDirsCommand := fmt.Sprintf("(Get-ChildItem -Path %s).Count", remoteDir)
	out, err := tc.runPowerShellSSHJob("check-registry-config-files", countDirsCommand, address)
	if err != nil {
		return 0, fmt.Errorf("error running SSH job: %w", err)
	}
	// Final line should contain a single number representing the number of dirs and files found in the remote dir
	count, err := strconv.Atoi(finalLine(out))
	if err != nil {
		return 0, fmt.Errorf("job result is not an integer: %w", err)
	}
	return count, nil
}

// ensureMirrorSetExists creates the test mirror set if it does not exist
func (tc *testContext) ensureMirrorSetExists() (*config.ImageTagMirrorSet, error) {
	itms, err := tc.client.Config.ConfigV1().ImageTagMirrorSets().Get(context.TODO(), testMirrorSetName, meta.GetOptions{})
	if err == nil {
		return itms, nil
	}
	if errors.IsNotFound(err) {
		return tc.createMirrorSet()
	}
	return nil, fmt.Errorf("error creating mirror set: %w", err)
}

// createMirrorSet creates a cluster resource to mirror an image reference from an invalid source to the real location
func (tc *testContext) createMirrorSet() (*config.ImageTagMirrorSet, error) {
	itms := &config.ImageTagMirrorSet{
		ObjectMeta: meta.ObjectMeta{
			Name: testMirrorSetName,
		},
		Spec: config.ImageTagMirrorSetSpec{
			ImageTagMirrors: []config.ImageTagMirrors{{
				Source:             badPowershellImageLocation,
				Mirrors:            []config.ImageMirror{upstreamPowershellImageLocation},
				MirrorSourcePolicy: config.AllowContactingSource,
			}},
		},
	}
	// Create the ImageTagMirrorSet in the cluster
	return tc.client.Config.ConfigV1().ImageTagMirrorSets().Create(context.TODO(), itms, meta.CreateOptions{})
}

// testWorkloadWithMirroredImage runs a job using an image from a mirror registry URL
func (tc *testContext) testWorkloadWithMirroredImage(t *testing.T) {
	if tc.mirrorRegistry == "" {
		t.Skip("test disabled, container mirror registry is exclusively setup for the disconnected job")
	}
	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			nodeAffinity, err := getAffinityForNode(&node)
			require.NoError(t, err, "could not get node affinity")
			mirrorImage := tc.mirrorRegistry + "/" + strings.Split(tc.getWindowsServerContainerImage(), "/")[1]
			winJob, err := tc.createWindowsServerJobWithMirrorImage("test-powershell-mirror", mirrorImage,
				"Get-Help", nodeAffinity)
			require.NoError(t, err, "could not create Windows test job")
			defer tc.deleteJob(winJob.Name)

			_, err = tc.waitUntilJobSucceeds(winJob.Name)
			assert.NoError(t, err, "Windows test job failed")
		})
	}
}

// createWindowsServerJobWithMirrorImage creates a job to run the given PowerShell command with the specified container image
func (tc *testContext) createWindowsServerJobWithMirrorImage(name, containerImage, pwshCommand string,
	affinity *core.Affinity) (*batch.Job, error) {
	rcName, err := tc.getRuntimeClassName()
	if err != nil {
		return nil, err
	}
	windowsOS := &core.PodOS{Name: core.Windows}
	command := []string{powerShellExe, "-command", pwshCommand}
	pullSecret := []core.LocalObjectReference{{
		Name: "pull-secret",
	}}
	return tc.createJob(name, containerImage, command, &rcName, affinity, windowsOS, pullSecret)
}
