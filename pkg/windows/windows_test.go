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

func TestGetLogFileSizeMB(t *testing.T) {
	tests := []struct {
		name        string
		logFileSize string
		expected    uint64
		expectError bool
	}{
		{
			name:        "empty string returns error",
			logFileSize: "",
			expected:    0,
			expectError: true,
		},
		{
			name:        "value in megabytes returns correct value",
			logFileSize: "1M",
			expected:    1,
			expectError: false,
		},
		{
			name:        "value in mebibytes returns correct megabyte equivalent rounded up",
			logFileSize: "100Mi",
			expected:    105,
			expectError: false,
		},
		{
			name:        "value in gigabytes returns correct megabyte equivalent",
			logFileSize: "1G",
			expected:    1000,
			expectError: false,
		},
		{
			name:        "value in gibibytes returns correct megabyte equivalent rounded up",
			logFileSize: "1Gi",
			expected:    1074,
			expectError: false,
		},
		{
			name:        "zero value returns zero",
			logFileSize: "0",
			expected:    0,
			expectError: false,
		},
		{
			name:        "invalid quantity returns error",
			logFileSize: "notaquantity",
			expected:    0,
			expectError: true,
		},
		{
			name:        "plain integer bytes rounds up to one megabyte",
			logFileSize: "500",
			expected:    1,
			expectError: false,
		},
		{
			name:        "value in kilobytes below one megabyte rounds up to one megabyte",
			logFileSize: "500k",
			expected:    1,
			expectError: false,
		},
		{
			name:        "value in kilobytes above one megabyte returns correct value",
			logFileSize: "2000k",
			expected:    2,
			expectError: false,
		},
		{
			name:        "extremely large value in exabytes is valid and returns large megabyte value",
			logFileSize: "1E",
			expected:    1000000000000,
			expectError: false,
		},
		{
			name:        "negative value returns error",
			logFileSize: "-1M",
			expected:    0,
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := getLogFileSizeMB(tc.logFileSize)
			if tc.expectError {
				assert.Error(t, err)
				assert.Equal(t, tc.expected, result)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}
