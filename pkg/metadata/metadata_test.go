package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
)

func TestGeneratePatch(t *testing.T) {
	testCases := []struct {
		name             string
		operation        string
		inputAnnotations map[string]string
		inputLabels      map[string]string
		expectedOut      []*patch.JSONPatch
		expectedErr      bool
	}{
		{
			name:             "Both nil",
			inputAnnotations: nil,
			inputLabels:      nil,
			operation:        "add",
			expectedOut:      nil,
			expectedErr:      true,
		},
		{
			name:             "Both empty",
			inputAnnotations: map[string]string{},
			inputLabels:      map[string]string{},
			operation:        "add",
			expectedOut:      nil,
			expectedErr:      true,
		},
		{
			name:             "Single annotation",
			inputAnnotations: map[string]string{"annotation-1": "3.0"},
			inputLabels:      nil,
			operation:        "add",
			expectedOut: []*patch.JSONPatch{{
				Op:    "add",
				Path:  "/metadata/annotations/annotation-1",
				Value: "3.0",
			}},
			expectedErr: false,
		},
		{
			name:             "Multiple annotations",
			inputAnnotations: map[string]string{"annotation-1": "3.0", "escaped/annotation": "17"},
			inputLabels:      nil,
			operation:        "add",
			expectedOut: []*patch.JSONPatch{
				{
					Op:    "add",
					Path:  "/metadata/annotations/annotation-1",
					Value: "3.0",
				},
				{
					Op:    "add",
					Path:  "/metadata/annotations/escaped~1annotation",
					Value: "17",
				},
			},
			expectedErr: false,
		},
		{
			name:             "Single label",
			inputAnnotations: nil,
			inputLabels:      map[string]string{"label-1": "3.0"},
			operation:        "add",
			expectedOut: []*patch.JSONPatch{{
				Op:    "add",
				Path:  "/metadata/labels/label-1",
				Value: "3.0",
			}},
			expectedErr: false,
		},
		{
			name:             "Multiple labels",
			inputAnnotations: nil,
			inputLabels:      map[string]string{"label-1": "3.0", "escaped/label": "test"},
			operation:        "add",
			expectedOut: []*patch.JSONPatch{
				{
					Op:    "add",
					Path:  "/metadata/labels/label-1",
					Value: "3.0",
				},
				{
					Op:    "add",
					Path:  "/metadata/labels/escaped~1label",
					Value: "test",
				},
			},
			expectedErr: false,
		},
		{
			name:             "Multiple labels and annotations",
			inputAnnotations: map[string]string{"annotation-1": "3.0", "escaped/annotation": "17"},
			inputLabels:      map[string]string{"label-1": "3.0", "escaped/label": "test"},
			operation:        "add",
			expectedOut: []*patch.JSONPatch{
				{
					Op:    "add",
					Path:  "/metadata/labels/label-1",
					Value: "3.0",
				},
				{
					Op:    "add",
					Path:  "/metadata/labels/escaped~1label",
					Value: "test",
				},
				{
					Op:    "add",
					Path:  "/metadata/annotations/annotation-1",
					Value: "3.0",
				},
				{
					Op:    "add",
					Path:  "/metadata/annotations/escaped~1annotation",
					Value: "17",
				},
			},
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := generatePatch(test.operation, test.inputLabels, test.inputAnnotations)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}
