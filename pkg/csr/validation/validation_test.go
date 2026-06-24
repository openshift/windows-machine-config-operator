package validation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func generateTestCSRPEM(t *testing.T, cn string, orgs []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: orgs,
		},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csrDER,
	})
}

func TestValidateCertificateContentOrganizationMatch(t *testing.T) {
	testCases := []struct {
		name        string
		orgs        []string
		certType    CertificateType
		expectError bool
		errContains string
	}{
		{
			name:        "exact match for WICD type passes",
			orgs:        []string{"system:wicd-nodes"},
			certType:    WICDCertType,
			expectError: false,
		},
		{
			name:        "exact match for kubelet-client type passes",
			orgs:        []string{NodeGroup},
			certType:    KubeletClientCertType,
			expectError: false,
		},
		{
			name:        "exact match for kubelet-serving type passes",
			orgs:        []string{NodeGroup},
			certType:    KubeletServingCertType,
			expectError: false,
		},
		{
			name:        "extra org system:masters rejected",
			orgs:        []string{"system:wicd-nodes", "system:masters"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "extra org cluster-admin rejected",
			orgs:        []string{"system:wicd-nodes", "cluster-admin"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "single org cluster-admin rejected",
			orgs:        []string{"cluster-admin"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid subject organization in CSR content",
		},
		{
			name:        "extra org system:nodes rejected for WICD",
			orgs:        []string{"system:wicd-nodes", "system:nodes"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "extra arbitrary org rejected",
			orgs:        []string{"system:wicd-nodes", "my-custom-group"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "missing required org rejected",
			orgs:        []string{"system:masters"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid subject organization",
		},
		{
			name:        "empty org rejected",
			orgs:        []string{},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "nil org rejected",
			orgs:        nil,
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "only system:masters rejected for kubelet-client",
			orgs:        []string{"system:masters"},
			certType:    KubeletClientCertType,
			expectError: true,
			errContains: "invalid subject organization",
		},
		{
			name:        "extra org on kubelet-serving rejected",
			orgs:        []string{NodeGroup, "system:masters"},
			certType:    KubeletServingCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
		{
			name:        "reversed org order rejected",
			orgs:        []string{"system:masters", "system:wicd-nodes"},
			certType:    WICDCertType,
			expectError: true,
			errContains: "invalid number of organizations",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			csrPEM := generateTestCSRPEM(t, tc.certType.UserPrefix+":test-node", tc.orgs)
			parsedCSR, err := ParseCSR(csrPEM)
			require.NoError(t, err)

			validator := NewCSRValidator(nil, tc.certType)
			err = validator.validateCertificateContent(parsedCSR)
			if tc.expectError {
				assert.Error(t, err)
				if tc.errContains != "" {
					assert.Contains(t, err.Error(), tc.errContains)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
