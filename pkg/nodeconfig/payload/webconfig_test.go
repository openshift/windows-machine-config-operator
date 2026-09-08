package payload

import (
	"strings"
	"testing"

	oconfig "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateWebConfig(t *testing.T) {
	tests := []struct {
		name              string
		tlsProfileSpec    oconfig.TLSProfileSpec
		honorTLSProfile   bool
		wantContains      []string
		wantNotContains   []string
		wantUnsupported   int
		wantBaseConfigLen bool // true if we expect the short base config
	}{
		{
			name:            "honor disabled returns base config",
			tlsProfileSpec:  *oconfig.TLSProfiles[oconfig.TLSProfileIntermediateType],
			honorTLSProfile: false,
			wantContains: []string{
				"tls_server_config:",
				"cert_file: C:\\\\k\\\\tls\\\\certs\\\\tls.crt",
				"key_file: C:\\\\k\\\\tls\\\\certs\\\\tls.key",
			},
			wantNotContains: []string{
				"min_version:",
				"cipher_suites:",
				"curve_preferences:",
			},
			wantBaseConfigLen: true,
		},
		{
			name:            "intermediate profile",
			tlsProfileSpec:  *oconfig.TLSProfiles[oconfig.TLSProfileIntermediateType],
			honorTLSProfile: true,
			wantContains: []string{
				"tls_server_config:",
				"cert_file: C:\\\\k\\\\tls\\\\certs\\\\tls.crt",
				"key_file: C:\\\\k\\\\tls\\\\certs\\\\tls.key",
				"min_version: TLS12",
				"cipher_suites:",
				"curve_preferences:",
				"X25519",
				"CurveP256",
				"CurveP384",
			},
			wantNotContains: []string{
				// TLS 1.3 ciphers should be filtered out
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
				"TLS_CHACHA20_POLY1305_SHA256",
			},
		},
		{
			name:            "old profile includes TLS10 and more ciphers",
			tlsProfileSpec:  *oconfig.TLSProfiles[oconfig.TLSProfileOldType],
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS10",
				"cipher_suites:",
				"curve_preferences:",
			},
			wantNotContains: []string{
				"TLS_AES_128_GCM_SHA256",
			},
		},
		{
			name:            "modern profile (TLS 1.3) omits cipher_suites",
			tlsProfileSpec:  *oconfig.TLSProfiles[oconfig.TLSProfileModernType],
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS13",
				"curve_preferences:",
			},
			wantNotContains: []string{
				"cipher_suites:",
			},
		},
		{
			name: "custom profile with specific settings",
			tlsProfileSpec: oconfig.TLSProfileSpec{
				MinTLSVersion: oconfig.VersionTLS12,
				Ciphers: []string{
					"ECDHE-ECDSA-AES128-GCM-SHA256",
					"ECDHE-RSA-AES256-GCM-SHA384",
				},
				Groups: []oconfig.TLSGroup{
					oconfig.TLSGroupX25519,
					oconfig.TLSGroupSecP256r1,
				},
			},
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS12",
				"cipher_suites:",
				"curve_preferences:",
				"X25519",
				"CurveP256",
			},
			wantNotContains: []string{
				"CurveP384",
			},
		},
		{
			name: "custom profile with only TLS 1.3 ciphers",
			tlsProfileSpec: oconfig.TLSProfileSpec{
				MinTLSVersion: oconfig.VersionTLS12,
				Ciphers: []string{
					"TLS_AES_128_GCM_SHA256",
					"TLS_AES_256_GCM_SHA384",
				},
			},
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS12",
			},
			wantNotContains: []string{
				"cipher_suites:",
			},
		},
		{
			name: "profile with unsupported ciphers",
			tlsProfileSpec: oconfig.TLSProfileSpec{
				MinTLSVersion: oconfig.VersionTLS12,
				Ciphers: []string{
					"ECDHE-ECDSA-AES128-GCM-SHA256",
					"UNSUPPORTED-CIPHER",
				},
			},
			honorTLSProfile: true,
			wantContains: []string{
				"cipher_suites:",
			},
			wantUnsupported: 1,
		},
		{
			name: "profile with no groups",
			tlsProfileSpec: oconfig.TLSProfileSpec{
				MinTLSVersion: oconfig.VersionTLS12,
				Ciphers: []string{
					"ECDHE-RSA-AES128-GCM-SHA256",
				},
			},
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS12",
				"cipher_suites:",
			},
			wantNotContains: []string{
				"curve_preferences:",
			},
		},
		{
			name: "profile with unsupported groups only",
			tlsProfileSpec: oconfig.TLSProfileSpec{
				MinTLSVersion: oconfig.VersionTLS12,
				Ciphers: []string{
					"ECDHE-RSA-AES128-GCM-SHA256",
				},
				Groups: []oconfig.TLSGroup{
					oconfig.TLSGroupX25519MLKEM768,
				},
			},
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS12",
			},
			wantNotContains: []string{
				"curve_preferences:",
				"X25519MLKEM768",
			},
			wantUnsupported: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, unsupported := GenerateWebConfig(tt.tlsProfileSpec, tt.honorTLSProfile)

			for _, want := range tt.wantContains {
				assert.Contains(t, content, want,
					"webconfig should contain %q\nActual:\n%s", want, content)
			}

			for _, notWant := range tt.wantNotContains {
				assert.NotContains(t, content, notWant,
					"webconfig should NOT contain %q\nActual:\n%s", notWant, content)
			}

			if tt.wantUnsupported > 0 {
				assert.Len(t, unsupported, tt.wantUnsupported,
					"expected %d unsupported ciphers, got %d: %v",
					tt.wantUnsupported, len(unsupported), unsupported)
			}

			if tt.wantBaseConfigLen {
				// Base config should be relatively short (only cert + key)
				lines := strings.Split(strings.TrimSpace(content), "\n")
				assert.Equal(t, 3, len(lines),
					"base config should have exactly 3 lines (header + cert + key)")
			}
		})
	}
}

func TestMapTLSVersion(t *testing.T) {
	tests := []struct {
		input    oconfig.TLSProtocolVersion
		expected string
	}{
		{oconfig.VersionTLS10, "TLS10"},
		{oconfig.VersionTLS11, "TLS11"},
		{oconfig.VersionTLS12, "TLS12"},
		{oconfig.VersionTLS13, "TLS13"},
		{"UnknownVersion", "TLS12"}, // unknown defaults to TLS12
	}

	for _, tt := range tests {
		t.Run(string(tt.input), func(t *testing.T) {
			result := mapTLSVersion(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestMapCipherSuites(t *testing.T) {
	tests := []struct {
		name            string
		input           []string
		wantCiphers     []string
		wantUnsupported []string
	}{
		{
			name: "OpenSSL names converted to IANA",
			input: []string{
				"ECDHE-ECDSA-AES128-GCM-SHA256",
				"ECDHE-RSA-AES256-GCM-SHA384",
			},
			wantCiphers: []string{
				"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
				"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
			},
		},
		{
			name: "IANA/Go cipher name used directly",
			input: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
		},
		{
			name: "TLS 1.3 ciphers filtered out",
			input: []string{
				"TLS_AES_128_GCM_SHA256",
				"TLS_AES_256_GCM_SHA384",
				"TLS_CHACHA20_POLY1305_SHA256",
				"ECDHE-RSA-AES128-GCM-SHA256",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
		},
		{
			name: "unsupported ciphers collected",
			input: []string{
				"ECDHE-RSA-AES128-GCM-SHA256",
				"NOT-A-REAL-CIPHER",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantUnsupported: []string{"NOT-A-REAL-CIPHER"},
		},
		{
			name:        "empty input",
			input:       []string{},
			wantCiphers: nil,
		},
		{
			name:        "nil input",
			input:       nil,
			wantCiphers: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, unsupported := mapCipherSuites(tt.input)
			assert.Equal(t, tt.wantCiphers, result)
			if tt.wantUnsupported != nil {
				assert.Equal(t, tt.wantUnsupported, unsupported)
			}
		})
	}
}

func TestMapCurvePreferences(t *testing.T) {
	tests := []struct {
		name            string
		input           []oconfig.TLSGroup
		expected        []string
		wantUnsupported []string
	}{
		{
			name:     "standard groups mapped",
			input:    []oconfig.TLSGroup{oconfig.TLSGroupX25519, oconfig.TLSGroupSecP256r1, oconfig.TLSGroupSecP384r1},
			expected: []string{"X25519", "CurveP256", "CurveP384"},
		},
		{
			name:            "unsupported post-quantum groups returned",
			input:           []oconfig.TLSGroup{oconfig.TLSGroupX25519MLKEM768, oconfig.TLSGroupX25519, oconfig.TLSGroupSecP256r1},
			expected:        []string{"X25519", "CurveP256"},
			wantUnsupported: []string{string(oconfig.TLSGroupX25519MLKEM768)},
		},
		{
			name:     "all groups including P521",
			input:    []oconfig.TLSGroup{oconfig.TLSGroupX25519, oconfig.TLSGroupSecP256r1, oconfig.TLSGroupSecP384r1, oconfig.TLSGroupSecP521r1},
			expected: []string{"X25519", "CurveP256", "CurveP384", "CurveP521"},
		},
		{
			name:     "empty input",
			input:    []oconfig.TLSGroup{},
			expected: nil,
		},
		{
			name:     "nil input",
			input:    nil,
			expected: nil,
		},
		{
			name:            "only unsupported groups",
			input:           []oconfig.TLSGroup{oconfig.TLSGroupX25519MLKEM768},
			expected:        nil,
			wantUnsupported: []string{string(oconfig.TLSGroupX25519MLKEM768)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, unsupported := mapCurvePreferences(tt.input)
			assert.Equal(t, tt.expected, result)
			if tt.wantUnsupported != nil {
				assert.Equal(t, tt.wantUnsupported, unsupported)
			} else {
				assert.Empty(t, unsupported)
			}
		})
	}
}

func TestGenerateWebConfigYAMLValidity(t *testing.T) {
	// Verify the generated YAML starts and ends correctly
	intermediateProfile := *oconfig.TLSProfiles[oconfig.TLSProfileIntermediateType]
	content, _ := GenerateWebConfig(intermediateProfile, true)

	// Check starts with expected header
	require.True(t, strings.HasPrefix(content, "tls_server_config:\n"),
		"webconfig should start with tls_server_config header")

	// Check ends with newline
	require.True(t, strings.HasSuffix(content, "\n"),
		"webconfig should end with a newline")

	// Verify indentation is consistent (2 spaces for keys under tls_server_config)
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for i, line := range lines {
		if i == 0 {
			// First line is the top-level key
			assert.Equal(t, "tls_server_config:", line)
			continue
		}
		// All other lines should start with at least 2 spaces
		assert.True(t, strings.HasPrefix(line, "  "),
			"line %d should be indented: %q", i+1, line)
	}
}

func TestBaseWebConfigMatchesStaticFile(t *testing.T) {
	// The base webconfig (when not honoring TLS profile) should match
	// the original static file content
	content := generateBaseWebConfig()

	expected := "tls_server_config:\n" +
		"  cert_file: C:\\\\k\\\\tls\\\\certs\\\\tls.crt\n" +
		"  key_file: C:\\\\k\\\\tls\\\\certs\\\\tls.key\n"

	assert.Equal(t, expected, content,
		"base webconfig should match the original static file content")
}
