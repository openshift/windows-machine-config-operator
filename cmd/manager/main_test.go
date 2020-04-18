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
		name         string
		version      string
		errorMessage string
	}{
		{"cluster version lower than supported version ", "v1.17.1", "Unsupported server version: v1.17.1. Supported version is v1.18.x"},
		{"cluster version equals supported version", "v1.18.3", ""},
		{"cluster version greater than supported version", "v1.19.0", "Unsupported server version: v1.19.0. Supported version is v1.18.x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// specify version to be tested
			fakeConfigClient.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{
				GitVersion: tt.version,
			}
			clusterconfig := clusterConfig{oclient: fakeConfigClient}
			err := clusterconfig.validateK8sVersion()
			if tt.errorMessage == "" {
				require.Nil(t, err, "Successful check for valid network type")
			} else {
				require.Error(t, err, "Function getK8sVersion did not throw an error "+
					"when it was expected to")
				assert.Contains(t, err.Error(), tt.errorMessage)
			}
		})
	}
}
