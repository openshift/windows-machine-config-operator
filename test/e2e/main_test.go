package e2e

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers"
)

var (
	// numberOfNodes represent the number of nodes to be dealt with in the test suite.
	numberOfNodes int
	// privateKeyPath is the path of the private key file used to configure the Windows node
	privateKeyPath string
	// wmcoPath is the path to the WMCO binary that was used within the operator image
	wmcoPath string
	// gc is the global context across the test suites.
	gc = globalContext{}
)

// globalContext holds the information that we want to use across the test suites.
// If you want to move item here make sure that
// 1.) It is needed across test suites
// 2.) You're responsible for checking if the field is stale or not. Any field
//     in this struct is not guaranteed to be latest from the apiserver.
type globalContext struct {
	// numberOfNodes to be used for the test suite.
	numberOfNodes int32
	// nodes are the Windows nodes created by the operator
	nodes []core.Node
	// privateKeyPath is the path of the private key file used to configure the Windows node
	privateKeyPath string
}

// testContext holds the information related to the individual test suite. This data structure
// should be instantiated by every test suite, so that we can update the test context to be
// passed around to get the information which was created within the test suite. For example,
// if the create test suite creates a Windows Node object, the node object and other related
// information should be easily accessible by other methods within the same test suite.
// Some of the fields we have here can be exposed by via flags to the test suite.
type testContext struct {
	// namespace is the namespace the operator is deployed in
	namespace string
	// client is the OpenShift client
	client *clusterinfo.OpenShift
	// retryInterval to check for existence of resource in kube api server
	retryInterval time.Duration
	// timeout to terminate checking for the existence of resource in kube apiserver
	timeout time.Duration
	// CloudProvider to talk to various cloud providers
	providers.CloudProvider
	// hasCustomVXLAN tells if the cluster is using a custom VXLAN port for communication
	hasCustomVXLAN bool
	// workloadNamespace is the namespace to deploy our test pods on
	workloadNamespace string
}

// NewTestContext returns a new test context to be used by every test.
func NewTestContext() (*testContext, error) {
	oc, err := clusterinfo.GetOpenShift()
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialize OpenShift client")
	}
	hasCustomVXLANPort, err := oc.HasCustomVXLANPort()
	if err != nil {
		return nil, errors.Wrap(err, "failed to determine if cluster is using custom VXLAN port")
	}
	cloudProvider, err := providers.NewCloudProvider(hasCustomVXLANPort)
	if err != nil {
		return nil, errors.Wrap(err, "cloud provider creation failed")
	}
	// number of nodes, retry interval and timeout should come from user-input flags
	return &testContext{client: oc, timeout: retry.Timeout, retryInterval: retry.Interval,
		namespace: "openshift-windows-machine-config-operator", CloudProvider: cloudProvider,
		hasCustomVXLAN: hasCustomVXLANPort, workloadNamespace: "wmco-test"}, nil
}

func TestMain(m *testing.M) {
	flag.IntVar(&numberOfNodes, "node-count", 2, "number of nodes to be created for testing")
	flag.StringVar(&wmcoPath, "wmco-path", "./build/_output/bin/windows-machine-config-operator",
		"Path to the WMCO binary, used for version validation")
	flag.StringVar(&privateKeyPath, "private-key-path", "",
		"path of the private key file used to configure the Windows node")
	flag.Parse()

	os.Exit(m.Run())
}
