package payload

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	oconfig "github.com/openshift/api/config/v1"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
)

// tls13Ciphers are TLS 1.3 cipher suites that Go always enables.
// They must not appear in the webconfig cipher_suites field because Go's
// crypto/tls ignores CipherSuites when MinVersion is TLS 1.3, and the
// exporter-toolkit follows the same behaviour.
var tls13Ciphers = map[string]bool{
	"TLS_AES_128_GCM_SHA256":       true,
	"TLS_AES_256_GCM_SHA384":       true,
	"TLS_CHACHA20_POLY1305_SHA256": true,
}

// tlsVersionMap maps OpenShift TLS protocol version identifiers to the short
// form expected by the Prometheus exporter-toolkit webconfig (Go crypto/tls).
var tlsVersionMap = map[oconfig.TLSProtocolVersion]string{
	oconfig.VersionTLS10: "TLS10",
	oconfig.VersionTLS11: "TLS11",
	oconfig.VersionTLS12: "TLS12",
	oconfig.VersionTLS13: "TLS13",
}

// groupToCurve maps OpenShift TLSGroup identifiers to the Go tls.CurveID
// string names accepted by the exporter-toolkit webconfig.
var groupToCurve = map[oconfig.TLSGroup]string{
	oconfig.TLSGroupX25519:    "X25519",
	oconfig.TLSGroupSecP256r1: "CurveP256",
	oconfig.TLSGroupSecP384r1: "CurveP384",
	oconfig.TLSGroupSecP521r1: "CurveP521",
}

// webConfigCertFile is the Windows path to the TLS certificate used by windows-exporter
const webConfigCertFile = `C:\\k\\tls\\certs\\tls.crt`

// webConfigKeyFile is the Windows path to the TLS private key used by windows-exporter
const webConfigKeyFile = `C:\\k\\tls\\certs\\tls.key`

// PopulateWebConfig generates the windows-exporter webconfig YAML and writes it
// to the payload as a compressed .tar.gz file. When honorTLSProfile is true, the
// webconfig includes min_version, cipher_suites, and curve_preferences derived
// from the cluster TLS profile. The returned string slice contains any cipher suite
// names from the profile that could not be mapped to Go cipher suites.
func PopulateWebConfig(tlsProfileSpec oconfig.TLSProfileSpec, honorTLSProfile bool) ([]string, error) {
	content, unsupported := GenerateWebConfig(tlsProfileSpec, honorTLSProfile)
	fileName := strings.TrimSuffix(filepath.Base(TLSConfPath), ".tar.gz")
	shaMap[fileName] = fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	compressedFile, err := os.Create(TLSConfPath)
	if err != nil {
		return unsupported, fmt.Errorf("failed to create webconfig file: %w", err)
	}
	defer compressedFile.Close()
	if err := createTarGzFile([]byte(content), fileName, compressedFile); err != nil {
		return unsupported, fmt.Errorf("failed to write webconfig tar.gz: %w", err)
	}
	return unsupported, nil
}

// GenerateWebConfig returns the windows-exporter webconfig YAML content and a
// list of cipher suite names that were not supported by Go (for logging).
func GenerateWebConfig(tlsProfileSpec oconfig.TLSProfileSpec, honorTLSProfile bool) (string, []string) {
	if !honorTLSProfile {
		return generateBaseWebConfig(), nil
	}

	minVersion := mapTLSVersion(tlsProfileSpec.MinTLSVersion)
	cipherSuites, unsupported := mapCipherSuites(tlsProfileSpec.Ciphers)
	curvePrefs := mapCurvePreferences(tlsProfileSpec.Groups)

	// Only include cipher_suites when min version is below TLS 1.3, as Go's
	// TLS 1.3 implementation does not allow configuring cipher suites.
	includeCiphers := minVersion != "TLS13"

	return generateFullWebConfig(minVersion, cipherSuites, curvePrefs, includeCiphers), unsupported
}

// generateBaseWebConfig returns the webconfig YAML with only cert_file and key_file,
// which is the default when the cluster TLS profile is not honoured.
func generateBaseWebConfig() string {
	var b strings.Builder
	b.WriteString("tls_server_config:\n")
	b.WriteString(fmt.Sprintf("  cert_file: %s\n", webConfigCertFile))
	b.WriteString(fmt.Sprintf("  key_file: %s\n", webConfigKeyFile))
	return b.String()
}

// generateFullWebConfig returns the webconfig YAML with TLS profile settings.
func generateFullWebConfig(minVersion string, cipherSuites, curvePrefs []string, includeCiphers bool) string {
	var b strings.Builder
	b.WriteString("tls_server_config:\n")
	b.WriteString(fmt.Sprintf("  cert_file: %s\n", webConfigCertFile))
	b.WriteString(fmt.Sprintf("  key_file: %s\n", webConfigKeyFile))
	b.WriteString(fmt.Sprintf("  min_version: %s\n", minVersion))

	if includeCiphers && len(cipherSuites) > 0 {
		b.WriteString("  cipher_suites:\n")
		for _, cs := range cipherSuites {
			b.WriteString(fmt.Sprintf("    - %s\n", cs))
		}
	}

	if len(curvePrefs) > 0 {
		b.WriteString("  curve_preferences:\n")
		for _, cp := range curvePrefs {
			b.WriteString(fmt.Sprintf("    - %s\n", cp))
		}
	}

	return b.String()
}

// mapTLSVersion converts an OpenShift TLSProtocolVersion to the short form
// used by the Go crypto/tls package and the Prometheus exporter-toolkit
// webconfig (e.g. "VersionTLS12" -> "TLS12"). Returns "TLS12" for unknown
// values as a safe default.
func mapTLSVersion(v oconfig.TLSProtocolVersion) string {
	if mapped, ok := tlsVersionMap[v]; ok {
		return mapped
	}
	return "TLS12"
}

// mapCipherSuites converts cipher names from the OpenShift TLS profile to
// IANA cipher suite names accepted by the windows-exporter webconfig.
// TLS 1.3 cipher suites are filtered out because Go enables them
// unconditionally. Unsupported cipher names are collected and returned
// separately for logging.
func mapCipherSuites(ciphers []string) ([]string, []string) {
	var result []string
	var unsupported []string
	for _, cipher := range ciphers {
		// Skip TLS 1.3 ciphers -- they are always enabled in Go
		if tls13Ciphers[cipher] {
			continue
		}

		// Try as Go/IANA name directly
		if _, err := libgocrypto.CipherSuite(cipher); err == nil {
			result = append(result, cipher)
			continue
		}

		// Try converting from OpenSSL name to IANA
		ianaCiphers := libgocrypto.OpenSSLToIANACipherSuites([]string{cipher})
		if len(ianaCiphers) == 1 {
			ianaName := ianaCiphers[0]
			if _, err := libgocrypto.CipherSuite(ianaName); err == nil {
				result = append(result, ianaName)
				continue
			}
		}
		unsupported = append(unsupported, cipher)
	}
	return result, unsupported
}

// mapCurvePreferences converts OpenShift TLSGroup identifiers to the Go
// tls.CurveID string names accepted by the exporter-toolkit webconfig.
// Unsupported groups (e.g. post-quantum X25519MLKEM768) are silently skipped.
func mapCurvePreferences(groups []oconfig.TLSGroup) []string {
	var result []string
	for _, g := range groups {
		if curveName, ok := groupToCurve[g]; ok {
			result = append(result, curveName)
		}
	}
	return result
}
