package csr

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certificates "k8s.io/api/certificates/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/pkg/instance"
)

const (
	goodCSR = `
Certificate Request:
    Data:
        Version: 1 (0x0)
        Subject: O = system:nodes, CN = system:node:test
...
        Requested Extensions:
            X509v3 Subject Alternative Name:
                DNS:node1, DNS:node1.local, IP Address:10.0.0.1, IP Address:127.0.0.1
...
-----BEGIN CERTIFICATE REQUEST-----
MIICszCCAZsCAQAwMjEVMBMGA1UEChMMc3lzdGVtOm5vZGVzMRkwFwYDVQQDExBz
eXN0ZW06bm9kZTp0ZXN0MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA
ukG4TvbrMbVklA2nLmK0T7+SygWRYebsd0vJMWkw87+zxkYY0tEo+y5ijHXucb1S
3m4mGulmzxP1KQI/0RDuba1HhekAaOxy2TZWYhtQUxCHbrREz3b+OBbDkf2Dzp7Q
o6J3l7fYBRCD/AnTzSCaK5LwzmH0X3TCJnrLBIf8gFrqAHsCXadNV3JQ2Iip6Gjs
8VCqnZHS/oFhXpKiMnrB0IMpC6F21/T4Uoe+vyWoUTZQTAjZVBcIDLp3r8c6FnmF
5YjouWafNVfbttVczNpuSt/3YxXLb2P/EQfb8QniNUXnkxSNwOpZx6QO2PZHSSBW
cW+q+EUeFXsInl41dK5avwIDAQABoDwwOgYJKoZIhvcNAQkOMS0wKzApBgNVHREE
IjAgggVub2RlMYILbm9kZTEubG9jYWyHBAoAAAGHBH8AAAEwDQYJKoZIhvcNAQEL
BQADggEBAEFFAuuhgUGs7Mhg9hMdj8csuBiLHUah5bkavvi/dwH3CaHpXRAxMwRI
0K+puuDsHn7Y7xInO2IfyYVaZ6Xr2ppT9u0Hjn9DzN3Wmd/ngTWbWsctvXVMkGw4
Mkc4v7oq9wBbMDbsT3xKaRqWvxqAsD3NXUVGW4tIJhqZnKk3QtZ70p/q4L4/TbEV
yOf1lhGA26sAJX4gMeTHUxPu85NedLzTg5DYDyPPvIYPKw7ww8tm2fYb67sr21WU
p1VlUzB7qtkVJ4coGNFPwl7vu3rps5VPN7ONV9JG8+PVvjxhyQD5ZBqLVPbT7ZGI
NKbWRRtEF/XLPoZs3kq95YCgn2oQ9ws=
-----END CERTIFICATE REQUEST-----
`

	emptyCSR = `
Certificate Request:
    Data:
        Version: 1 (0x0)
        Subject: O = system:nodes, CN = system:node:test
...
        Requested Extensions:
            X509v3 Subject Alternative Name:
                DNS:node1, DNS:node1.local, IP Address:10.0.0.1, IP Address:127.0.0.1
...
-----BEGIN??
`
	invalidCSR = `
Certificate Request:
    Data:
        Version: 1 (0x0)
        Subject: O = system:nodes, CN = system:node:test
...
        Requested Extensions:
            X509v3 Subject Alternative Name:
                DNS:node1, DNS:node1.local, IP Address:10.0.0.1, IP Address:127.0.0.1
...
-----BEGIN CERTIFICATE REQUEST-----
MIICszCCAZsCAQAwMjEVMBMGA1UEChMMc3lzdGVtOm5vZGVzMRkwFwYDVQQDExBz
eXN0ZW06bm9kZTp0ZXN0MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA
ukG4TvbrMbVklA2nLmK0T7+SygWRYebsd0vJMWkw87+zxkYY0tEo+y5ijHXucb1S
3m4mGulmzxP1KQI/0RDuba1HhekAaOxy2TZWYhtQUxCHbrREz3b+OBbDkf2Dzp7Q
o6J3l7fYBRCD/AnTzSCaK5LwzmH0X3TCJnrLBIf8gFrqAHsCXadNV3JQ2Iip6Gjs
8VCqnZHS/oFhXpKiMnrB0IMpC6F21/T4Uoe+vyWoUTZQTAjZVBcIDLp3r8c6FnmF
5YjouWafNVfbttVczNpuSt/3YxXLb2P/EQfb8QniNUXnkxSNwOpZx6QO2PZHSSBW
cW+q+EUeFXsInl41dK5avwIDAQABoDwwOgYJKoZIhvcNAQkOMS0wKzApBgNVHREE
IjAgggVub2RlMYILbm9kZTEubG9jYWyHBAoAAAGHBH8AAAEwDQYJKoZIhvcNAQEL
BQADggEBAEFFAuuhgUGs7Mhg9hMdj8csuBiLHUah5bkavvi/dwH3CaHpXRAxMwRI
0K+puuDsHn7Y7xInO2IfyYVaZ6Xr2ppT9u0Hjn9DzN3Wmd/ngTWbWsctvXVMkGw4
Mkc4v7oq9wBbMDbsT3xKaRqWvxqAsD3NXUVGW4tIJhqZnKk3QtZ70p/q4L4/TbEV
yOf1lhGA26sAJX4gMeTHUxPu85NedLzTg5DYDyPPvIYPKw7ww8tm2fYb67sr21WU
p1VlUzB7qtkVJ4coGNFPwl7vu3rps5VPN7ONV9JG8+PVvjxhyQD5ZBqLVPbT7ZGI
`
)

func TestParseCSR(t *testing.T) {

	testCases := []struct {
		name        string
		input       []byte
		expectedErr bool
	}{
		{
			name:        "valid CSR",
			input:       []byte(goodCSR),
			expectedErr: false,
		},
		{
			name:        "empty CSR",
			input:       []byte(emptyCSR),
			expectedErr: true,
		},
		{
			name:        "invalid CSR",
			input:       []byte(invalidCSR),
			expectedErr: true,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := ParseCSR(test.input)
			if test.expectedErr {
				assert.Error(t, err)
				assert.Nil(t, out)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, out)
		})
	}
}

// newTestKubeletServingCSR returns a CertificateSigningRequest with the groups and usages required
// to pass the checks in validateKubeletServingCSR that precede the subject organization validation.
func newTestKubeletServingCSR() *certificates.CertificateSigningRequest {
	return &certificates.CertificateSigningRequest{
		ObjectMeta: meta.ObjectMeta{Name: "test-csr"},
		Spec: certificates.CertificateSigningRequestSpec{
			Groups: []string{nodeGroup, systemPrefix},
			Usages: kubeletServerUsages,
		},
	}
}

// TestValidateKubeletServingCSROrganizationMatch ensures validateKubeletServingCSR requires an
// exact single-org match, mirroring the kubelet client path in isNodeClientCert().
func TestValidateKubeletServingCSROrganizationMatch(t *testing.T) {
	testCases := []struct {
		name        string
		orgs        []string
		expectedErr bool
	}{
		{
			name:        "exact match passes",
			orgs:        []string{nodeGroup},
			expectedErr: false,
		},
		{
			name:        "extra org system:masters rejected",
			orgs:        []string{nodeGroup, "system:masters"},
			expectedErr: true,
		},
		{
			name:        "single org cluster-admin rejected",
			orgs:        []string{"cluster-admin"},
			expectedErr: true,
		},
		{
			name:        "reversed org order rejected",
			orgs:        []string{"system:masters", nodeGroup},
			expectedErr: true,
		},
		{
			name:        "empty org rejected",
			orgs:        []string{},
			expectedErr: true,
		},
		{
			name:        "nil org rejected",
			orgs:        nil,
			expectedErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Approver{csr: newTestKubeletServingCSR()}
			parsedCSR := &x509.CertificateRequest{
				Subject: pkix.Name{
					CommonName:   nodeUserName + ":test-node",
					Organization: tc.orgs,
				},
			}
			err := a.validateKubeletServingCSR(parsedCSR)
			if tc.expectedErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "does not contain required subject organization")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAddressesEqual(t *testing.T) {

	testCases := []struct {
		name        string
		nodeName    string
		instances   []*instance.Info
		output      bool
		expectedErr bool
	}{
		{
			name:        "instance IP matches node name",
			nodeName:    "localhost",
			instances:   []*instance.Info{{Address: "127.0.0.1", Username: "username=core"}},
			output:      true,
			expectedErr: false,
		},
		{
			name:        "instance DNS matches node name",
			nodeName:    "localhost",
			instances:   []*instance.Info{{Address: "localhost", Username: "username=core"}},
			output:      true,
			expectedErr: false,
		},
		{
			name:        "instance DNS not matching node name",
			nodeName:    "localhost",
			instances:   []*instance.Info{{Address: "invalid", Username: "username=core"}},
			output:      false,
			expectedErr: false,
		},
		{
			name:        "instance IP not matching node name",
			nodeName:    "newhost",
			instances:   []*instance.Info{{Address: "127.0.0.1", Username: "username=core"}},
			output:      false,
			expectedErr: false,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out, err := matchesDNS(test.nodeName, test.instances)
			if test.expectedErr {
				assert.Error(t, err)
				assert.False(t, out)
				return
			}
			require.NoError(t, err)
			if test.output {
				assert.True(t, out)
			}
		})
	}
}
