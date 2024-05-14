package registries

import (
	"testing"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
)

func TestGenerateConfig(t *testing.T) {
	testCases := []struct {
		name           string
		input          mirrorSet
		expectedOutput string
	}{
		{
			name: "basic one digest mirror",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", false},
				},
				mirrorSourcePolicy: config.AllowContactingSource,
			},
			expectedOutput: "server = \"https://registry.access.redhat.com/ubi9/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\"]\r\n",
		},
		{
			name: "basic one tag mirror",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", true},
				},
				mirrorSourcePolicy: config.AllowContactingSource,
			},
			expectedOutput: "server = \"https://registry.access.redhat.com/ubi9/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\", \"resolve\"]\r\n",
		},
		{
			name: "one digest mirror never contact source",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", false},
				},
				mirrorSourcePolicy: config.NeverContactSource,
			},
			expectedOutput: "server = \"https://example.io/example/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\"]\r\n",
		},
		{
			name: "tags mirror never contact source",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", true},
				},
				mirrorSourcePolicy: config.NeverContactSource,
			},
			expectedOutput: "server = \"https://example.io/example/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\", \"resolve\"]\r\n",
		},
		{
			name: "multiple mirrors",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", false},
					{"mirror.example.com/redhat", false},
					{"mirror.example.net/image", true},
				},
				mirrorSourcePolicy: config.AllowContactingSource,
			},
			expectedOutput: "server = \"https://registry.access.redhat.com/ubi9/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\"]\r\n[host.\"https://mirror.example.com/redhat\"]\r\n  capabilities = [\"pull\"]\r\n[host.\"https://mirror.example.net/image\"]\r\n  capabilities = [\"pull\", \"resolve\"]\r\n",
		},
		{
			name: "multiple mirrors never contact source",
			input: mirrorSet{
				source: "registry.access.redhat.com/ubi9/ubi-minimal",
				mirrors: []mirror{
					{"example.io/example/ubi-minimal", false},
					{"mirror.example.com/redhat", false},
					{"mirror.example.net/image", true},
				},
				mirrorSourcePolicy: config.NeverContactSource,
			},
			expectedOutput: "server = \"https://example.io/example/ubi-minimal\"\r\n\r\n[host.\"https://example.io/example/ubi-minimal\"]\r\n  capabilities = [\"pull\"]\r\n[host.\"https://mirror.example.com/redhat\"]\r\n  capabilities = [\"pull\"]\r\n[host.\"https://mirror.example.net/image\"]\r\n  capabilities = [\"pull\", \"resolve\"]\r\n",
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := test.input.generateConfig()
			assert.Equal(t, out, test.expectedOutput)
		})
	}
}

func TestMergeMirrorSets(t *testing.T) {
	testCases := []struct {
		name  string
		input []mirrorSet
		// expectedOutput's sources and mirror orders matter since result is expected to be sorted alphabetically
		expectedOutput []mirrorSet
	}{
		{
			name: "same source but different mirrors",
			input: []mirrorSet{
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"example.io/example/ubi-minimal", false},
						{"example.com/example/ubi-minimal", true},
					},
				},
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.net/image", false},
						{"mirror.example.com/redhat", true},
					},
				},
			},
			expectedOutput: []mirrorSet{
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"example.com/example/ubi-minimal", true},
						{"example.io/example/ubi-minimal", false},
						{"mirror.example.com/redhat", true},
						{"mirror.example.net/image", false},
					},
				},
			},
		},
		{
			name: "same source, ensuring mirrorSourcePolicy is handled correctly",
			input: []mirrorSet{
				{
					source:             "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrorSourcePolicy: config.NeverContactSource,
				},
				{
					source:             "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrorSourcePolicy: config.AllowContactingSource,
				},
				{
					source:             "quay.io/openshift-release-dev/ocp-release",
					mirrorSourcePolicy: config.AllowContactingSource,
				},
				{
					source:             "quay.io/openshift-release-dev/ocp-release",
					mirrorSourcePolicy: config.AllowContactingSource,
				},
			},
			expectedOutput: []mirrorSet{
				{
					source:             "quay.io/openshift-release-dev/ocp-release",
					mirrorSourcePolicy: config.AllowContactingSource,
				},
				{
					source:             "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrorSourcePolicy: config.NeverContactSource,
				},
			},
		},
		{
			name: "same source and duplicated mirrors, ensuring resolveTags is handled correctly",
			input: []mirrorSet{
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.net/image", false},
						{"mirror.example.com/redhat", false},
					},
				},
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.net/image", false},
						{"mirror.example.com/redhat", true},
					},
				},
			},
			expectedOutput: []mirrorSet{
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.com/redhat", true},
						{"mirror.example.net/image", false},
					},
				},
			},
		},
		{
			name: "different sources",
			input: []mirrorSet{
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.com/redhat", false},
						{"mirror.example.net/image", false},
					},
				},
				{
					source: "quay.io/openshift-release-dev/ocp-release",
					mirrors: []mirror{
						{"mirror.registry.com:443/ocp/release", false},
					},
				},
			},
			expectedOutput: []mirrorSet{
				{
					source: "quay.io/openshift-release-dev/ocp-release",
					mirrors: []mirror{
						{"mirror.registry.com:443/ocp/release", false},
					},
				},
				{
					source: "registry.access.redhat.com/ubi9/ubi-minimal",
					mirrors: []mirror{
						{"mirror.example.com/redhat", false},
						{"mirror.example.net/image", false},
					},
				},
			},
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := mergeMirrorSets(test.input)
			assert.Equal(t, out, test.expectedOutput)
		})
	}
}

func TestMergeMirrors(t *testing.T) {
	testCases := []struct {
		name            string
		mirrorsA        []mirror
		mirrorsB        []mirror
		expectedMirrors []mirror
	}{
		{
			name:            "no mirrors",
			mirrorsA:        []mirror{},
			mirrorsB:        []mirror{},
			expectedMirrors: []mirror{},
		},
		{
			name: "one empty slice",
			mirrorsA: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
			mirrorsB: []mirror{},
			expectedMirrors: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
		},
		{
			name: "duplicate mirror",
			mirrorsA: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
			mirrorsB: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
			expectedMirrors: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
		},
		{
			name: "duplicate host but different resolveTags",
			mirrorsA: []mirror{
				{host: "openshift.com", resolveTags: false},
			},
			mirrorsB: []mirror{
				{host: "openshift.com", resolveTags: true},
			},
			expectedMirrors: []mirror{
				{host: "openshift.com", resolveTags: true},
			},
		},
		{
			name: "different mirrors",
			mirrorsA: []mirror{
				{host: "redhat.com", resolveTags: false},
			},
			mirrorsB: []mirror{
				{host: "openshift.com", resolveTags: true},
			},
			expectedMirrors: []mirror{
				{host: "redhat.com", resolveTags: false},
				{host: "openshift.com", resolveTags: true},
			},
		},
		{
			name: "multiple mirrors",
			mirrorsA: []mirror{
				{host: "redhat.com", resolveTags: false},
				{host: "openshift.com", resolveTags: true},
				{host: "example.test.io", resolveTags: true},
			},
			mirrorsB: []mirror{
				{host: "openshift.com", resolveTags: true},
				{host: "example.test.io", resolveTags: true},
			},
			expectedMirrors: []mirror{
				{host: "redhat.com", resolveTags: false},
				{host: "openshift.com", resolveTags: true},
				{host: "example.test.io", resolveTags: true},
			},
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := mergeMirrors(test.mirrorsA, test.mirrorsB)
			assert.Equal(t, len(out), len(test.expectedMirrors))
			for _, m := range test.expectedMirrors {
				assert.Contains(t, out, m)
			}
		})
	}
}

func TestDropCommonSuffix(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		mirror   string
		expected string
	}{
		{
			name:     "Exact match",
			source:   "example.com/path/to/resource",
			mirror:   "example.com/path/to/resource",
			expected: "",
		},
		{
			name:     "Different domain",
			source:   "example.com/path/to/resource",
			mirror:   "example.org/path/to/resource",
			expected: "example.org",
		},
		{
			name:     "Last letter equal but no namespace match",
			source:   "example.com/path/to/resource/x",
			mirror:   "example.com/path/to/resourceax",
			expected: "example.com/path/to/resourceax",
		},
		{
			name:     "Different tags",
			source:   "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022",
			mirror:   "quay.io/powershell:23",
			expected: "quay.io/powershell:23",
		},
		{
			name:     "Different domain with tag",
			source:   "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022",
			mirror:   "quay.io/powershell:lts-nanoserver-ltsc2022",
			expected: "quay.io",
		},
		{
			name:     "1 leading namespace",
			source:   "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022",
			mirror:   "quay.io/random_namespace/powershell:lts-nanoserver-ltsc2022",
			expected: "quay.io/random_namespace",
		},
		{
			name:     "2 leading namespaces",
			source:   "mcr.microsoft.com/windows/servercore:ltsc2022",
			mirror:   "quay.io/mohashai/random_namespace/windows/servercore:ltsc2022",
			expected: "quay.io/mohashai/random_namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := dropCommonSuffix(tt.source, tt.mirror)
			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}
