package netutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveToIPv4Address(t *testing.T) {
	testCases := []struct {
		name        string
		address     string
		expectErr   bool
		expectedOut string
	}{
		{
			name:        "ipv4 address",
			address:     "127.0.0.1",
			expectErr:   false,
			expectedOut: "127.0.0.1",
		},
		{
			name:        "ipv4 resolvable dns address",
			address:     "localhost",
			expectErr:   false,
			expectedOut: "127.0.0.1",
		},
		{
			name:        "ipv6 address",
			address:     "::1",
			expectErr:   true,
			expectedOut: "",
		},
		{
			name:        "unresolvable DNS address",
			address:     "fake.local",
			expectErr:   true,
			expectedOut: "",
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := ResolveToIPv4Address(test.address)
			if test.expectErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedOut, out)
		})
	}
}
