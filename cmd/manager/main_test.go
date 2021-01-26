package main

import (
	"testing"

	fakeconfigclient "github.com/openshift/client-go/config/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
)

// TestCheckIfRequiredFilesExist tests if checkIfRequiredFilesExist function is throwing appropriate error when some
// of the files required by WMCO are missing
func TestCheckIfRequiredFilesExist(t *testing.T) {
	// required files of WMCO that are missing
	var missingRequiredFiles = []string{
		"/payload/file-1",
		"/payload/file-2",
	}
	err := checkIfRequiredFilesExist(missingRequiredFiles)
	require.Error(t, err, "Function checkIfRequiredFilesExist did not throw an error when it was expected to")
	assert.Contains(t, err.Error(), "could not stat /payload/file-1: stat /payload/file-1: no such file or directory",
		"Expected error message is absent")
	assert.Contains(t, err.Error(), "could not stat /payload/file-2: stat /payload/file-2: no such file or directory",
		"Expected error message is absent")

}

// TestIsValidKubernetesVersion tests if validateK8sVersion function throws error if K8s version is not a supported k8s version
func TestIsValidKubernetesVersion(t *testing.T) {
	fakeConfigClient := fakeconfigclient.NewSimpleClientset()
	var tests = []struct {
		name    string
		version string
		error   bool
	}{
		{"cluster version lower than supported version ", "v1.17.1", true},
		{"cluster version equals supported version", "v1.19.0", false},
		{"cluster version equals supported version", "v1.20.4", false},
		{"cluster version greater than supported version ", "v1.22.2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// specify version to be tested
			fakeConfigClient.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{
				GitVersion: tt.version,
			}
			clusterconfig := clusterConfig{oclient: fakeConfigClient}
			err := clusterconfig.validateK8sVersion()
			if tt.error {
				require.Error(t, err, "Function getK8sVersion did not throw an error "+
					"when it was expected to")
			} else {
				require.Nil(t, err, "Successful check for valid network type")
			}
		})
	}
}
