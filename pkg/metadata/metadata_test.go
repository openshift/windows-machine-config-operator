package metadata

import (
	"testing"

	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeneratePatch(t *testing.T) {
	testCases := []struct {
		name        string
		operation   string
		input       map[string]string
		expectedOut []*patch.JSONPatch
		expectedErr bool
	}{
		{
			name:        "Annotations nil",
			input:       nil,
			operation:   "add",
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "Annotations empty",
			input:       map[string]string{},
			operation:   "add",
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:      "Single annotation",
			input:     map[string]string{"annotation-1": "3.0"},
			operation: "add",
			expectedOut: []*patch.JSONPatch{{
				Op:    "add",
				Path:  "/metadata/annotations/annotation-1",
				Value: "3.0",
			}},
			expectedErr: false,
		},
		{
			name:      "Multiple annotations",
			input:     map[string]string{"annotation-1": "3.0", "escaped/annotation": "17"},
			operation: "add",
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
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := generateAnnotationPatch(test.operation, test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}
