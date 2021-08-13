package annotations

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAddPatch(t *testing.T) {
	testCases := []struct {
		name        string
		input       map[string]string
		expectedOut []byte
		expectedErr bool
	}{
		{
			name:        "Annotations nil",
			input:       nil,
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "Annotations empty",
			input:       map[string]string{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "Single annotation",
			input:       map[string]string{"annotation-1": "3.0"},
			expectedOut: []byte("[{\"op\":\"add\",\"path\":\"/metadata/annotations/annotation-1\",\"value\":\"3.0\"}]"),
			expectedErr: false,
		},
		{
			name:  "Multiple annotations",
			input: map[string]string{"annotation-1": "3.0", "annotation-2": "17"},
			expectedOut: []byte("[{\"op\":\"add\",\"path\":\"/metadata/annotations/annotation-1\",\"value\":\"3.0\"}," +
				"{\"op\":\"add\",\"path\":\"/metadata/annotations/annotation-2\",\"value\":\"17\"}]"),
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := GenerateAddPatch(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}
func TestGenerateRemovePatch(t *testing.T) {
	testCases := []struct {
		name        string
		input       []string
		expectedOut []byte
		expectedErr bool
	}{
		{
			name:        "Annotations nil",
			input:       nil,
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "Annotations empty",
			input:       []string{},
			expectedOut: nil,
			expectedErr: true,
		},
		{
			name:        "Single annotation",
			input:       []string{"annotation-1"},
			expectedOut: []byte("[{\"op\":\"remove\",\"path\":\"/metadata/annotations/annotation-1\",\"value\":\"\"}]"),
			expectedErr: false,
		},
		{
			name:  "Multiple annotations",
			input: []string{"annotation-1", "annotation-2"},
			expectedOut: []byte("[{\"op\":\"remove\",\"path\":\"/metadata/annotations/annotation-1\",\"value\":\"\"}," +
				"{\"op\":\"remove\",\"path\":\"/metadata/annotations/annotation-2\",\"value\":\"\"}]"),
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := GenerateRemovePatch(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.ElementsMatch(t, test.expectedOut, out)
		})
	}
}
