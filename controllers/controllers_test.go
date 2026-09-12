package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
)

func TestGetAddress(t *testing.T) {
	testCases := []struct {
		name        string
		input       []core.NodeAddress
		expectedOut []string
		expectedErr bool
	}{
		{
			name:        "no addresses",
			input:       []core.NodeAddress{{}},
			expectedOut: []string{""},
			expectedErr: true,
		},
		{
			name:        "ipv6",
			input:       []core.NodeAddress{{Type: core.NodeInternalIP, Address: "::1"}},
			expectedOut: []string{""},
			expectedErr: true,
		},
		{
			name:        "ipv4",
			input:       []core.NodeAddress{{Type: core.NodeInternalIP, Address: "127.0.0.1"}},
			expectedOut: []string{"127.0.0.1"},
			expectedErr: false,
		},
		{
			name:        "dns",
			input:       []core.NodeAddress{{Type: core.NodeInternalDNS, Address: "localhost"}},
			expectedOut: []string{"localhost"},
			expectedErr: false,
		},
		{
			name: "dns and ipv4",
			input: []core.NodeAddress{
				{Type: core.NodeInternalDNS, Address: "localhost"},
				{Type: core.NodeInternalIP, Address: "127.0.0.1"}},
			expectedOut: []string{"localhost", "127.0.0.1"},
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := GetAddress(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			// The output can be any valid address in the expected list, so check that the output is one of the possible
			// correct ones
			assert.Contains(t, test.expectedOut, out)
		})
	}
}

// TestEnsureWebConfigForNodeDecision verifies the decision logic used by
// ensureWebConfigForNode to determine whether a webconfig push is needed.
// After refactoring, ensureWebConfigForNode delegates this decision to
// instance.Info.WebConfigUpToDate. These table tests exercise the critical
// scenarios the controller must handle correctly.
//
// The full ensureWebConfigForNode path (SSH push + annotation write) requires
// cluster infrastructure and is covered by e2e tests that exercise the
// complete ensureWebConfigForNode → updateWebConfig → nodeconfig pipeline.
func TestEnsureWebConfigForNodeDecision(t *testing.T) {
	testCases := []struct {
		name         string
		node         *core.Node
		expectedSHA  string
		wantUpToDate bool // true = no push needed (no-op)
	}{
		{
			name: "Missing SHA annotation triggers push",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:        "win-node-1",
					Annotations: map[string]string{},
				},
			},
			expectedSHA:  "abc123",
			wantUpToDate: false,
		},
		{
			name: "Matching SHA annotation is a no-op",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-2",
					Annotations: map[string]string{
						metadata.WebConfigSHAAnnotation: "abc123",
					},
				},
			},
			expectedSHA:  "abc123",
			wantUpToDate: true,
		},
		{
			name: "Mismatched SHA annotation triggers push",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-3",
					Annotations: map[string]string{
						metadata.WebConfigSHAAnnotation: "old-sha",
					},
				},
			},
			expectedSHA:  "new-sha",
			wantUpToDate: false,
		},
		{
			name:         "Nil node is handled gracefully",
			node:         nil,
			expectedSHA:  "abc123",
			wantUpToDate: true,
		},
		{
			name: "Empty expected SHA is always up to date",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name:        "win-node-4",
					Annotations: map[string]string{},
				},
			},
			expectedSHA:  "",
			wantUpToDate: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			info := &instance.Info{Node: tc.node}
			got := info.WebConfigUpToDate(tc.expectedSHA)
			assert.Equal(t, tc.wantUpToDate, got)
		})
	}
}
