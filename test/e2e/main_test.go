package e2e

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	imageClient "github.com/openshift/client-go/image/clientset/versioned/typed/image/v1"
	"github.com/openshift/library-go/pkg/image/imageutil"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
	"github.com/openshift/windows-machine-config-operator/test/e2e/providers"
)

var (
	// numberOfMachineNodes are the number of nodes which should be configured by the Machine Controller
	numberOfMachineNodes int
	// numberOfBYOHNodes are the number of nodes which should be configured by the ConfigMap Controller
	numberOfBYOHNodes int
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
	// numberOfMachineNodes are the number of nodes which should be configured by the Machine Controller
	numberOfMachineNodes int32
	// numberOfBYOHNodes are the number of nodes which should be configured by the ConfigMap Controller
	numberOfBYOHNodes int32
	// machineNodes are the Windows nodes configured by the Machine controller
	machineNodes []core.Node
	// byohNodes are the Windows nodes configured by the ConfigMap controller
	byohNodes []core.Node
	// privateKeyPath is the path of the private key file used to configure the Windows node
	privateKeyPath string
}

// allNodes returns the combined contents of gc.machineNodes and gc.byohNodes
func (gc *globalContext) allNodes() []core.Node {
	return append(gc.machineNodes, gc.byohNodes...)
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
	// toolsImage is the image specified by the  openshift/tools ImageStream, and is the same image used by `oc debug`.
	// This image is available on all OpenShift Clusters, and has SSH pre-installed.
	toolsImage string
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
	toolsImage, err := getOpenShiftToolsImage(oc.Images)
	if err != nil {
		return nil, errors.Wrap(err, "error getting debug image")
	}

	// number of nodes, retry interval and timeout should come from user-input flags
	return &testContext{client: oc, timeout: retry.Timeout, retryInterval: retry.Interval,
		namespace: "openshift-windows-machine-config-operator", CloudProvider: cloudProvider,
		hasCustomVXLAN: hasCustomVXLANPort, workloadNamespace: "wmco-test", toolsImage: toolsImage}, nil
}

// vmUsername returns the name of the user which can be used to log into each Windows instance
func (tc *testContext) vmUsername() string {
	// username will be Administrator on all cloud providers, except Azure where it is "capi"
	if tc.CloudProvider.GetType() == config.AzurePlatformType {
		return "capi"
	} else {
		return "Administrator"
	}
}

// getOpenShiftToolsImage returns a pullable image from the openshift/tools imagestream
func getOpenShiftToolsImage(imageClient imageClient.ImageV1Interface) (string, error) {
	imageStream, err := imageClient.ImageStreams("openshift").Get(context.TODO(), "tools", meta.GetOptions{})
	if err != nil {
		return "", errors.Wrap(err, "unable to get openshift/tools imagestream")
	}
	image, _, _, _, err := imageutil.ResolveRecentPullSpecForTag(imageStream, "latest", false)
	if err != nil {
		return "", errors.Wrap(err, "unable to get latest debug image from imagestream")
	}
	return image, nil
}

func TestMain(m *testing.M) {
	flag.IntVar(&numberOfBYOHNodes, "byoh-node-count", 1,
		"number of nodes to be created for testing the ConfigMap controller. "+
			"Setting this to 0 will result in some tests being skipped")
	flag.IntVar(&numberOfMachineNodes, "machine-node-count", 1,
		"number of nodes to be created for testing the Machine controller."+
			"Setting this to 0 will result in some tests being skipped")
	flag.StringVar(&wmcoPath, "wmco-path", "./build/_output/bin/windows-machine-config-operator",
		"Path to the WMCO binary, used for version validation")
	flag.StringVar(&privateKeyPath, "private-key-path", "",
		"path of the private key file used to configure the Windows node")
	flag.Parse()

	os.Exit(m.Run())
}
