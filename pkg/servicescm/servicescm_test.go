package servicescm

import (
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
			name: "both expected keys",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
			},
			expectedErr: false,
		},
		{
			name:        "no keys",
			input:       map[string]string{},
			expectedErr: true,
		},
		{
			name: "only 1 of the expected keys",
			input: map[string]string{
				servicesKey: "[]",
			},
			expectedErr: true,
		},
		{
			name: "correct number but incorrect key",
			input: map[string]string{
				filesKey:  "[]",
				"testKey": "[]",
			},
			expectedErr: true,
		},
		{
			name: "too many keys",
			input: map[string]string{
				servicesKey: "[]",
				filesKey:    "[]",
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
		})
	}
}

func TestGenerate(t *testing.T) {
	// Ensure that the ConfigMap we generate internally as the source of truth passes our own validation functions
	configMap, err := Generate(Name, "testNamespace")
	require.NoError(t, err)

	data, err := Parse(configMap.Data)
	require.NoError(t, err)
	assert.NoError(t, data.ValidateRequiredContent())
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
			cmData, err := newData(&test.input, &[]FileInfo{})
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
					PowershellVariablesInCommand: []PowershellCmdArg{
						{
							Name: "NETWORK_IP",
							Path: "C:\\k\\scripts\\get_net_ip.ps",
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
