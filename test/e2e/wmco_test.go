package e2e

import (
	"testing"
	"time"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
)

var (
	nodeCreationTime     = time.Minute * 20
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
	gc.skipNodeDeletion = skipNodeDeletion
	gc.sshKeyPair = sshKeyPair

	t.Run("create", creationTestSuite)
	if !gc.skipNodeDeletion {
		t.Run("destroy", deletionTestSuite)
	}
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
