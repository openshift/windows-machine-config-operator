package instance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/version"
)

func TestUpToDate(t *testing.T) {
	testCases := []struct {
		name        string
		input       Info
		expectedOut bool
	}{
		{
			name:        "No associated Node",
			input:       Info{Node: nil},
			expectedOut: false,
		},
		{
			name: "Version annotation missing",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{"wrong-annotation": version.Get()}},
				},
			},
			expectedOut: false,
		},
		{
			name: "Version annotation mismatch",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{metadata.VersionAnnotation: "incorrect"}},
				},
			},
			expectedOut: false,
		},
		{
			name: "Version annotation correct",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{metadata.VersionAnnotation: version.Get()}},
				},
			},
			expectedOut: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := test.input.UpToDate()
			assert.Equal(t, test.expectedOut, out)
		})
	}
}
func TestUpgradeRequired(t *testing.T) {
	testCases := []struct {
		name        string
		input       Info
		expectedOut bool
	}{
		{
			name:        "No associated Node",
			input:       Info{Node: nil},
			expectedOut: false,
		},
		{
			name: "Version annotation missing",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{"wrong-annotation": version.Get()}},
				},
			},
			expectedOut: false,
		},
		{
			name: "Version annotation mismatch",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{metadata.VersionAnnotation: "incorrect"}},
				},
			},
			expectedOut: true,
		},
		{
			name: "Version annotation correct",
			input: Info{
				Node: &core.Node{
					ObjectMeta: meta.ObjectMeta{Annotations: map[string]string{metadata.VersionAnnotation: version.Get()}},
				},
			},
			expectedOut: false,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := test.input.UpgradeRequired()
			assert.Equal(t, test.expectedOut, out)
		})
	}
}
