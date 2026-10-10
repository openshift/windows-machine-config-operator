package controllers

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/version"
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
// cluster infrastructure and is not covered by these unit tests. The tests
// below validate the decision/caller-path logic only; remote SSH and TLS
// handshake behavior is deferred to integration or e2e testing.
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
						metadata.WindowsExporterWebConfigSHAAnnotation: "abc123",
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
						metadata.WindowsExporterWebConfigSHAAnnotation: "old-sha",
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

// TestEnsureInstanceIsUpToDateWebConfigPath verifies that
// ensureInstanceIsUpToDate enters the webconfig reconciliation branch when the
// instance version annotation matches the current WMCO version. This exercises
// the current-version caller path through ensureWebConfigIsUpToDate.
func TestEnsureInstanceIsUpToDateWebConfigPath(t *testing.T) {
	testCases := []struct {
		name      string
		info      *instance.Info
		wantErr   bool
		errSubstr string
	}{
		{
			name: "up-to-date instance with mismatched SHA enters webconfig path and errors without client",
			info: &instance.Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "win-node-stale-sha",
						Annotations: map[string]string{
							metadata.VersionAnnotation:                     version.Get(),
							metadata.WindowsExporterWebConfigSHAAnnotation: "stale-sha",
						},
					},
				},
			},
			// With no cache client, ensureWebConfigForNode will fail when
			// the SHA does not match (i.e. it attempts the push). The test
			// validates the webconfig reconciliation path is reached and
			// errors are propagated.
			wantErr:   true,
			errSubstr: "webconfig",
		},
		{
			name:    "nil instance returns error",
			info:    nil,
			wantErr: true,
		},
		{
			name: "outdated instance attempts full configure and errors without infrastructure",
			info: &instance.Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "win-node-old",
						Annotations: map[string]string{
							metadata.VersionAnnotation: "0.0.0-old",
						},
					},
				},
			},
			wantErr: true,
		},
	}

	// Pre-populate the webconfig SHA so the reconciliation path detects a
	// mismatch against the stale-sha annotation in the test node.
	payload.SetWebConfigSHAForTest("expected-sha")
	defer payload.SetWebConfigSHAForTest("")

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := &instanceReconciler{
				log: logr.Discard(),
			}
			err := r.ensureInstanceIsUpToDate(context.Background(), tc.info, nil, nil)
			if tc.wantErr {
				require.Error(t, err)
				if tc.errSubstr != "" {
					assert.Contains(t, err.Error(), tc.errSubstr,
						"error should contain %q", tc.errSubstr)
				}
			} else {
				require.NoError(t, err)
			}
		})
	}
}
