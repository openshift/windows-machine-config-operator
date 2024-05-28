package e2e

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	testMirrorSetName               = "e2e-powershell-mirror"
	upstreamPowershellImageLocation = "mcr.microsoft.com/powershell"
	badPowershellImageLocation      = "test.registry.io/powershell"
)

// numBaseRegistryConfigFiles represents the number registry config files that exist before this test suite's steps run
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
	t.Run("Mirror settings cleared from nodes", tc.testMirrorSettingsCleared)
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
			err = wait.PollUntilContextTimeout(context.TODO(), 15*time.Second, 5*time.Minute, true,
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
	err := tc.client.Config.ConfigV1().ImageTagMirrorSets().Delete(context.TODO(), testMirrorSetName, meta.DeleteOptions{})
	require.NoErrorf(t, err, "error deleting mirror set %s", testMirrorSetName)

	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			// retry to give time for registry controller to clear settings from all nodes
			err = wait.PollUntilContextTimeout(context.TODO(), 15*time.Second, 5*time.Minute, true,
				func(ctx context.Context) (done bool, err error) {
					count, err := tc.countItemsInDir(windows.ContainerdConfigDir, addr)
					require.NoErrorf(t, err, "error counting items in dir %s on node %s", windows.ContainerdConfigDir, addr)
					return count == numBaseRegistryConfigFiles, nil
				})
			assert.NoError(t, err, "error waiting for mirror settings to be cleared")
		})
	}
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
