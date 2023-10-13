package windows

import (
	"testing"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"

	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
)

func TestGetFilesToTransfer(t *testing.T) {
	testCases := []struct {
		name     string
		platform *config.PlatformType
	}{
		{
			name:     "test AWS",
			platform: func() *config.PlatformType { t := config.AWSPlatformType; return &t }(),
		},
		{
			name:     "test Azure",
			platform: func() *config.PlatformType { t := config.AzurePlatformType; return &t }(),
		},
		{
			name:     "test Nil",
			platform: nil,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			files := getFilesToTransfer(test.platform)
			if test.platform != nil && *test.platform == config.AzurePlatformType {
				file := files[payload.AzureCloudNodeManagerPath]
				assert.Equal(t, K8sDir, file)
			} else {
				_, exists := files[payload.AzureCloudNodeManagerPath]
				assert.False(t, exists)
			}
		})
	}
}

func TestSplitPath(t *testing.T) {
	testCases := []struct {
		name                string
		inputWindowsPath    string
		expectedOutDir      string
		expectedOutFileName string
	}{
		{
			name:                "empty input",
			inputWindowsPath:    "",
			expectedOutDir:      "",
			expectedOutFileName: "",
		},
		{
			name:                "filename only",
			inputWindowsPath:    "test.yml",
			expectedOutDir:      "",
			expectedOutFileName: "test.yml",
		},
		{
			name:                "directory only",
			inputWindowsPath:    "C:\\var\\log\\",
			expectedOutDir:      "C:\\var\\log\\",
			expectedOutFileName: "",
		},
		{
			name:                "full Windows path",
			inputWindowsPath:    "C:\\var\\log\\service.txt",
			expectedOutDir:      "C:\\var\\log\\",
			expectedOutFileName: "service.txt",
		},
		{
			name:                "full linux path",
			inputWindowsPath:    "/home/user/README.md",
			expectedOutDir:      "",
			expectedOutFileName: "/home/user/README.md",
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			dir, fileName := SplitPath(test.inputWindowsPath)
			assert.Equal(t, test.expectedOutDir, dir)
			assert.Equal(t, test.expectedOutFileName, fileName)
		})
	}
}
