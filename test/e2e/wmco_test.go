package e2e

import (
	"testing"
	"time"

	"github.com/openshift/windows-machine-config-operator/pkg/apis"
	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
)

var (
	retryInterval        = time.Second * 5
	timeout              = time.Minute * 60
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 5
)

// TestWMCO sets up the testing suite for WMCO.
func TestWMCO(t *testing.T) {
	if err := setupWMCOResources(); err != nil {
		t.Fatalf("%v", err)
	}
	// TODO: In future, we'd like to skip the teardown for each test. As of now, since we just have deletion it should
	// 		be ok to call destroy directly.
	//		Jira Story: https://issues.redhat.com/browse/WINC-283
	t.Run("WMC CR validation", testWMCValidation)
	t.Run("create", creationTestSuite)
	t.Run("destroy", deletionTestSuite)
}

// setupWMCO setups the resources needed to run WMCO tests
func setupWMCOResources() error {
	wmcoList := &operator.WindowsMachineConfigList{}
	err := framework.AddToFrameworkScheme(apis.AddToScheme, wmcoList)
	if err != nil {
		return errors.Wrap(err, "failed setting up test suite")
	}
	return nil
}
