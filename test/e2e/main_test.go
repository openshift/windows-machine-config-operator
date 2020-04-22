package e2e

import (
	"flag"
	"testing"
	"time"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/tracker"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	// numberOfNodes represent the number of nodes to be dealt with in the test suite.
	numberOfNodes int
	// skipNodeDeletion allows the Windows nodes to hang around after the test suite has been run. This skips the deletion
	// test suite.
	skipNodeDeletion bool
	// sshKeyPair is the name of the keypair that we can use to decrypt the Windows node created in AWS cloud
	sshKeyPair string
	// gc is the global context across the test suites.
	gc = globalContext{}
)

// testVM encapsulates a VM created by the test
type testVM struct {
	types.WindowsVM
}

// globalContext holds the information that we want to use across the test suites.
// If you want to move item here make sure that
// 1.) It is needed across test suites
// 2.) You're responsible for checking if the field is stale or not. Any field
//     in this struct is not guaranteed to be latest from the apiserver.
type globalContext struct {
	// numberOfNodes to be used for the test suite.
	numberOfNodes int
	// nodes are the Windows nodes created by the operator
	nodes []v1.Node
	// windowsVMs is used to interact with the Windows VMs created by the test suite
	windowsVMs []testVM
	// skipNodeDeletion allows the Windows nodes to hang around after the test suite has been run.
	skipNodeDeletion bool
	// sshKeyPair is the name of the keypair that we can use to decrypt the Windows node created in AWS cloud
	sshKeyPair string
}

// testContext holds the information related to the individual test suite. This data structure
// should be instantiated by every test suite, so that we can update the test context to be
// passed around to get the information which was created within the test suite. For example,
// if the create test suite creates a Windows Node object, the node object and other related
// information should be easily accessible by other methods within the same test suite.
// Some of the fields we have here can be exposed by via flags to the test suite.
type testContext struct {
	// namespace is the test namespace, we get this from the operator SDK's test framework.
	namespace string
	// osdkTestCtx is the operator sdk framework's test Context
	osdkTestCtx *framework.TestCtx
	// credentials to be used to access the Windows nodes
	credentials []tracker.Credentials
	// kubeclient is the kube client
	kubeclient kubernetes.Interface
	// tracker is a pointer to the tracker ConfigMap created by operator
	tracker *v1.ConfigMap
	// retryInterval to check for existence of resource in kube api server
	retryInterval time.Duration
	// timeout to terminate checking for the existence of resource in kube apiserver
	timeout time.Duration
}

// NewTestContext returns a new test context to be used by every test.
func NewTestContext(t *testing.T) (*testContext, error) {
	fmwkTestContext := framework.NewTestCtx(t)
	namespace, err := fmwkTestContext.GetNamespace()
	if err != nil {
		return nil, errors.Wrap(err, "test context instantiation failed")
	}
	// number of nodes, retry interval and timeout should come from user-input flags
	return &testContext{osdkTestCtx: fmwkTestContext, kubeclient: framework.Global.KubeClient,
		timeout: retry.Timeout, retryInterval: retry.Interval, namespace: namespace}, nil
}

// cleanup cleans up the test context
func (tc *testContext) cleanup() {
	tc.osdkTestCtx.Cleanup()
}

func TestMain(m *testing.M) {
	flag.IntVar(&numberOfNodes, "node-count", 1, "number of nodes to be created for testing")
	flag.BoolVar(&skipNodeDeletion, "skip-node-deletion", false,
		"Option to disable deletion of the VMs")
	// We're using libra as default value to be used in CI
	flag.StringVar(&sshKeyPair, "ssh-key-pair", "libra", "SSH Key Pair to be used for decrypting "+
		"the Windows Node password")
	framework.MainEntry(m)
}
