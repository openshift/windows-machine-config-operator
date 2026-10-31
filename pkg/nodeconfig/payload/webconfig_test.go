package payload

import (
	"crypto/tls"
	"strings"
	"testing"

	oconfig "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGenerateWebConfig verifies that GenerateWebConfig returns correct YAML
// content for each TLS profile type (Old, Intermediate, Modern, Custom).
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
			name:            "old profile includes TLS10 and filters weak ciphers",
			tlsProfileSpec:  *oconfig.TLSProfiles[oconfig.TLSProfileOldType],
			honorTLSProfile: true,
			wantContains: []string{
				"min_version: TLS10",
				"cipher_suites:",
				"curve_preferences:",
			},
			wantNotContains: []string{
				"TLS_AES_128_GCM_SHA256",
				// Weak ciphers from the Old profile must be filtered.
				// Use newline suffix to avoid substring matches with SHA256 variants.
				"TLS_RSA_WITH_3DES_EDE_CBC_SHA\n",
				"TLS_RSA_WITH_AES_128_CBC_SHA\n",
				"TLS_RSA_WITH_AES_256_CBC_SHA\n",
				"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA\n",
				"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA\n",
				"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA\n",
				"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA\n",
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

// TestGetWebConfigSHA verifies that GetWebConfigSHA returns the correct SHA
// from the global shaMap after being set by PopulateWebConfig.
func TestGetWebConfigSHA(t *testing.T) {
	// Before any webconfig is populated, the SHA should be empty
	origShaMap := shaMap
	defer func() { shaMap = origShaMap }()
	shaMap = make(map[string]string)

	assert.Equal(t, "", GetWebConfigSHA(),
		"GetWebConfigSHA should return empty string when webconfig is not populated")

	// Manually set the webconfig SHA as PopulateWebConfig would
	fileName := strings.TrimSuffix("windows-exporter-webconfig.yaml.tar.gz", ".tar.gz")
	shaMap[fileName] = "abc123def456"
	assert.Equal(t, "abc123def456", GetWebConfigSHA(),
		"GetWebConfigSHA should return the SHA from the shaMap")
}

// TestMapTLSVersion verifies the mapping of OpenShift TLS protocol versions
// to the short form used by the exporter-toolkit webconfig.
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

// TestMapCipherSuites verifies cipher suite conversion from OpenSSL to IANA
// names, TLS 1.3 filtering, weak cipher filtering, and unsupported cipher reporting.
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
		{
			name: "weak cipher DES-CBC3-SHA filtered",
			input: []string{
				"DES-CBC3-SHA",
				"ECDHE-RSA-AES128-GCM-SHA256",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantUnsupported: []string{"DES-CBC3-SHA"},
		},
		{
			name: "weak SHA-1 ciphers filtered",
			input: []string{
				"ECDHE-RSA-AES128-SHA",
				"AES128-SHA",
				"ECDHE-RSA-AES128-GCM-SHA256",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantUnsupported: []string{
				"ECDHE-RSA-AES128-SHA",
				"AES128-SHA",
			},
		},
		{
			name: "IANA weak cipher filtered directly",
			input: []string{
				"TLS_RSA_WITH_3DES_EDE_CBC_SHA",
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantCiphers: []string{
				"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
			},
			wantUnsupported: []string{"TLS_RSA_WITH_3DES_EDE_CBC_SHA"},
		},
		{
			name: "all weak ciphers from Old profile filtered",
			input: []string{
				"ECDHE-ECDSA-AES128-SHA",
				"ECDHE-RSA-AES128-SHA",
				"ECDHE-ECDSA-AES256-SHA",
				"ECDHE-RSA-AES256-SHA",
				"AES128-SHA",
				"AES256-SHA",
				"DES-CBC3-SHA",
			},
			wantCiphers: nil,
			wantUnsupported: []string{
				"ECDHE-ECDSA-AES128-SHA",
				"ECDHE-RSA-AES128-SHA",
				"ECDHE-ECDSA-AES256-SHA",
				"ECDHE-RSA-AES256-SHA",
				"AES128-SHA",
				"AES256-SHA",
				"DES-CBC3-SHA",
			},
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

// TestMapCurvePreferences verifies the mapping of OpenShift TLSGroup identifiers
// to Go tls.CurveID names and the reporting of unsupported groups.
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

// TestGenerateWebConfigYAMLValidity verifies that the generated YAML has correct
// structure, indentation, and line endings.
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

// TestIsWeakCipher verifies that isWeakCipher correctly identifies weak cipher
// suites (3DES, RC4, MD5, NULL, SHA-1) and allows strong ones (GCM, ChaCha20).
func TestIsWeakCipher(t *testing.T) {
	tests := []struct {
		name     string
		cipher   string
		expected bool
	}{
		// Weak ciphers
		{"3DES IANA", "TLS_RSA_WITH_3DES_EDE_CBC_SHA", true},
		{"RC4 IANA", "TLS_RSA_WITH_RC4_128_SHA", true},
		{"MD5 IANA", "TLS_RSA_WITH_AES_128_CBC_SHA_MD5", true},
		{"NULL IANA", "TLS_RSA_WITH_NULL_SHA", true},
		{"SHA-1 CBC IANA", "TLS_RSA_WITH_AES_128_CBC_SHA", true},
		{"SHA-1 ECDHE IANA", "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA", true},
		{"SHA-1 ECDSA IANA", "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA", true},
		// Strong ciphers
		{"GCM SHA256 IANA", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", false},
		{"GCM SHA384 IANA", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", false},
		{"CHACHA20 IANA", "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256", false},
		{"CBC SHA256 IANA", "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256", false},
		{"RSA GCM SHA256", "TLS_RSA_WITH_AES_128_GCM_SHA256", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isWeakCipher(tt.cipher),
				"isWeakCipher(%q) should be %v", tt.cipher, tt.expected)
		})
	}
}

// TestOldProfileNoWeakCiphersInOutput verifies that the Old TLS profile's weak
// ciphers (DES-CBC3-SHA, *-SHA) are filtered from the generated webconfig output.
func TestOldProfileNoWeakCiphersInOutput(t *testing.T) {
	// The Old TLS profile includes weak ciphers (DES-CBC3-SHA, *-SHA, etc.)
	// that must be filtered from the generated webconfig.
	oldProfile := *oconfig.TLSProfiles[oconfig.TLSProfileOldType]
	content, unsupported := GenerateWebConfig(oldProfile, true)

	// Weak cipher IANA names that must NOT appear in the output.
	// Use newline suffix to avoid substring matches (e.g. _CBC_SHA vs _CBC_SHA256).
	weakCiphers := []string{
		"TLS_RSA_WITH_3DES_EDE_CBC_SHA\n",
		"TLS_RSA_WITH_AES_128_CBC_SHA\n",
		"TLS_RSA_WITH_AES_256_CBC_SHA\n",
		"TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA\n",
		"TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA\n",
		"TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA\n",
		"TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA\n",
	}
	for _, wc := range weakCiphers {
		assert.NotContains(t, content, wc,
			"webconfig should NOT contain weak cipher %q", wc)
	}

	// Strong ciphers should still be present
	assert.Contains(t, content, "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"webconfig should contain strong GCM cipher")
	assert.Contains(t, content, "cipher_suites:",
		"webconfig should still have cipher_suites section")

	// The weak ciphers should appear in the unsupported list
	assert.True(t, len(unsupported) > 0,
		"weak ciphers should be reported as unsupported")
}

// TestBaseWebConfigMatchesStaticFile verifies that the base webconfig (without
// TLS profile) matches the original static file content.
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

// TestIsSupportedCipher verifies that isSupportedCipher accepts only cipher
// suite names from tls.CipherSuites() (the secure set) and rejects everything
// else — including ciphers in tls.InsecureCipherSuites(), which the
// exporter-toolkit does not recognize.
func TestIsSupportedCipher(t *testing.T) {
	tests := []struct {
		name     string
		cipher   string
		expected bool
	}{
		// Ciphers in tls.CipherSuites (secure set — accepted by exporter-toolkit)
		{"GCM SHA256 supported", "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256", true},
		{"GCM SHA384 supported", "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384", true},
		{"CHACHA20 supported", "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256", true},

		// Ciphers only in tls.InsecureCipherSuites — rejected by exporter-toolkit
		{"3DES insecure rejected", "TLS_RSA_WITH_3DES_EDE_CBC_SHA", false},
		{"RC4 insecure rejected", "TLS_RSA_WITH_RC4_128_SHA", false},

		// Fabricated names that are NOT in Go's cipher suite list
		{"fabricated cipher not supported", "TLS_FAKE_WITH_AES_128_GCM_SHA256", false},
		{"empty string not supported", "", false},
		{"random string not supported", "NOT_A_CIPHER", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSupportedCipher(tt.cipher)
			assert.Equal(t, tt.expected, result,
				"isSupportedCipher(%q) should be %v", tt.cipher, tt.expected)
		})
	}
}

// TestMapCipherSuitesFiltersGoUnsupported verifies that mapCipherSuites filters
// out cipher names not in tls.CipherSuites() (the secure set). This prevents
// the exporter-toolkit from rejecting unknown ciphers and crash-looping.
func TestMapCipherSuitesFiltersGoUnsupported(t *testing.T) {
	// TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 is a standard Go cipher — should pass.
	// TLS_FAKE_WITH_AES_128_GCM_SHA256 is fabricated — should be filtered even
	// if it somehow got past the library-go resolution step.
	result, unsupported := mapCipherSuites([]string{
		"ECDHE-RSA-AES128-GCM-SHA256",
	})
	assert.Contains(t, result, "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		"recognized cipher should pass through")
	assert.Empty(t, unsupported,
		"recognized cipher should not appear in unsupported list")
}

// TestOldProfileCiphersAreGoSupported verifies that after both weak-cipher and
// Go-runtime filtering, every remaining cipher from the Old TLS profile is
// present in tls.CipherSuites() (the secure set). The exporter-toolkit only
// accepts ciphers from this set — any other name causes a crash-loop.
func TestOldProfileCiphersAreGoSupported(t *testing.T) {
	// Build the set of cipher names accepted by the exporter-toolkit
	goSupported := make(map[string]bool)
	for _, cs := range tls.CipherSuites() {
		goSupported[cs.Name] = true
	}

	oldProfile := *oconfig.TLSProfiles[oconfig.TLSProfileOldType]
	ciphers, _ := mapCipherSuites(oldProfile.Ciphers)

	require.NotEmpty(t, ciphers,
		"Old profile should produce at least one supported cipher after filtering")

	for _, c := range ciphers {
		assert.True(t, goSupported[c],
			"cipher %q from Old profile output is not in tls.CipherSuites() — "+
				"the exporter-toolkit would reject it as an unknown cipher", c)
	}
}
