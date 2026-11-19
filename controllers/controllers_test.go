package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

// TestEnsureWebConfigForNode verifies the full ensureWebConfigForNode path,
// exercising the code path used by the Machine controller (direct call to
// ensureWebConfigForNode). The updateWebConfigOverride field stubs the SSH
// file-transfer dependency so the test exercises decision logic, update
// invocation, and annotation writes end-to-end.
func TestEnsureWebConfigForNode(t *testing.T) {
	payload.SetWebConfigSHAForTest("expected-sha")
	defer payload.SetWebConfigSHAForTest("")

	testCases := []struct {
		name             string
		node             *core.Node
		updateErr        error // error returned by the update override
		wantUpdateCalled bool
		wantErr          bool
		wantSHA          string // expected SHA annotation after reconcile
	}{
		{
			name: "mismatched SHA triggers push and updates annotation",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-stale",
					Annotations: map[string]string{
						metadata.WindowsExporterWebConfigSHAAnnotation: "old-sha",
					},
				},
			},
			wantUpdateCalled: true,
			wantSHA:          "expected-sha",
		},
		{
			name: "missing SHA annotation triggers push and sets annotation",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-no-sha",
					Annotations: map[string]string{
						// Version annotation is always present on configured
						// nodes; the SHA annotation is absent when the node
						// was configured before webconfig support was added.
						metadata.VersionAnnotation: version.Get(),
					},
				},
			},
			wantUpdateCalled: true,
			wantSHA:          "expected-sha",
		},
		{
			name: "matching SHA is a no-op",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-current",
					Annotations: map[string]string{
						metadata.WindowsExporterWebConfigSHAAnnotation: "expected-sha",
					},
				},
			},
			wantUpdateCalled: false,
			wantSHA:          "expected-sha",
		},
		{
			name: "update failure propagates error and does not write annotation",
			node: &core.Node{
				ObjectMeta: meta.ObjectMeta{
					Name: "win-node-fail",
					Annotations: map[string]string{
						metadata.WindowsExporterWebConfigSHAAnnotation: "old-sha",
					},
				},
			},
			updateErr:        fmt.Errorf("ssh connection refused"),
			wantUpdateCalled: true,
			wantErr:          true,
			wantSHA:          "old-sha", // unchanged on failure
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, core.AddToScheme(scheme))
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(tc.node).Build()

			updateCalled := false
			r := &instanceReconciler{
				client: fakeClient,
				log:    logr.Discard(),
				updateWebConfigOverride: func(ctx context.Context, node core.Node) error {
					updateCalled = true
					return tc.updateErr
				},
			}

			err := r.ensureWebConfigForNode(context.Background(), *tc.node)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantUpdateCalled, updateCalled,
				"updateWebConfig called=%v, want=%v", updateCalled, tc.wantUpdateCalled)

			// Verify the annotation on the node in the fake client
			updatedNode := &core.Node{}
			getErr := fakeClient.Get(context.Background(),
				client.ObjectKeyFromObject(tc.node), updatedNode)
			require.NoError(t, getErr)
			assert.Equal(t, tc.wantSHA,
				updatedNode.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation],
				"SHA annotation should be %q after reconcile", tc.wantSHA)
		})
	}
}

// TestEnsureInstanceIsUpToDateWebConfigPath verifies that
// ensureInstanceIsUpToDate enters the webconfig reconciliation branch when the
// instance version annotation matches the current WMCO version. This exercises
// the BYOH/ConfigMap controller path through ensureWebConfigIsUpToDate and
// ensureWebConfigForNode, verifying update invocation and annotation writes.
func TestEnsureInstanceIsUpToDateWebConfigPath(t *testing.T) {
	payload.SetWebConfigSHAForTest("expected-sha")
	defer payload.SetWebConfigSHAForTest("")

	testCases := []struct {
		name             string
		info             *instance.Info
		updateErr        error // error returned by the update override
		wantUpdateCalled bool
		wantErr          bool
		errSubstr        string
		wantSHA          string // expected SHA annotation after reconcile; empty skips check
	}{
		{
			name: "up-to-date instance with mismatched SHA pushes webconfig and updates annotation",
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
			wantUpdateCalled: true,
			wantSHA:          "expected-sha",
		},
		{
			name: "up-to-date instance with matching SHA is a no-op",
			info: &instance.Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "win-node-current-sha",
						Annotations: map[string]string{
							metadata.VersionAnnotation:                     version.Get(),
							metadata.WindowsExporterWebConfigSHAAnnotation: "expected-sha",
						},
					},
				},
			},
			wantUpdateCalled: false,
			wantSHA:          "expected-sha",
		},
		{
			name:    "nil instance returns error",
			info:    nil,
			wantErr: true,
		},
		{
			name: "update failure in webconfig path propagates error with context",
			info: &instance.Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{
						Name: "win-node-update-fail",
						Annotations: map[string]string{
							metadata.VersionAnnotation:                     version.Get(),
							metadata.WindowsExporterWebConfigSHAAnnotation: "stale-sha",
						},
					},
				},
			},
			updateErr:        fmt.Errorf("ssh connection refused"),
			wantUpdateCalled: true,
			wantErr:          true,
			errSubstr:        "webconfig",
			wantSHA:          "stale-sha", // unchanged on failure
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, core.AddToScheme(scheme))

			// Build the fake client, pre-seeding the node if present
			clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.info != nil && tc.info.Node != nil {
				clientBuilder = clientBuilder.WithObjects(tc.info.Node)
			}
			fakeClient := clientBuilder.Build()

			updateCalled := false
			r := &instanceReconciler{
				client: fakeClient,
				log:    logr.Discard(),
				updateWebConfigOverride: func(ctx context.Context, node core.Node) error {
					updateCalled = true
					return tc.updateErr
				},
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
			assert.Equal(t, tc.wantUpdateCalled, updateCalled,
				"updateWebConfig called=%v, want=%v", updateCalled, tc.wantUpdateCalled)

			// Verify annotation if node exists and wantSHA is specified
			if tc.info != nil && tc.info.Node != nil && tc.wantSHA != "" {
				updatedNode := &core.Node{}
				getErr := fakeClient.Get(context.Background(),
					client.ObjectKeyFromObject(tc.info.Node), updatedNode)
				require.NoError(t, getErr)
				assert.Equal(t, tc.wantSHA,
					updatedNode.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation],
					"SHA annotation should be %q after reconcile", tc.wantSHA)
			}
		})
	}
}

// TestWebConfigFailureIsolation verifies that a webconfig push failure for one
// node does not prevent another node from being updated. Each call to
// ensureWebConfigForNode is independent, mirroring the Machine controller's
// per-Reconcile behavior.
func TestWebConfigFailureIsolation(t *testing.T) {
	payload.SetWebConfigSHAForTest("expected-sha")
	defer payload.SetWebConfigSHAForTest("")

	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))

	failNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "win-node-fail",
			Annotations: map[string]string{
				metadata.WindowsExporterWebConfigSHAAnnotation: "old-sha",
			},
		},
	}
	successNode := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: "win-node-success",
			Annotations: map[string]string{
				metadata.WindowsExporterWebConfigSHAAnnotation: "old-sha",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(failNode, successNode).Build()

	r := &instanceReconciler{
		client: fakeClient,
		log:    logr.Discard(),
		updateWebConfigOverride: func(ctx context.Context, node core.Node) error {
			if node.Name == "win-node-fail" {
				return fmt.Errorf("ssh connection refused")
			}
			return nil
		},
	}

	// First node fails
	err := r.ensureWebConfigForNode(context.Background(), *failNode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ssh connection refused")

	// Second node succeeds despite first node's failure
	err = r.ensureWebConfigForNode(context.Background(), *successNode)
	require.NoError(t, err)

	// Verify: failed node's annotation unchanged
	updatedFail := &core.Node{}
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKeyFromObject(failNode), updatedFail))
	assert.Equal(t, "old-sha",
		updatedFail.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation],
		"failed node should keep old SHA")

	// Verify: successful node's annotation updated
	updatedSuccess := &core.Node{}
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKeyFromObject(successNode), updatedSuccess))
	assert.Equal(t, "expected-sha",
		updatedSuccess.Annotations[metadata.WindowsExporterWebConfigSHAAnnotation],
		"successful node should have new SHA")
}
