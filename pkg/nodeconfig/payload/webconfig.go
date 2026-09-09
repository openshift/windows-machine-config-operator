package payload

import (
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	oconfig "github.com/openshift/api/config/v1"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	klog "k8s.io/klog/v2"
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

// weakCipherPatterns contains substrings that identify weak cipher suites in
// both OpenSSL and IANA naming conventions. Cipher suites whose IANA name
// contains any of these patterns are filtered out of the generated webconfig.
var weakCipherPatterns = []string{
	"3DES",
	"DES-CBC",
	"DES_CBC",
	"RC4",
	"BLOWFISH",
	"ECB",
	"MD5",
	"NULL",
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
// to the generated payload directory as a compressed .tar.gz file. When
// honorTLSProfile is true, the webconfig includes min_version, cipher_suites,
// and curve_preferences derived from the cluster TLS profile. The returned
// string slice contains any cipher suite names from the profile that could not
// be mapped to Go cipher suites.
//
// Live pickup of webconfig changes by a running windows-exporter relies on
// exporter-toolkit's GetConfigForClient reload: exporter-toolkit ≥ 0.14.0
// re-reads the entire webconfig on every TLS handshake, rebuilding MinVersion,
// CipherSuites, and CurvePreferences from the file on disk. Pre-existing
// keep-alive connections retain the old TLS parameters until the client
// reconnects.
func PopulateWebConfig(tlsProfileSpec oconfig.TLSProfileSpec, honorTLSProfile bool) ([]string, error) {
	content, unsupported := GenerateWebConfig(tlsProfileSpec, honorTLSProfile)
	fileName := strings.TrimSuffix(filepath.Base(TLSConfPath), ".tar.gz")
	shaMap[fileName] = fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	compressedFile, err := os.Create(TLSConfPath)
	if err != nil {
		return unsupported, fmt.Errorf("failed to create webconfig file: %w", err)
	}
	if err := createTarGzFile([]byte(content), fileName, compressedFile); err != nil {
		writeErr := fmt.Errorf("failed to write webconfig tar.gz: %w", err)
		if closeErr := compressedFile.Close(); closeErr != nil {
			return unsupported, errors.Join(writeErr, fmt.Errorf("failed to close webconfig file: %w", closeErr))
		}
		return unsupported, writeErr
	}
	if err := compressedFile.Close(); err != nil {
		return unsupported, fmt.Errorf("failed to close webconfig file: %w", err)
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
	curvePrefs, unsupportedGroups := mapCurvePreferences(tlsProfileSpec.Groups)
	unsupported = append(unsupported, unsupportedGroups...)

	// Only include cipher_suites when min version is below TLS 1.3, as Go's
	// TLS 1.3 implementation does not allow configuring cipher suites.
	includeCiphers := minVersion != "TLS13"

	return generateFullWebConfig(minVersion, cipherSuites, curvePrefs, includeCiphers), unsupported
}

// writeWebConfigHeader writes the common YAML header shared by both the base
// and full webconfig: the tls_server_config key plus cert_file and key_file.
func writeWebConfigHeader(b *strings.Builder) {
	b.WriteString("tls_server_config:\n")
	b.WriteString(fmt.Sprintf("  cert_file: %s\n", webConfigCertFile))
	b.WriteString(fmt.Sprintf("  key_file: %s\n", webConfigKeyFile))
}

// generateBaseWebConfig returns the webconfig YAML with only cert_file and key_file,
// which is the default when the cluster TLS profile is not honoured.
func generateBaseWebConfig() string {
	var b strings.Builder
	writeWebConfigHeader(&b)
	return b.String()
}

// generateFullWebConfig returns the webconfig YAML with TLS profile settings.
func generateFullWebConfig(minVersion string, cipherSuites, curvePrefs []string, includeCiphers bool) string {
	var b strings.Builder
	writeWebConfigHeader(&b)
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
//
// Note: In FIPS builds (X:strictfipsruntime), Go's crypto/tls enforces a
// minimum of TLS 1.2 regardless of the configured min_version value.
// Setting TLS10 or TLS11 here will not actually lower the TLS floor on the
// node.
func mapTLSVersion(v oconfig.TLSProtocolVersion) string {
	if mapped, ok := tlsVersionMap[v]; ok {
		return mapped
	}
	klog.Warningf("unknown TLS version %q, defaulting to TLS12", v)
	return "TLS12"
}

// mapCipherSuites converts cipher names from the OpenShift TLS profile to
// IANA cipher suite names accepted by the windows-exporter webconfig.
// TLS 1.3 cipher suites are filtered out because Go enables them
// unconditionally. Weak cipher suites (DES/3DES, RC4, Blowfish, ECB, MD5,
// SHA-1) are also filtered out. Unsupported and weak cipher names are
// collected and returned separately for logging.
func mapCipherSuites(ciphers []string) ([]string, []string) {
	var result []string
	var unsupported []string
	for _, cipher := range ciphers {
		// Skip TLS 1.3 ciphers -- they are always enabled in Go
		if tls13Ciphers[cipher] {
			continue
		}

		// Determine the IANA name for the cipher
		var ianaName string

		// Try as Go/IANA name directly
		if _, err := libgocrypto.CipherSuite(cipher); err == nil {
			ianaName = cipher
		} else {
			// Try converting from OpenSSL name to IANA
			ianaCiphers := libgocrypto.OpenSSLToIANACipherSuites([]string{cipher})
			if len(ianaCiphers) == 1 {
				if _, err := libgocrypto.CipherSuite(ianaCiphers[0]); err == nil {
					ianaName = ianaCiphers[0]
				}
			}
		}

		if ianaName == "" {
			unsupported = append(unsupported, cipher)
			continue
		}

		// Filter ciphers not recognized by the Go runtime's crypto/tls
		// package. In FIPS builds (X:strictfipsruntime), the available
		// cipher set is restricted and the exporter-toolkit rejects
		// unknown cipher names, causing the windows_exporter to
		// crash-loop.
		if !isSupportedCipher(ianaName) {
			unsupported = append(unsupported, cipher)
			continue
		}

		// Filter weak cipher suites (DES/3DES, RC4, Blowfish, ECB, MD5, SHA-1)
		if isWeakCipher(ianaName) {
			unsupported = append(unsupported, cipher)
			continue
		}

		result = append(result, ianaName)
	}
	return result, unsupported
}

// isWeakCipher returns true if the cipher suite name (in IANA format) indicates
// a weak cryptographic algorithm: DES/3DES, RC4, Blowfish, ECB mode, MD5, or
// SHA-1 MAC. SHA-1 MAC ciphers are identified by IANA names ending in "_SHA"
// (as opposed to "_SHA256" or "_SHA384").
func isWeakCipher(name string) bool {
	upper := strings.ToUpper(name)
	for _, pattern := range weakCipherPatterns {
		if strings.Contains(upper, pattern) {
			return true
		}
	}
	// SHA-1 MAC: IANA names end with "_SHA" (not "_SHA256" or "_SHA384")
	if strings.HasSuffix(upper, "_SHA") {
		return true
	}
	return false
}

// isSupportedCipher returns true if the given IANA cipher suite name is
// recognized by the Go runtime's crypto/tls package. It checks both
// tls.CipherSuites() (secure suites) and tls.InsecureCipherSuites()
// (deprecated but still recognized by Go).
//
// In FIPS builds (X:strictfipsruntime), tls.CipherSuites() returns a
// restricted set and the exporter-toolkit validates cipher names against it.
// Any cipher name not in that set is rejected as "unknown cipher", causing
// the windows_exporter to crash-loop. This function prevents those ciphers
// from being included in the webconfig.
func isSupportedCipher(ianaName string) bool {
	for _, cs := range tls.CipherSuites() {
		if cs.Name == ianaName {
			return true
		}
	}
	for _, cs := range tls.InsecureCipherSuites() {
		if cs.Name == ianaName {
			return true
		}
	}
	return false
}

// mapCurvePreferences converts OpenShift TLSGroup identifiers to the Go
// tls.CurveID string names accepted by the exporter-toolkit webconfig.
// Unsupported groups (e.g. post-quantum X25519MLKEM768) are collected and
// returned separately for logging.
func mapCurvePreferences(groups []oconfig.TLSGroup) ([]string, []string) {
	var result []string
	var unsupported []string
	for _, g := range groups {
		if curveName, ok := groupToCurve[g]; ok {
			result = append(result, curveName)
		} else {
			unsupported = append(unsupported, string(g))
		}
	}
	return result, unsupported
}
