package nodeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
)

func TestCreateBootstrapKubeconfig(t *testing.T) {
	testCases := []struct {
		name         string
		secret       *core.Secret
		expectedSpec string
		expectedErr  bool
	}{
		{
			name: "real secret data",
			secret: &core.Secret{
				Data: map[string][]byte{
					core.ServiceAccountRootCAKey: []byte("-----BEGIN%20CERTIFICATE-----%0AMIIDCTCCAfGgAwIBAgIBADANBgkqhkiG9w0BAQsFADAmMRIwEAYDVQQLEwlvcGVu%0Ac2hpZnQxEDAOBgNVBAMTB3Jvb3QtY2EwHhcNMTgxMDI0MTc0NjE0WhcNMjgxMDIx%0AMTc0NjE0WjAmMRIwEAYDVQQLEwlvcGVuc2hpZnQxEDAOBgNVBAMTB3Jvb3QtY2Ew%0AggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEKAoIBAQCv8EgOZ%2BvexDJkpmEPuIVv%0ACJtvaJ9TEgpD4d0mN1N%2F2g0GWWP1sNM8lxztyA3mhahNkHLAYRScYjURKlaarXgo%0A0%2BnM2rEkkECn4o7TAetHmBd2%2FFgV3peTucVRIWV801QZMmP9vwCa4yPi2L8Ez37k%0A2RpepeeSVIvHARz7%2BHbMHu5cXauPRazSFko05P2y0VgvdhRzX6zm8DjppLQIHqTH%0AkvsIwEXwsQ8GjUnlqnYhDnI%2F1sTG3SVR3%2FbCobiq5N2JH9wKIfIt89KbNPfE7eH1%0AcTcsS1adPMnAVrviEYk9ukebd3pc9gDFUbxhEJLnMo815sy9O%2FyyrPG%2F3Xfjfn4Z%0AAgMBAAGjQjBAMA4GA1UdDwEB%2FwQEAwICpDAPBgNVHRMBAf8EBTADAQH%2FMB0GA1Ud%0ADgQWBBRRKkS2ZLQotJ2ft4o%2B1xf7hrM17DANBgkqhkiG9w0BAQsFAAOCAQEAj72Y%0AHILMf59%2Bcq%2BkHcwizFJk5dj%2FQaN5Bwe0wT1n%2FjneyV2ISzIC5NVbwcnP2DgZWVOT%0ArxA%2BIBuKH%2FXbjzaDpahgtnK1yqObjSAzsdz7DdstdpriqD0YjBQg23d5idrwyEep%0AF7%2FvdTfWjAZkDrszOCr%2BjWsrsCLUDiBf43u1B9RuuqCsl1bFVAHCK7Gj2cMBXJHd%0AjC4%2BOaZY4TUhmSZIi1nyiie79jMKRFiHtM1P%2BERljT4899faGoGbEHDlYn75HvQA%0AM1Yif0VCtzi%2B6xnKDZ5O3wvxctQTtmb9ayL11d1GT%2FOrM9II0UAtodIjpxBo%2BY7n%0Au4k%2BQSXwlOfqDSixwA%3D%3D%0A-----END%20CERTIFICATE-----%0A"),
					core.ServiceAccountTokenKey:  []byte("AAAAB3NzaC1yc2EAAAADAQABAAACAQDaJEjsx1sOoRh+vT3UXIspVBlnqswPFXUv4DeOwA9iM/++CrIqRZQNge760WzBOLXMq6J6FHw/w5seim3oCpdFa54JmZsJMFBx8u56Hg9JZdjtKk36kpSvu53Moiit8/aTQyzKZZht+n+xxaby7b4XXM/MCNhvHERsbX36LLlK1BU+45037ePGmCwgjk+rHUjHHWHFhPG7mtXIiI/7CMt5mV03CkxflryU9iWNkYWl4vgy6NaYolfAgdlX4ryvEjBKx6HywllQsmK4AmoQyAErwKvurWrHn1zFellNBK5r2vvMTpPufdJN7yDERmFeOsucxZzvpESu5jVys1PFUJmyA8W0h3Xbq4IrxTGm0xhYmt2HNWQ8i840jD7ZKVq6RIpeiVOn60Ha0pVDBZfyWMOVZAeF3mV8g8gI9WkJDJHXkwhOZn5P3D7sv3ABuI3u3xzXQiLQgpAEuZzMB1NwDR8Rz9csRfxRHd1Nl6oYOGT583qPpBQsXrTI9kINpCAVDH7k+qS+IwCIq6soczUUYgXNWCafwdiqPSZOZCOnnTmJ9+SLFHAaa4sHlEhdekfPND4riKYIpZwlsU87aN2tGWZDcRPX/6di3opTx5B68izGKBWgKgF5XcpSH5e2dSqhnA1QjLMUDIZZ0hMIC1HjGTY6mkHXsS3FD79vKi+SX8jTLw=="),
				},
			},
			expectedSpec: "{\"preferences\":{},\"clusters\":[{\"name\":\"local\",\"cluster\":{\"server\":\"\",\"certificate-authority-data\":\"LS0tLS1CRUdJTiUyMENFUlRJRklDQVRFLS0tLS0lMEFNSUlEQ1RDQ0FmR2dBd0lCQWdJQkFEQU5CZ2txaGtpRzl3MEJBUXNGQURBbU1SSXdFQVlEVlFRTEV3bHZjR1Z1JTBBYzJocFpuUXhFREFPQmdOVkJBTVRCM0p2YjNRdFkyRXdIaGNOTVRneE1ESTBNVGMwTmpFMFdoY05Namd4TURJeCUwQU1UYzBOakUwV2pBbU1SSXdFQVlEVlFRTEV3bHZjR1Z1YzJocFpuUXhFREFPQmdOVkJBTVRCM0p2YjNRdFkyRXclMEFnZ0VpTUEwR0NTcUdTSWIzRFFFQkFRVUFBNElCRHdBd2dnRUtBb0lCQVFDdjhFZ09aJTJCdmV4REprcG1FUHVJVnYlMEFDSnR2YUo5VEVncEQ0ZDBtTjFOJTJGMmcwR1dXUDFzTk04bHh6dHlBM21oYWhOa0hMQVlSU2NZalVSS2xhYXJYZ28lMEEwJTJCbk0yckVra0VDbjRvN1RBZXRIbUJkMiUyRkZnVjNwZVR1Y1ZSSVdWODAxUVpNbVA5dndDYTR5UGkyTDhFejM3ayUwQTJScGVwZWVTVkl2SEFSejclMkJIYk1IdTVjWGF1UFJhelNGa28wNVAyeTBWZ3ZkaFJ6WDZ6bThEanBwTFFJSHFUSCUwQWt2c0l3RVh3c1E4R2pVbmxxblloRG5JJTJGMXNURzNTVlIzJTJGYkNvYmlxNU4ySkg5d0tJZkl0ODlLYk5QZkU3ZUgxJTBBY1Rjc1MxYWRQTW5BVnJ2aUVZazl1a2ViZDNwYzlnREZVYnhoRUpMbk1vODE1c3k5TyUyRnl5clBHJTJGM1hmamZuNFolMEFBZ01CQUFHalFqQkFNQTRHQTFVZER3RUIlMkZ3UUVBd0lDcERBUEJnTlZIUk1CQWY4RUJUQURBUUglMkZNQjBHQTFVZCUwQURnUVdCQlJSS2tTMlpMUW90SjJmdDRvJTJCMXhmN2hyTTE3REFOQmdrcWhraUc5dzBCQVFzRkFBT0NBUUVBajcyWSUwQUhJTE1mNTklMkJjcSUyQmtIY3dpekZKazVkaiUyRlFhTjVCd2Uwd1QxbiUyRmpuZXlWMklTeklDNU5WYndjblAyRGdaV1ZPVCUwQXJ4QSUyQklCdUtIJTJGWGJqemFEcGFoZ3RuSzF5cU9ialNBenNkejdEZHN0ZHByaXFEMFlqQlFnMjNkNWlkcnd5RWVwJTBBRjclMkZ2ZFRmV2pBWmtEcnN6T0NyJTJCaldzcnNDTFVEaUJmNDN1MUI5UnV1cUNzbDFiRlZBSENLN0dqMmNNQlhKSGQlMEFqQzQlMkJPYVpZNFRVaG1TWklpMW55aWllNzlqTUtSRmlIdE0xUCUyQkVSbGpUNDg5OWZhR29HYkVIRGxZbjc1SHZRQSUwQU0xWWlmMFZDdHppJTJCNnhuS0RaNU8zd3Z4Y3RRVHRtYjlheUwxMWQxR1QlMkZPck05SUkwVUF0b2RJanB4Qm8lMkJZN24lMEF1NGslMkJRU1h3bE9mcURTaXh3QSUzRCUzRCUwQS0tLS0tRU5EJTIwQ0VSVElGSUNBVEUtLS0tLSUwQQ==\"}}],\"users\":[{\"name\":\"kubelet\",\"user\":{\"token\":\"AAAAB3NzaC1yc2EAAAADAQABAAACAQDaJEjsx1sOoRh+vT3UXIspVBlnqswPFXUv4DeOwA9iM/++CrIqRZQNge760WzBOLXMq6J6FHw/w5seim3oCpdFa54JmZsJMFBx8u56Hg9JZdjtKk36kpSvu53Moiit8/aTQyzKZZht+n+xxaby7b4XXM/MCNhvHERsbX36LLlK1BU+45037ePGmCwgjk+rHUjHHWHFhPG7mtXIiI/7CMt5mV03CkxflryU9iWNkYWl4vgy6NaYolfAgdlX4ryvEjBKx6HywllQsmK4AmoQyAErwKvurWrHn1zFellNBK5r2vvMTpPufdJN7yDERmFeOsucxZzvpESu5jVys1PFUJmyA8W0h3Xbq4IrxTGm0xhYmt2HNWQ8i840jD7ZKVq6RIpeiVOn60Ha0pVDBZfyWMOVZAeF3mV8g8gI9WkJDJHXkwhOZn5P3D7sv3ABuI3u3xzXQiLQgpAEuZzMB1NwDR8Rz9csRfxRHd1Nl6oYOGT583qPpBQsXrTI9kINpCAVDH7k+qS+IwCIq6soczUUYgXNWCafwdiqPSZOZCOnnTmJ9+SLFHAaa4sHlEhdekfPND4riKYIpZwlsU87aN2tGWZDcRPX/6di3opTx5B68izGKBWgKgF5XcpSH5e2dSqhnA1QjLMUDIZZ0hMIC1HjGTY6mkHXsS3FD79vKi+SX8jTLw==\"}}],\"contexts\":[{\"name\":\"kubelet\",\"context\":{\"cluster\":\"local\",\"user\":\"kubelet\"}}],\"current-context\":\"kubelet\"}",
			expectedErr:  false,
		},
		{
			name: "empty secret data",
			secret: &core.Secret{
				Data: map[string][]byte{
					core.ServiceAccountRootCAKey: []byte(""),
					core.ServiceAccountTokenKey:  []byte(""),
				},
			},
			expectedSpec: "{\"preferences\":{},\"clusters\":[{\"name\":\"local\",\"cluster\":{\"server\":\"\"}}],\"users\":[{\"name\":\"kubelet\",\"user\":{}}],\"contexts\":[{\"name\":\"kubelet\",\"context\":{\"cluster\":\"local\",\"user\":\"kubelet\"}}],\"current-context\":\"kubelet\"}",
			expectedErr:  false,
		},
		{
			name: "missing CA in secret",
			secret: &core.Secret{
				Data: map[string][]byte{
					core.ServiceAccountTokenKey: []byte("test"),
				},
			},
			expectedSpec: "",
			expectedErr:  true,
		},
		{
			name: "missing token in secret",
			secret: &core.Secret{
				Data: map[string][]byte{
					core.ServiceAccountRootCAKey: []byte(""),
				},
			},
			expectedSpec: "",
			expectedErr:  true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			actualSpec, err := createBootstrapKubeconfig(test.secret)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedSpec, actualSpec)
		})
	}
}

func TestCreateKubeletConf(t *testing.T) {
	testCases := []struct {
		name         string
		cidr         string
		expectedSpec string
		expectedErr  bool
	}{
		{
			name:         "valid cidr",
			cidr:         "10.0.128.8/24",
			expectedSpec: "{\"kind\":\"KubeletConfiguration\",\"apiVersion\":\"kubelet.config.k8s.io/v1beta1\",\"syncFrequency\":\"0s\",\"fileCheckFrequency\":\"0s\",\"httpCheckFrequency\":\"0s\",\"rotateCertificates\":true,\"serverTLSBootstrap\":true,\"authentication\":{\"x509\":{\"clientCAFile\":\"C:\\\\k\\\\kubelet-ca.crt\"},\"webhook\":{\"cacheTTL\":\"0s\"},\"anonymous\":{\"enabled\":false}},\"authorization\":{\"webhook\":{\"cacheAuthorizedTTL\":\"0s\",\"cacheUnauthorizedTTL\":\"0s\"}},\"clusterDomain\":\"cluster.local\",\"clusterDNS\":[\"10.0.128.10\"],\"streamingConnectionIdleTimeout\":\"0s\",\"nodeStatusUpdateFrequency\":\"0s\",\"nodeStatusReportFrequency\":\"0s\",\"imageMinimumGCAge\":\"0s\",\"volumeStatsAggPeriod\":\"0s\",\"cgroupsPerQOS\":false,\"cpuManagerReconcilePeriod\":\"0s\",\"runtimeRequestTimeout\":\"10m0s\",\"maxPods\":250,\"kubeAPIQPS\":50,\"kubeAPIBurst\":100,\"serializeImagePulls\":false,\"evictionPressureTransitionPeriod\":\"0s\",\"featureGates\":{\"CSIMigrationAzureFile\":false,\"CSIMigrationvSphere\":false,\"LegacyNodeRoleBehavior\":false,\"NodeDisruptionExclusion\":true,\"RotateKubeletServerCertificate\":true,\"SCTPSupport\":true,\"ServiceNodeExclusion\":true,\"SupportPodPidsLimit\":true},\"memorySwap\":{},\"containerLogMaxSize\":\"50Mi\",\"systemReserved\":{\"cpu\":\"500m\",\"ephemeral-storage\":\"1Gi\",\"memory\":\"1Gi\"},\"logging\":{\"flushFrequency\":0,\"verbosity\":0,\"options\":{\"json\":{\"infoBufferSize\":\"0\"}}},\"shutdownGracePeriod\":\"0s\",\"shutdownGracePeriodCriticalPods\":\"0s\",\"enforceNodeAllocatable\":[]}",
			expectedErr:  false,
		},
		{
			name:         "empty cidr",
			cidr:         "",
			expectedSpec: "",
			expectedErr:  true,
		},
		{
			name:         "invalid cidr",
			cidr:         "172.30.0.0",
			expectedSpec: "",
			expectedErr:  true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			actualSpec, err := createKubeletConf(test.cidr)
			if test.expectedErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedSpec, actualSpec)
		})
	}
}
