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
		vxlanPort              string
		debug                  bool
		expectedCmdContains    []string
		expectedCmdNotContains []string
	}{
		{
			name:      "Basic configuration with no optional flags",
			vxlanPort: "",
			debug:     false,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.TrustedCABundlePath,
			},
			expectedCmdNotContains: []string{
				"--hybrid-overlay-vxlan-port",
				"--loglevel 5",
			},
		},
		{
			name:      "Configuration with debug logging enabled",
			vxlanPort: "",
			debug:     true,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.TrustedCABundlePath,
				"--loglevel 5",
			},
			expectedCmdNotContains: []string{
				"--hybrid-overlay-vxlan-port",
			},
		},
		{
			name:      "Configuration with all optional flags enabled",
			vxlanPort: "4789",
			debug:     true,
			expectedCmdContains: []string{
				windows.HybridOverlayPath,
				"--node NODE_NAME",
				"--bootstrap-kubeconfig=" + windows.KubeconfigPath,
				"--cert-dir=" + windows.CniConfDir,
				"--cert-duration=24h",
				"--windows-service",
				"--logfile",
				windows.HybridOverlayLogDir + "\\hybrid-overlay.log",
				"--k8s-cacert " + windows.TrustedCABundlePath,
				"--hybrid-overlay-vxlan-port 4789",
				"--loglevel 5",
			},
			expectedCmdNotContains: []string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := hybridOverlayConfiguration(test.vxlanPort, test.debug)

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
