package services

import (
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

func TestGetLogRunnerForCmd(t *testing.T) {
	tests := []struct {
		name             string
		commandPath      string
		logfilePath      string
		expectedContains []string
	}{
		{
			name:        "uses default values",
			commandPath: "C:\\k\\kubelet.exe",
			logfilePath: "C:\\var\\log\\kubelet\\kubelet.log",
			expectedContains: []string{
				windows.KubeLogRunnerPath,
				" -log-file=C:\\var\\log\\kubelet\\kubelet.log",
				" -log-file-size=100M",
				" -log-file-age=168h",
				" -flush-interval=5s",
				" C:\\k\\kubelet.exe",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := getLogRunnerForCmd(test.commandPath, test.logfilePath)
			for _, expectedStr := range test.expectedContains {
				assert.Contains(t, got, expectedStr,
					"expected: %s\ngot: %s", expectedStr, got)
			}
		})
	}
}

func TestGetEnvQuantityOrDefault(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		setEnv       bool
		defaultValue string
		expected     string
	}{
		{
			name:         "returns default when env var not set",
			setEnv:       false,
			defaultValue: "100M",
			expected:     "100M",
		},
		{
			name:         "returns env value when set with valid quantity",
			envValue:     "200M",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "200M",
		},
		{
			name:         "returns env value with numeric only quantity",
			envValue:     "1000",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "1000",
		},
		{
			name:         "returns default when env var is empty string",
			envValue:     "",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "100M",
		},
		{
			name:         "returns default when env var is only spaces",
			envValue:     "   ",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "100M",
		},
		{
			name:         "returns default when env var is whitespace mix",
			envValue:     " \t \n ",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "100M",
		},
		{
			name:         "returns env value when trimmed value is valid",
			envValue:     "  500M  ",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "500M",
		},
		{
			name:         "returns default when env var has invalid format",
			envValue:     "invalid",
			setEnv:       true,
			defaultValue: "100M",
			expected:     "100M",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.setEnv {
				t.Setenv("TEST_QUANTITY", test.envValue)
			}

			result := getEnvQuantityOrDefault("TEST_QUANTITY", test.defaultValue)
			assert.Equal(t, test.expected, result,
				"getEnvQuantityOrDefault(key, %q) = %q; want %q", test.defaultValue, result, test.expected)
		})
	}
}

func TestGetEnvDurationOrDefault(t *testing.T) {
	tests := []struct {
		name         string
		envValue     string
		setEnv       bool
		defaultValue string
		expected     string
	}{
		{
			name:         "returns default when env var not set",
			setEnv:       false,
			defaultValue: "5s",
			expected:     "5s",
		},
		{
			name:         "returns env value when set with valid duration",
			envValue:     "10s",
			setEnv:       true,
			defaultValue: "5s",
			expected:     "10s",
		},
		{
			name:         "returns default when env var is empty string",
			envValue:     "",
			setEnv:       true,
			defaultValue: "5s",
			expected:     "5s",
		},
		{
			name:         "returns default when env var is only spaces",
			envValue:     "   ",
			setEnv:       true,
			defaultValue: "5s",
			expected:     "5s",
		},
		{
			name:         "returns env value when trimmed value is valid",
			envValue:     "  15s  ",
			setEnv:       true,
			defaultValue: "5s",
			expected:     "15s",
		},
		{
			name:         "returns default when env var has invalid format",
			envValue:     "invalid",
			setEnv:       true,
			defaultValue: "5s",
			expected:     "5s",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.setEnv {
				t.Setenv("TEST_DURATION", test.envValue)
			}

			result := getEnvDurationOrDefault("TEST_DURATION", test.defaultValue)
			assert.Equal(t, test.expected, result,
				"getEnvDurationOrDefault(key, %q) = %q; want %q", test.defaultValue, result, test.expected)
		})
	}
}
