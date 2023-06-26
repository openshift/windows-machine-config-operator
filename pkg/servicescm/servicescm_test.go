package servicescm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	// The following cases test only the structure of the services ConfigMap.
	// Content validtion is tested below through test methods upon the helpers.
	testCases := []struct {
		name        string
		input       map[string]string
		expectedErr bool
	}{
		{
			name: "all possible keys",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
				envVarsKey:  "{}",
			},
			expectedErr: false,
		},
		{
			name: "null envVars value",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
				envVarsKey:  "null",
			},
			expectedErr: false,
		},
		{
			name: "non-null envVars value",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
				envVarsKey:  "{\"NO_PROXY\":\"localhost;127.0.0.1\"}",
			},
			expectedErr: false,
		},
		{
			name: "only required keys",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
			},
			expectedErr: false,
		},
		{
			name: "optional key plus only 1 of 2 required keys",
			input: map[string]string{
				servicesKey: "[]",
				envVarsKey:  "{}",
			},
			expectedErr: true,
		},
		{
			name:        "no keys",
			input:       map[string]string{},
			expectedErr: true,
		},
		{
			name: "only 1 of the required keys",
			input: map[string]string{
				servicesKey: "[]",
			},
			expectedErr: true,
		},
		{
			name: "correct number but incorrect key",
			input: map[string]string{
				filesKey:   "[]",
				"testKey":  "[]",
				envVarsKey: "{}",
			},
			expectedErr: true,
		},
		{
			name: "too many keys",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
				envVarsKey:  "{}",
				"testKey":   "[]",
			},
			expectedErr: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			cmData, err := Parse(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, cmData.Services)
			assert.NotNil(t, cmData.Files)
			// if any env vars are present, ensure they are not nil when parsed. Otherwise ensure it is nil
			if value, exists := test.input[envVarsKey]; exists {
				envVars := &map[string]string{}
				require.NoError(t, json.Unmarshal([]byte(value), envVars))
				if len(*envVars) > 0 {
					assert.NotEmpty(t, cmData.EnvironmentVars)
					return
				}
			}
			assert.Nil(t, cmData.EnvironmentVars)
		})
	}
}

func TestGenerate(t *testing.T) {
	testServices := []Service{
		{
			Name:    "test-service",
			Command: "test-command test-arg",
			NodeVariablesInCommand: []NodeCmdArg{{
				Name:               "NAME_VAR",
				NodeObjectJsonPath: "{.metadata.name}",
			}},
			PowershellPreScripts: []PowershellPreScript{{
				VariableName: "PS_VAR",
				Path:         "C:\\k\\test-path.ps1",
			}},
			Dependencies: []string{"kubelet"},
			Bootstrap:    false,
			Priority:     0,
		},
	}
	testFiles := []FileInfo{
		{
			Path:     "C:\\k\\test-path.ps1",
			Checksum: "1",
		},
	}

	testCases := []struct {
		name     string
		services []Service
		files    []FileInfo
		envVars  map[string]string
	}{
		{
			name:     "non-empty envVars",
			services: testServices,
			files:    testFiles,
			envVars: map[string]string{
				"HTTP_PROXY": "http://dev:ad2bc205af349589cb2c425daacf7a00@10.0.29.194:3128/",
				"HTTS_PROXY": "http://dev:ad2bc205af349589cb2c425daacf7a00@10.0.29.194:3128/",
				"NO_PROXY":   "localhost",
			},
		},
		{
			name:     "empty envVars",
			services: testServices,
			files:    testFiles,
			envVars:  map[string]string{},
		},
		{
			name:     "nil envVars",
			services: testServices,
			files:    testFiles,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			data, err := NewData(&test.services, &test.files, test.envVars)
			require.NoError(t, err)
			configMap, err := Generate(Name, "testNamespace", data)
			require.NoError(t, err)

			// Ensure that the ConfigMap we generate passes our own validation functions
			parsed, err := Parse(configMap.Data)
			require.NoError(t, err)
			assert.NoError(t, parsed.ValidateExpectedContent(data))
		})
	}
}

func TestGetBootstrapServices(t *testing.T) {
	testCases := []struct {
		name                     string
		input                    []Service
		expectedNumBootstrapSvcs int
	}{
		{
			name:                     "empty services list",
			input:                    []Service{},
			expectedNumBootstrapSvcs: 0,
		},
		{
			name: "only bootstrap services",
			input: []Service{
				{
					Name:      "new-bootstrap-service",
					Bootstrap: true,
					Priority:  0,
				},
				{
					Name:      "new-bootstrap-service-2",
					Bootstrap: true,
					Priority:  1,
				},
			},
			expectedNumBootstrapSvcs: 2,
		},
		{
			name: "only controller services",
			input: []Service{
				{
					Name:      "test-controller-service",
					Bootstrap: false,
					Priority:  1,
				},
				{
					Name:      "test-controller-service-2",
					Bootstrap: false,
					Priority:  2,
				},
			},
			expectedNumBootstrapSvcs: 0,
		},
		{
			name: "unordered mix of bootstrap and controller services",
			input: []Service{
				{
					Name:      "test-controller-service",
					Bootstrap: false,
					Priority:  1,
				},
				{
					Name:      "new-bootstrap-service",
					Bootstrap: true,
					Priority:  0,
				},
				{
					Name:      "test-controller-service-2",
					Bootstrap: false,
					Priority:  2,
				},
			},
			expectedNumBootstrapSvcs: 1,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			cmData, err := NewData(&test.input, &[]FileInfo{}, nil)
			require.NoError(t, err)
			bootstrapSvcs := cmData.GetBootstrapServices()
			assert.Equal(t, test.expectedNumBootstrapSvcs, len(bootstrapSvcs))
		})
	}
}

func TestValidateDependencies(t *testing.T) {
	testCases := []struct {
		name        string
		input       []Service
		expectedErr bool
	}{
		{
			name:        "empty services list",
			input:       []Service{},
			expectedErr: false,
		},
		{
			name: "no dependencies",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:    "test-controller-service",
					Command: "C:\\test-controller-service --variable-arg1=NODE_NAME",
					NodeVariablesInCommand: []NodeCmdArg{
						{
							Name:               "NODE_NAME",
							NodeObjectJsonPath: "metadata.name",
						},
					},
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedErr: false,
		},
		{
			name: "valid dependencies",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{"new-bootstrap-service"},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{"test-controller-service"},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: false,
		},
		{
			name: "bootstrap service requires node variable in command",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg1=NODE_NAME",
					NodeVariablesInCommand: []NodeCmdArg{
						{
							Name:               "NODE_NAME",
							NodeObjectJsonPath: "metadata.name",
						},
					},
					Dependencies: []string{"test-controller-service", "test-controller-service-2"},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedErr: true,
		},
		{
			name: "bootstrap depends on all non-bootstrap services",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{"test-controller-service", "test-controller-service-2"},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: true,
		},
		{
			name: "bootstrap in the middle of non-bootstrap services",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{"test-controller-service"},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{"new-bootstrap-service"},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: true,
		},
		{
			name: "Service depends on itself",
			input: []Service{
				{
					Name:         "test-service",
					Command:      "C:\\test-service",
					Dependencies: []string{"test-service"},
					Bootstrap:    false,
					Priority:     0,
				},
			},
			expectedErr: true,
		},
		{
			name: "Service depends on a service not defined in the services ConfigMap",
			input: []Service{
				{
					Name:         "test-service",
					Command:      "C:\\test-service",
					Dependencies: []string{"external-svc"},
					Bootstrap:    false,
					Priority:     0,
				},
			},
			expectedErr: false,
		},
		{
			name: "Cyclical dependency structure",
			input: []Service{
				{
					Name:         "service-0",
					Command:      "C:\\service-0",
					Dependencies: []string{"service-1"},
					Bootstrap:    false,
					Priority:     0,
				},
				{
					Name:         "service-1",
					Command:      "C:\\service-1",
					Dependencies: []string{"service-2"},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "service-2",
					Command:      "C:\\service-2",
					Dependencies: []string{"service-0"},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: true,
		},
		{
			name: "Disconnected cyclical dependency structure",
			input: []Service{
				{
					Name:         "service-0",
					Command:      "C:\\service-0",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     0,
				},
				{
					Name:         "service-1",
					Command:      "C:\\service-1",
					Dependencies: []string{"service-2"},
					Bootstrap:    false,
					Priority:     1,
				},
				{
					Name:         "service-2",
					Command:      "C:\\service-2",
					Dependencies: []string{"service-1"},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			err := validateDependencies(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidatePriorities(t *testing.T) {
	testCases := []struct {
		name        string
		input       []Service
		expectedErr bool
	}{
		{
			name:        "empty services list",
			input:       []Service{},
			expectedErr: false,
		},
		{
			name: "valid priorities",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{"new-bootstrap-service"},
					Bootstrap:    false,
					Priority:     2,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{"test-controller-service-2"},
					Bootstrap:    false,
					Priority:     5,
				},
			},
			expectedErr: false,
		},
		{
			name: "valid repeated priorities",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "new-bootstrap-service-2",
					Command:      "C:\\tnew-bootstrap-service-2",
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedErr: false,
		},
		{
			name: "overlapping bootstrap and non-bootstrap priorities",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     0,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{"new-bootstrap-service"},
					Bootstrap:    false,
					Priority:     0,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{"test-controller-service-2"},
					Bootstrap:    false,
					Priority:     5,
				},
			},
			expectedErr: true,
		},
		{
			name: "bootstrap lower priority than all non-bootstrap services",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     2,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     0,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{"test-controller-service"},
					Bootstrap:    false,
					Priority:     1,
				},
			},
			expectedErr: true,
		},
		{
			name: "bootstrap in the middle of non-bootstrap services",
			input: []Service{
				{
					Name:    "new-bootstrap-service",
					Command: "C:\\new-service --variable-arg2=NETWORK_IP",
					PowershellPreScripts: []PowershellPreScript{
						{
							VariableName: "NETWORK_IP",
							Path:         "C:\\k\\scripts\\get_net_ip.ps",
						},
					},
					Dependencies: []string{},
					Bootstrap:    true,
					Priority:     1,
				},
				{
					Name:         "test-controller-service",
					Command:      "C:\\test-controller-service",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     0,
				},
				{
					Name:         "test-controller-service-2",
					Command:      "C:\\test-controller-service-2",
					Dependencies: []string{},
					Bootstrap:    false,
					Priority:     2,
				},
			},
			expectedErr: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			err := validatePriorities(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
