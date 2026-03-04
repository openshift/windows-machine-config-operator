package services

import (
	"strings"
	"testing"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

func TestGetHostnameCmd(t *testing.T) {
	tests := []struct {
		name         string
		platformType config.PlatformType
		expected     string
	}{
		{
			name:         "any platform",
			platformType: "",
			expected:     "",
		},
		{
			name:         "AWS platform",
			platformType: config.AWSPlatformType,
			expected:     "Invoke-RestMethod -UseBasicParsing -Uri http://169.254.169.254/latest/meta-data/local-hostname",
		},
		{
			name:         "GCP platform",
			platformType: config.GCPPlatformType,
			expected:     "C:\\Temp\\gcp-get-hostname.ps1",
		},
		{
			name:         "VSphere platform",
			platformType: config.VSpherePlatformType,
			expected:     "$output = Invoke-Expression 'ipconfig /all'; $hostNameLine = ($output -split '`n') | Where-Object { $_ -match 'Host Name' }; $dnsSuffixLine = ($output -split '`n') | Where-Object { $_ -match 'Primary Dns Suffix' }; $hostName = ($hostNameLine -split ':')[1].Trim(); $dnsSuffix = ($dnsSuffixLine -split ':')[1].Trim(); if (-not $dnsSuffix) { return $hostName }; $fqdn = $hostName + '.' + $dnsSuffix; return $fqdn",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := getHostnameCmd(test.platformType)
			assert.Equal(t, test.expected, actual)
		})
	}
}

func TestHybridOverlayConfiguration(t *testing.T) {
	tests := []struct {
		name                   string
		apiServerEndpoint      string
		vxlanPort              string
		debug                  bool
		expectedCmdContains    []string
		expectedCmdNotContains []string
	}{
		{
			name:              "Basic configuration with no optional flags",
			apiServerEndpoint: "https://api-server.endpoint.local:6443",
			vxlanPort:         "",
			debug:             false,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.BootstrapCaCertPath,
			},
			expectedCmdNotContains: []string{
				"--hybrid-overlay-vxlan-port",
				"--loglevel 5",
			},
		},
		{
			name:              "Configuration with debug logging enabled",
			apiServerEndpoint: "https://api-server.endpoint.local:6443",
			vxlanPort:         "",
			debug:             true,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.BootstrapCaCertPath,
				"--loglevel 5",
			},
			expectedCmdNotContains: []string{
				"--hybrid-overlay-vxlan-port",
			},
		},
		{
			name:              "Configuration with all optional flags enabled",
			apiServerEndpoint: "https://api-server.endpoint.local:6443",
			vxlanPort:         "4789",
			debug:             true,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.BootstrapCaCertPath,
				"--hybrid-overlay-vxlan-port 4789",
				"--loglevel 5",
			},
			expectedCmdNotContains: []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := hybridOverlayConfiguration(test.apiServerEndpoint, test.vxlanPort, test.debug)

			assert.Equal(t, windows.HybridOverlayServiceName, result.Name,
				"Service name should match expected value")
			assert.False(t, result.Bootstrap,
				"Service should not be a bootstrap service")
			assert.Equal(t, uint(2), result.Priority,
				"Service priority should be 2")
			assert.Equal(t, []string{windows.KubeletServiceName}, result.Dependencies,
				"Service should depend on kubelet service")
			assert.Nil(t, result.PowershellPreScripts,
				"Service should not have PowerShell pre-scripts")

			require.Len(t, result.NodeVariablesInCommand, 1,
				"Service should have exactly one node variable")
			assert.Equal(t, "NODE_NAME", result.NodeVariablesInCommand[0].Name,
				"Node variable name should be NODE_NAME")
			assert.Equal(t, "{.metadata.name}", result.NodeVariablesInCommand[0].NodeObjectJsonPath,
				"Node variable JSON path should match expected format")

			for _, expectedStr := range test.expectedCmdContains {
				assert.Contains(t, result.Command, expectedStr,
					"Command should contain: %s\nActual command: %s",
					expectedStr, result.Command)
			}

			for _, unexpectedStr := range test.expectedCmdNotContains {
				assert.NotContains(t, result.Command, unexpectedStr,
					"Command should not contain: %s\nActual command: %s",
					unexpectedStr, result.Command)
			}
		})
	}
}

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
			commandPath:   windows.KubeletPath,
			logfilePath:   windows.KubeletLog,
			logFileSize:   "",
			logFileAge:    "",
			flushInterval: "",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				"-log-file=" + windows.KubeletLog,
				windows.KubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-log-file-age=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with log file size set",
			commandPath:   windows.KubeProxyPath,
			logfilePath:   windows.KubeProxyLog,
			logFileSize:   "100Mi",
			logFileAge:    "",
			flushInterval: "",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				"-log-file=" + windows.KubeProxyLog,
				"-log-file-size=100Mi",
				windows.KubeProxyPath,
			},
			expectedNotContains: []string{
				"-log-file-age=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with log file age set",
			commandPath:   windows.KubeletPath,
			logfilePath:   windows.KubeletLog,
			logFileSize:   "",
			logFileAge:    "24h",
			flushInterval: "",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				"-log-file=" + windows.KubeletLog,
				"-log-file-age=24h",
				windows.KubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-flush-interval=",
			},
		},
		{
			name:          "command with flush interval set",
			commandPath:   windows.KubeletPath,
			logfilePath:   windows.KubeletLog,
			logFileSize:   "",
			logFileAge:    "",
			flushInterval: "5s",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				"-log-file=" + windows.KubeletLog,
				"-flush-interval=5s",
				windows.KubeletPath,
			},
			expectedNotContains: []string{
				"-log-file-size=",
				"-log-file-age=",
			},
		},
		{
			name:          "command with all optional parameters set",
			commandPath:   windows.KubeletPath,
			logfilePath:   windows.KubeletLog,
			logFileSize:   "50Mi",
			logFileAge:    "48h",
			flushInterval: "10s",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				"-log-file=" + windows.KubeletLog,
				"-log-file-size=50Mi",
				"-log-file-age=48h",
				"-flush-interval=10s",
				windows.KubeletPath,
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

			result := getLogRunnerForCmd(tc.commandPath, tc.logfilePath)

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
			assert.Equal(t, 0, strings.Index(result, windows.KubeLogRunnerPath),
				"KubeLogRunnerPath must be at the start of the command")
			expectedSuffix := " " + tc.commandPath
			assert.True(t, len(result) >= len(expectedSuffix) &&
				result[len(result)-len(expectedSuffix):] == expectedSuffix,
				"Command path must be at the end of the command string.\nActual: %s", result)
		})
	}
}
