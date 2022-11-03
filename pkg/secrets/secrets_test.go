package secrets

import (
	"testing"

	oconfig "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
)

func TestProcessTags(t *testing.T) {
	tests := []struct {
		name         string
		platformType oconfig.PlatformType
		userData     string
		expected     string
	}{
		{
			name:         "GCP user data",
			platformType: oconfig.GCPPlatformType,
			userData:     "gcp-user-data",
			expected:     "gcp-user-data",
		},
		{
			name:         "default user data",
			platformType: "",
			userData:     "default-user-data",
			expected:     "<powershell>default-user-data</powershell>\n<persist>true</persist>\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := processTags(test.platformType, test.userData)
			assert.Equal(t, test.expected, actual)
		})
	}
}
