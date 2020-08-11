package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	nodeCreationTime     = time.Minute * 30
	nodeRetryInterval    = time.Minute * 1
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 5
	// deploymentRetries is the amount of time to retry creating a Windows Server deployment, to compensate for the
	// time it takes to download the Server2019 image to the node
	deploymentRetries = 10
)

// TestWMCO sets up the testing suite for WMCO.
func TestWMCO(t *testing.T) {
	if err := setupWMCOResources(); err != nil {
		t.Fatalf("%v", err)
	}

	// We've to update the global context struct here as the operator-sdk's framework has coupled flag
	// parsing along with test suite execution.
	// Reference:
	// https://github.com/operator-framework/operator-sdk/blob/b448429687fd7cb2343d022814ed70c9d264612b/pkg/test/main_entry.go#L51
	gc.numberOfNodes = int32(numberOfNodes)
	require.NotEmpty(t, privateKeyPath, "private-key-path is not set")
	gc.privateKeyPath = privateKeyPath
	// When the OPENSHIFT_CI env var is set to true, the test is running within CI
	if inCI := os.Getenv("OPENSHIFT_CI"); inCI == "true" {
		// In the CI container the WMCO binary will be found here
		wmcoPath = "/usr/local/bin/windows-machine-config-operator"
	}

	// test that the operator can deploy without the secret already created, we can later use a secret created by the
	// individual test suites after the operator is running
	t.Run("operator deployed without private key secret", testOperatorDeployed)
	t.Run("create", creationTestSuite)
	t.Run("upgrade", upgradeTestSuite)
	t.Run("destroy", deletionTestSuite)
}

// setupWMCO setups the resources needed to run WMCO tests
func setupWMCOResources() error {
	// Register the Machine API to create machine objects from framework's client
	err := framework.AddToFrameworkScheme(mapi.AddToScheme, &mapi.MachineSetList{})
	if err != nil {
		return errors.Wrap(err, "failed adding machine api scheme")
	}
	return nil
}

// testOperatorDeployed tests that the operator pod is running
func testOperatorDeployed(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	deployment, err := testCtx.kubeclient.AppsV1().Deployments(testCtx.namespace).Get(context.TODO(),
		"windows-machine-config-operator", meta.GetOptions{})
	require.NoError(t, err, "could not get WMCO deployment")
	require.NotZerof(t, deployment.Status.AvailableReplicas, "WMCO deployment has no available replicas: %v", deployment)
}
