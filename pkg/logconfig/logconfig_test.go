package logconfig

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testKubeLogRunnerPath = "C:\\k\\kube-log-runner.exe"
	testKubeletPath       = "C:\\k\\kubelet.exe"
	testKubeletLog        = "C:\\var\\log\\kubelet\\kubelet.log"
	testKubeProxyPath     = "C:\\k\\kube-proxy.exe"
	testKubeProxyLog      = "C:\\var\\log\\kube-proxy\\kube-proxy.log"
)

func TestGetEnvDuration(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		set         bool
		expected    string
		expectError bool
	}{
		{
			name:        "unset environment variable returns empty string",
			set:         false,
			expected:    "",
			expectError: false,
		},
		{
			name:        "empty string returns empty string",
			envValue:    "",
			set:         true,
			expected:    "",
			expectError: false,
		},
		{
			name:        "whitespace-only string returns empty string",
			envValue:    "   ",
			set:         true,
			expected:    "",
			expectError: false,
		},
		{
			name:        "valid duration in seconds",
			envValue:    "5s",
			set:         true,
			expected:    "5s",
			expectError: false,
		},
		{
			name:        "valid duration with leading/trailing whitespace is trimmed",
			envValue:    "  30s  ",
			set:         true,
			expected:    "30s",
			expectError: false,
		},
		{
			name:        "zero duration is valid",
			envValue:    "0s",
			set:         true,
			expected:    "0s",
			expectError: false,
		},
		{
			name:        "invalid duration string returns error",
			envValue:    "notaduration",
			set:         true,
			expected:    "",
			expectError: true,
		},
		{
			name:        "number without unit returns error",
			envValue:    "100",
			set:         true,
			expected:    "",
			expectError: true,
		},
		{
			name:        "negative duration returns error",
			envValue:    "-5s",
			set:         true,
			expected:    "",
			expectError: true,
		},
		{
			name:        "duration with invalid unit returns error",
			envValue:    "10x",
			set:         true,
			expected:    "",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TEST_DURATION_VAR", tc.envValue)
			}
			result, err := getEnvDurationString("TEST_DURATION_VAR")
			if tc.expectError {
				assert.Error(t, err)
				assert.Empty(t, result)
				assert.Contains(t, err.Error(), "TEST_DURATION_VAR")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestGetEnvQuantity(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		set         bool
		expected    string
		expectError bool
	}{
		{
			name:        "unset environment variable returns empty string",
			set:         false,
			expected:    "",
			expectError: false,
		},
		{
			name:        "empty string returns empty string",
			envValue:    "",
			set:         true,
			expected:    "",
			expectError: false,
		},
		{
			name:        "whitespace-only string returns empty string",
			envValue:    "   ",
			set:         true,
			expected:    "",
			expectError: false,
		},
		{
			name:        "valid integer quantity",
			envValue:    "100",
			set:         true,
			expected:    "100",
			expectError: false,
		},
		{
			name:        "valid quantity with suffix",
			envValue:    "100M",
			set:         true,
			expected:    "100M",
			expectError: false,
		},
		{
			name:        "valid quantity with leading/trailing whitespace is trimmed",
			envValue:    "  100Mi  ",
			set:         true,
			expected:    "100Mi",
			expectError: false,
		},
		{
			name:        "zero quantity is valid",
			envValue:    "0",
			set:         true,
			expected:    "0",
			expectError: false,
		},
		{
			name:        "negative quantity returns error",
			envValue:    "-1",
			set:         true,
			expected:    "",
			expectError: true,
		},
		{
			name:        "invalid quantity returns error",
			envValue:    "notaquantity",
			set:         true,
			expected:    "",
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv("TEST_QUANTITY_VAR", tc.envValue)
			}
			result, err := getEnvQuantityString("TEST_QUANTITY_VAR")
			if tc.expectError {
				assert.Error(t, err)
				assert.Empty(t, result)
				assert.Contains(t, err.Error(), "TEST_QUANTITY_VAR")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestGetLogRunnerForCmd(t *testing.T) {
	origLogFileSize := logFileSize
	origLogFileAge := logFileAge
	origFlushInterval := flushInterval
	t.Cleanup(func() {
		logFileSize = origLogFileSize
		logFileAge = origLogFileAge
		flushInterval = origFlushInterval
	})

	tests := []struct {
		name                string
		commandPath         string
		logfilePath         string
		logFileSize         string
		logFileAge          string
		flushInterval       string
		expectedContains    []string
		expectedNotContains []string
	}{
		{
			name:          "basic command with no optional parameters",
			commandPath:   testKubeletPath,
			logfilePath:   testKubeletLog,
			logFileSize:   "",
			logFileAge:    "",
			flushInterval: "",
			expectedContains: []string{
				testKubeLogRunnerPath,
				"-log-file=" + testKubeletLog,
				testKubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-log-file-age=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with log file size set",
			commandPath:   testKubeProxyPath,
			logfilePath:   testKubeProxyLog,
			logFileSize:   "100Mi",
			logFileAge:    "",
			flushInterval: "",
			expectedContains: []string{
				testKubeLogRunnerPath,
				"-log-file=" + testKubeProxyLog,
				"-log-file-size=100Mi",
				testKubeProxyPath,
			},
			expectedNotContains: []string{
				"-log-file-age=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with log file age set",
			commandPath:   testKubeletPath,
			logfilePath:   testKubeletLog,
			logFileSize:   "",
			logFileAge:    "24h",
			flushInterval: "",
			expectedContains: []string{
				testKubeLogRunnerPath,
				"-log-file=" + testKubeletLog,
				"-log-file-age=24h",
				testKubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with flush interval set",
			commandPath:   testKubeletPath,
			logfilePath:   testKubeletLog,
			logFileSize:   "",
			logFileAge:    "",
			flushInterval: "5s",
			expectedContains: []string{
				testKubeLogRunnerPath,
				"-log-file=" + testKubeletLog,
				"-flush-interval=5s",
				testKubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-log-file-age=",
			},
		},
		{
			name:          "command with all optional parameters set",
			commandPath:   testKubeletPath,
			logfilePath:   testKubeletLog,
			logFileSize:   "50Mi",
			logFileAge:    "48h",
			flushInterval: "10s",
			expectedContains: []string{
				testKubeLogRunnerPath,
				"-log-file=" + testKubeletLog,
				"-log-file-size=50Mi",
				"-log-file-age=48h",
				"-flush-interval=10s",
				testKubeletPath,
			},
			expectedNotContains: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Set package-level variables for this test case
			logFileSize = tc.logFileSize
			logFileAge = tc.logFileAge
			flushInterval = tc.flushInterval

			result := GenerateKubeLogRunnerCmd(testKubeLogRunnerPath, tc.commandPath, tc.logfilePath)

			for _, expected := range tc.expectedContains {
				assert.Contains(t, result, expected,
					"Command should contain: %s\nActual command: %s", expected, result)
			}
			for _, notExpected := range tc.expectedNotContains {
				assert.NotContains(t, result, notExpected,
					"Command should not contain: %s\nActual command: %s", notExpected, result)
			}

			// Verify ordering: KubeLogRunnerPath must come first, commandPath must come last
			assert.True(t, len(result) > 0, "Result should not be empty")
			assert.Equal(t, 0, strings.Index(result, testKubeLogRunnerPath),
				"KubeLogRunnerPath must be at the start of the command")
			expectedSuffix := " " + tc.commandPath
			assert.True(t, len(result) >= len(expectedSuffix) &&
				result[len(result)-len(expectedSuffix):] == expectedSuffix,
				"Command path must be at the end of the command string.\nActual: %s", result)
		})
	}
}
