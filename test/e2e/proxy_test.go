package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	// userCABundleName is the name of the ConfigMap that holds additional user-provided proxy certs
	userCABundleName      = "user-ca-bundle"
	userCABundleNamespace = "openshift-config"
)

// noProxyList is a list of hostnames and/or CIDRs and/or IPs for which the proxy should not be used in the test env
var noProxyList = []string{"google.com", "redhat.io", "static.redhat.com"}

// proxyTestSuite contains the validation cases for cluster-wide proxy.
// All subtests are skipped if a proxy is not enabled in the test environment.
func proxyTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	proxyEnabled, err := tc.client.ProxyEnabled()
	require.NoErrorf(t, err, "error checking if proxy is enabled in test environment")
	if !proxyEnabled {
		t.Skip("cluster-wide proxy is not enabled in this environment")
	}

	// Enables proxy test suite to be run individually on existing Windows nodes
	require.NoError(t, tc.loadExistingNodes())

	t.Run("Trusted CA ConfigMap validation", tc.testTrustedCAConfigMap)
	// Certificate validation test must run before environment variables removal validation since the latter results in
	// the deletion of TrustedCAConfigMap, which the former relies on
	t.Run("Certificate validation", tc.testCertsImport)
	t.Run("Environment variables validation", tc.testEnvVars)

	t.Run("External requests respect global proxy", tc.testProxyRequests)

	t.Run("Certificate validation removal", tc.testCertsRemoval)
	t.Run("Environment variables removal validation", tc.testEnvVarRemoval)
}

// testProxyRequests tests that external requests from each Windows node properly go through the proxy
// and circumvent the proxy if the target address is specified in the NO_PROXY list
func (tc *testContext) testProxyRequests(t *testing.T) {
	clusterProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster", meta.GetOptions{})
	require.NoError(t, err)

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			t.Run("HTTP_PROXY request", func(t *testing.T) {
				httpUrl := "http://raw.githubusercontent.com/openshift/windows-machine-config-operator/master/README.md"
				out, err := tc.curlHeadersOnly(addr, httpUrl)
				require.NoErrorf(t, err, "unable to get curl response to URL %s", httpUrl)

				httpProxyVarInUse, noProxyVarInUse, proxyAuthHeaderInUse := verifyCurlOutput(out,
					clusterProxy.Status.HTTPProxy, clusterProxy.Status.NoProxy)

				assert.Truef(t, httpProxyVarInUse, "expected http_proxy %s in request", clusterProxy.Status.HTTPProxy)
				assert.Truef(t, noProxyVarInUse, "expected no_proxy %s in request", clusterProxy.Status.NoProxy)
				assert.True(t, proxyAuthHeaderInUse, "Proxy-Authorization expected for proxied request")
			})

			t.Run("NO_PROXY request", func(t *testing.T) {
				// using an endpoint that appears in the noProxyList const
				nonProxiedUrl := noProxyList[0]
				out, err := tc.curlHeadersOnly(addr, nonProxiedUrl)
				require.NoErrorf(t, err, "unable to get curl response to URL %s", nonProxiedUrl)

				httpProxyVarInUse, noProxyVarInUse, proxyAuthHeaderInUse := verifyCurlOutput(out,
					clusterProxy.Status.HTTPProxy, clusterProxy.Status.NoProxy)

				assert.False(t, httpProxyVarInUse, "http_proxy not expected in request to no_proxy URL")
				assert.Truef(t, noProxyVarInUse, "expected no_proxy %s in request", clusterProxy.Status.NoProxy)
				assert.False(t, proxyAuthHeaderInUse, "Proxy-Authorization header not expected for no_proxy request")
			})
		})
	}
}

// curlHeadersOnly runs curl through Windows CMD to get all request headers and trace any redirects
/*
Example output:
* Uses proxy env variable no_proxy == '.cluster.local,.svc,10.128.0.0/14,10.94.72.128/25,127.0.0.1,172.30.0.0/16,api-int.xxxxxxxxxxxxxx.vmc-ci.devcluster.openshift.com,google.com,localhost,redhat.io,static.redhat.com'
* Uses proxy env variable http_proxy == 'http://XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX@10.94.72.159:3128'
*   Trying 10.94.72.159:3128...
* Connected to 10.94.72.159 (10.94.72.159) port 3128 (#0)
* Proxy auth using Basic with user 'proxy-user1'
> HEAD http://raw.githubusercontent.com/openshift/windows-machine-config-operator/master/README.md HTTP/1.1
> Host: raw.githubusercontent.com
> Proxy-Authorization: Basic XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
> User-Agent: curl/8.0.1
> Accept: */ /*
> Proxy-Connection: Keep-Alive
>

< HTTP/1.1 301 Moved Permanently
< Content-Length: 0
< Server: Varnish
< Retry-After: 0
< Location: https://raw.githubusercontent.com/openshift/windows-machine-config-operator/master/README.md
< Accept-Ranges: bytes
< Date: Wed, 10 Jan 2024 23:06:14 GMT
< X-Served-By: cache-dfw-kdal2120069-DFW
< X-Cache: HIT
< X-Cache-Hits: 0
< X-Timer: S1704927975.908992,VS0,VE0
< Access-Control-Allow-Origin: *
< Cross-Origin-Resource-Policy: cross-origin
< Expires: Wed, 10 Jan 2024 23:11:14 GMT
< Vary: Authorization,Accept-Encoding
< X-Cache: MISS from 951.27.49.01.in-addr.arpa
< X-Cache-Lookup: MISS from 951.27.49.01.in-addr.arpa:3128
< Via: 1.1 varnish, 1.1 951.27.49.01.in-addr.arpa (squid/4.4)
< Connection: keep-alive
<
...
< HTTP/1.1 200 Connection established
<
...
*/
func (tc *testContext) curlHeadersOnly(nodeAddress, urlToCurl string) (string, error) {
	command := fmt.Sprintf("cmd.exe /c curl -vsIL %s", urlToCurl)
	return tc.runPowerShellSSHJob("curl-headers-only", command, nodeAddress)
}

// verifyCurlOutput parses the output of a curl request searching for any proxy variables in use and proxy headers
func verifyCurlOutput(output, httpProxyURL, noProxyList string) (bool, bool, bool) {
	// variables to look for in the curl output
	expectedHttpProxyVal := fmt.Sprintf("http_proxy == '%s'", httpProxyURL)
	expectedNoProxyVal := fmt.Sprintf("no_proxy == '%s'", noProxyList)
	// regex to ensure proxy authorization appears in the curl request headers
	proxyAuthHeaderRegex := regexp.MustCompile(`> Proxy-Authorization:*`)

	var httpProxyVarFound, noProxyVarFound, viaHeaderFound bool
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, expectedHttpProxyVal) {
			httpProxyVarFound = true
		}
		if strings.Contains(line, expectedNoProxyVal) {
			noProxyVarFound = true
		}
		if proxyAuthHeaderRegex.MatchString(line) {
			viaHeaderFound = true
		}
	}
	return httpProxyVarFound, noProxyVarFound, viaHeaderFound
}

// testCertsImport tests that any additional certificates from the proxy's trusted bundle are imported by each node
func (tc *testContext) testCertsImport(t *testing.T) {
	// TODO: this only tests the user-provided certs, a subset of the required proxy certificates.
	// Should be addressed with https://issues.redhat.com/browse/WINC-1144
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(userCABundleNamespace).Get(context.TODO(), userCABundleName, meta.GetOptions{})
	require.NoErrorf(t, err, "error getting user-provided CA ConfigMap: %w", err)

	// Read all expected certs from CM data
	trustedCABundle := cm.Data[certificates.CABundleKey]
	assert.Greater(t, len(trustedCABundle), 0, "no additional user-provided certs in bundle")

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")
			tc.checkCertsImported(t, addr, trustedCABundle)
		})
	}
}

// checkCertsImported tests that all certs in the given bundle are imported into the node with the given address
func (tc *testContext) checkCertsImported(t *testing.T, address, trustedCABundle string) {
	proxyCertsImported, err := tc.checkProxyCerts(address, []byte(trustedCABundle), 1)
	require.NoError(t, err, "error determining if proxy certs are imported")
	assert.True(t, proxyCertsImported, "proxy certs not imported")
}

// testCertsRemoval tests that any additional certificates from the proxy's trusted bundle are removed by each node
func (tc *testContext) testCertsRemoval(t *testing.T) {
	// TODO: this only tests the user-provided certs, a subset of the required proxy certificates.
	// Should be addressed with https://issues.redhat.com/browse/WINC-1144
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(userCABundleNamespace).Get(context.TODO(), userCABundleName, meta.GetOptions{})
	require.NoErrorf(t, err, "error getting user-provided CA ConfigMap: %w", err)

	// Read all expected certs from CM data
	trustedCABundle := cm.Data[certificates.CABundleKey]
	assert.Greater(t, len(trustedCABundle), 0, "no additional user-provided certs in bundle")

	require.NoError(t, tc.removeProxyTrustedCA(), "error removing user CA bundle from cluster-wide proxy")
	require.NoError(t, tc.waitForAllNodesToReboot())

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")
			tc.checkCertsRemoved(t, addr, trustedCABundle)
		})
	}
}

// checkCertsRemoved tests that all certs in the given bundle do not exist on the node with the given address
func (tc *testContext) checkCertsRemoved(t *testing.T, address, trustedCABundle string) {
	proxyCertsRemoved, err := tc.checkProxyCerts(address, []byte(trustedCABundle), 0)
	require.NoError(t, err, "error determining if proxy certs are removed")
	assert.True(t, proxyCertsRemoved, "proxy certs not removed")
}

// waitForAllNodesToReboot waits for all Windows nodes in the cluster to be rebooted
func (tc *testContext) waitForAllNodesToReboot() error {
	// Wait for reboot annotation to be applied to all Windows nodes, and then wait for removal
	if err := tc.waitForRebootAnnotation(false); err != nil {
		return err
	}
	return tc.waitForRebootAnnotation(true)
}

// waitForRebootAnnotation waits for all nodes in the cluster to have the reboot annotation either applied or removed
func (tc *testContext) waitForRebootAnnotation(waitingForRemoval bool) error {
	allWinNodes := make(map[string]struct{})
	for _, node := range gc.allNodes() {
		allWinNodes[node.Name] = struct{}{}
	}
	seenWinNodes := make(map[string]struct{})

	err := wait.PollUntilContextTimeout(context.TODO(), retry.Interval, retry.Timeout, true,
		func(context.Context) (done bool, err error) {
			labelSelector := core.LabelOSStable + "=windows"
			winNodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(), meta.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				return false, err
			}
			// Add nodes to the seen list based on the presence of the reboot annotation
			for _, node := range winNodes.Items {
				_, present := node.Annotations[metadata.RebootAnnotation]
				if present != waitingForRemoval {
					seenWinNodes[node.Name] = struct{}{}
				}
			}
			// Poll until all expected Windows nodes are seen
			return reflect.DeepEqual(seenWinNodes, allWinNodes), nil
		})
	if err != nil {
		return fmt.Errorf("timeout waiting for reboot annotation on all Windows nodes: %w", err)
	}
	return nil
}

// checkProxyCerts determines if each certificates exists the expected number of times on the given instance
func (tc *testContext) checkProxyCerts(address string, caBundleData []byte, expectedNum int) (bool, error) {
	// Read in one cert at a time and test it exists in the Windows instance's system store
	i := 0
	for block, rest := pem.Decode(caBundleData); block != nil; block, rest = pem.Decode(rest) {
		certBytes := pem.EncodeToMemory(block)
		// Multi-line certificate data causes issues in the command. Encode to base64 as a workaround
		expectedCertBase64 := base64.StdEncoding.EncodeToString(certBytes)
		commandToRun := fmt.Sprintf("$base64Data=\\\"%s\\\";"+
			// Decode base64 into cert's actual string data
			"$certString=[Text.Encoding]::Utf8.GetString([Convert]::FromBase64String($base64Data));"+
			// Create a Powershell certificate object with the expected cert.
			// First requires data to be written to a file and then provide the file path the cert constructor
			"Set-Content C:\\Temp\\cert.pem $certString;"+
			"$expectedCert=[System.Security.Cryptography.X509Certificates.X509Certificate2]::new(\\\"C:\\Temp\\cert.pem\\\");"+
			// Get the number of existing certs equivalent to the expected cert
			"(Get-ChildItem -Path Cert:\\LocalMachine\\Root | Where-Object {$expectedCert.Equals($_)}).Count",
			expectedCertBase64)
		out, err := tc.runPowerShellSSHJob(fmt.Sprintf("get-cert-%d", i), commandToRun, address)
		if err != nil {
			return false, fmt.Errorf("error running SSH job: %w", err)
		}
		// Final line should contain a single number representing the number of certs found equal to the target cert
		count, err := strconv.Atoi(finalLine(out))
		if err != nil {
			return false, err
		}

		if count != expectedNum {
			return false, nil
		}
		i++
	}
	return true, nil
}

// testEnvVars tests that on each node
// 1. the system-level environment variables are set properly as per the cluster-wide proxy
// 2. the required Windows services pick up the proper values for proxy environment variables
func (tc *testContext) testEnvVars(t *testing.T) {
	clusterProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster", meta.GetOptions{})
	if err != nil {
		require.NoError(t, err)
	}
	expectedEnvVars := map[string]string{}
	expectedEnvVars["HTTP_PROXY"] = clusterProxy.Status.HTTPProxy
	expectedEnvVars["HTTPS_PROXY"] = clusterProxy.Status.HTTPSProxy
	expectedEnvVars["NO_PROXY"] = clusterProxy.Status.NoProxy
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")
	watchedEnvVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")

			for _, proxyVar := range watchedEnvVars {
				t.Run(proxyVar, func(t *testing.T) {
					systemEnvVars, err := tc.getSystemEnvVar(addr, proxyVar)
					require.NoError(t, err, "unable to get value of %s from instance", proxyVar)
					assert.Equalf(t, expectedEnvVars[proxyVar], systemEnvVars[proxyVar], "incorrect value for %s", proxyVar)
				})
			}

			for _, svcName := range windows.RequiredServices {
				t.Run(svcName, func(t *testing.T) {
					svcEnvVars, err := tc.getProxyEnvVarsFromService(addr, svcName,
						fmt.Sprintf("%s-%s", svcName, "added"))
					require.NoErrorf(t, err, "error getting environment variables of service %s", svcName)
					for _, proxyVar := range watchedEnvVars {
						assert.Equalf(t, expectedEnvVars[proxyVar], svcEnvVars[proxyVar], "incorrect value for %s", proxyVar)
					}
				})
			}
		})
	}

	// The windows-services ConfigMap must reflect the cluster-wide proxy state (OCPBUGS-111093). Only non-empty
	// values are expected, as empty proxy fields are not written to the ConfigMap.
	t.Run("services ConfigMap", func(t *testing.T) {
		expectedCMEnvVars := expectedProxyVars(clusterProxy.Status.HTTPProxy, clusterProxy.Status.HTTPSProxy,
			clusterProxy.Status.NoProxy)
		require.NoError(t, tc.waitForConfigMapProxyVars(expectedCMEnvVars),
			"windows-services ConfigMap proxy vars do not match cluster proxy status")
	})
}

// testEnvVarRemoval removes the cluster-wide proxy fields one at a time and, after each removal, verifies that
// the windows-services ConfigMap and the nodes reflect only the remaining proxy variables. This guards against
// OCPBUGS-111093, where removing a single proxy field (e.g. httpsProxy) left the corresponding variable behind
// in the ConfigMap.
func (tc *testContext) testEnvVarRemoval(t *testing.T) {
	stages := []struct {
		name  string
		field string
	}{
		{name: "NO_PROXY", field: "/spec/noProxy"},
		{name: "HTTPS_PROXY", field: "/spec/httpsProxy"},
		{name: "HTTP_PROXY", field: "/spec/httpProxy"},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			// Capture the current Proxy status so we can detect when the cluster network operator has
			// processed the upcoming removal; the status is updated asynchronously from the spec patch.
			prePatchProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster",
				meta.GetOptions{})
			require.NoError(t, err, "unable to get cluster proxy")

			// Remove a single proxy field from the cluster Proxy spec
			patches := []*patch.JSONPatch{patch.NewJSONPatch("remove", stage.field, nil)}
			patchData, err := json.Marshal(patches)
			require.NoErrorf(t, err, "%v", patches)
			_, err = tc.client.Config.ConfigV1().Proxies().Patch(context.TODO(), "cluster", types.JSONPatchType,
				patchData, meta.PatchOptions{})
			require.NoErrorf(t, err, "unable to patch %s", string(patchData))

			// Wait for the Proxy status to reflect the removal before deriving expectations, otherwise we
			// may compute expected variables from stale status. Convergence is any change from the pre-patch
			// status; note status.noProxy retains operator-generated entries while a proxy is still active,
			// so it does not necessarily become empty when spec.noProxy is removed.
			var expectedEnvVars map[string]string
			err = wait.PollUntilContextTimeout(context.TODO(), retry.Interval, retry.ResourceChangeTimeout, true,
				func(context.Context) (bool, error) {
					clusterProxy, err := tc.client.Config.ConfigV1().Proxies().Get(context.TODO(), "cluster",
						meta.GetOptions{})
					if err != nil {
						return false, nil
					}
					if reflect.DeepEqual(clusterProxy.Status, prePatchProxy.Status) {
						return false, nil
					}
					expectedEnvVars = expectedProxyVars(clusterProxy.Status.HTTPProxy,
						clusterProxy.Status.HTTPSProxy, clusterProxy.Status.NoProxy)
					return true, nil
				})
			require.NoErrorf(t, err, "cluster proxy status did not converge after removing %s", stage.name)

			// Primary assertion: the ConfigMap must drop the removed key while keeping the rest (OCPBUGS-111093)
			require.NoError(t, tc.waitForConfigMapProxyVars(expectedEnvVars),
				"windows-services ConfigMap proxy vars not updated after removing %s", stage.name)

			// The env var change triggers a node reboot; wait for it before checking node-level state
			require.NoError(t, tc.waitForAllNodesToReboot())
			for _, node := range gc.allNodes() {
				addr, err := controllers.GetAddress(node.Status.Addresses)
				require.NoError(t, err, "unable to get node address")
				require.NoErrorf(t, tc.checkNodeProxyVars(addr, expectedEnvVars),
					"node %s proxy vars do not match expected state after removing %s", node.GetName(), stage.name)
			}
		})
	}
}

// expectedProxyVars builds the set of proxy environment variables that should appear in the windows-services
// ConfigMap given the cluster Proxy status values. Only non-empty values are included, mirroring the operator's
// cluster.GetProxyVars behavior.
func expectedProxyVars(httpProxy, httpsProxy, noProxy string) map[string]string {
	expected := map[string]string{}
	if httpProxy != "" {
		expected["HTTP_PROXY"] = httpProxy
	}
	if httpsProxy != "" {
		expected["HTTPS_PROXY"] = httpsProxy
	}
	if noProxy != "" {
		expected["NO_PROXY"] = noProxy
	}
	return expected
}

// getCurrentServicesConfigMapName returns the name of the windows-services ConfigMap for the running WMCO version
func (tc *testContext) getCurrentServicesConfigMapName() (string, error) {
	operatorVersion, err := getWMCOVersion()
	if err != nil {
		return "", err
	}
	return servicescm.NamePrefix + operatorVersion, nil
}

// getProxyEnvVarsFromConfigMap returns the proxy environment variables stored in the current windows-services
// ConfigMap. Returns an empty map when no proxy variables are set.
func (tc *testContext) getProxyEnvVarsFromConfigMap() (map[string]string, error) {
	cmName, err := tc.getCurrentServicesConfigMapName()
	if err != nil {
		return nil, err
	}
	cm, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), cmName, meta.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting ConfigMap %s: %w", cmName, err)
	}
	cmData, err := servicescm.Parse(cm.Data)
	if err != nil {
		return nil, fmt.Errorf("unable to parse ConfigMap %s data: %w", cmName, err)
	}
	return cmData.EnvironmentVars, nil
}

// waitForConfigMapProxyVars waits until the windows-services ConfigMap's proxy environment variables match the
// expected map. Nil and empty maps are treated as equal, since the field is omitted when no proxy is set. This
// absorbs the delay between patching the Proxy spec, the status settling, and WMCO regenerating the ConfigMap.
func (tc *testContext) waitForConfigMapProxyVars(expected map[string]string) error {
	var lastSeen map[string]string
	var lastErr error
	err := wait.PollUntilContextTimeout(context.TODO(), retry.Interval, retry.ResourceChangeTimeout, true,
		func(context.Context) (bool, error) {
			actual, err := tc.getProxyEnvVarsFromConfigMap()
			if err != nil {
				lastErr = err
				return false, nil
			}
			lastSeen = actual
			if len(actual) == 0 && len(expected) == 0 {
				return true, nil
			}
			return reflect.DeepEqual(actual, expected), nil
		})
	if err != nil {
		return fmt.Errorf("timed out waiting for ConfigMap proxy vars to equal %v (last seen %v, last error %v): %w",
			expected, lastSeen, lastErr, err)
	}
	return nil
}

// checkNodeProxyVars verifies that the system-level and service-level proxy environment variables on the instance
// at the given address match the expected set: variables present in the set must have the expected value, and
// variables absent from the set must not be set on the instance.
func (tc *testContext) checkNodeProxyVars(address string, expected map[string]string) error {
	watchedEnvVars := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	for _, proxyVar := range watchedEnvVars {
		expectedVal, shouldExist := expected[proxyVar]

		systemEnvVars, err := tc.getSystemEnvVar(address, proxyVar)
		if err != nil {
			return fmt.Errorf("error retrieving system level ENV var %s: %w", proxyVar, err)
		}
		actualVal, exists := systemEnvVars[proxyVar]
		if shouldExist && (!exists || actualVal != expectedVal) {
			return fmt.Errorf("system ENV var %s = %q, expected %q", proxyVar, actualVal, expectedVal)
		}
		if !shouldExist && exists {
			return fmt.Errorf("system ENV var %s should be removed but is set to %q", proxyVar, actualVal)
		}

		for _, svcName := range windows.RequiredServices {
			svcEnvVars, err := tc.getProxyEnvVarsFromService(address, svcName,
				fmt.Sprintf("%s-%s", svcName, "staged"))
			if err != nil {
				return fmt.Errorf("error retrieving service %s ENV vars: %w", svcName, err)
			}
			svcVal, svcExists := svcEnvVars[proxyVar]
			if shouldExist && (!svcExists || svcVal != expectedVal) {
				return fmt.Errorf("service %s ENV var %s = %q, expected %q", svcName, proxyVar, svcVal, expectedVal)
			}
			if !shouldExist && svcExists {
				return fmt.Errorf("service %s ENV var %s should be removed but is set to %q", svcName, proxyVar,
					svcVal)
			}
		}
	}
	return nil
}

// testTrustedCAConfigMap tests multiple aspects of expected functionality for the trusted-ca ConfigMap
// 1. It exists on operator startup 2. It is re-created when deleted 3. It is patched if invalid contents are detected.
// The ConfigMap data is managed by CNO so no need to do content validation testing.
func (tc *testContext) testTrustedCAConfigMap(t *testing.T) {
	// Ensure the trusted-ca ConfigMap exists in the cluster as expected
	t.Run("Trusted CA ConfigMap metadata", func(t *testing.T) {
		trustedCA, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.GetOptions{})
		require.NoErrorf(t, err, "error ensuring ConfigMap %s exists", certificates.ProxyCertsConfigMap)
		assert.True(t, trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true")
	})

	t.Run("Trusted CA ConfigMap re-creation", func(t *testing.T) {
		err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.DeleteOptions{})
		require.NoError(t, err)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoErrorf(t, err, "error ensuring ConfigMap %s is re-created when deleted", certificates.ProxyCertsConfigMap)
	})

	t.Run("Invalid trusted CA ConfigMap patching", func(t *testing.T) {
		// Intentionally remove the required label and wait for WMCO to reconcile and re-apply it
		var labelPatch = []*patch.JSONPatch{
			patch.NewJSONPatch("remove", "/metadata/labels", map[string]string{controllers.InjectionRequestLabel: "true"}),
		}
		patchData, err := json.Marshal(labelPatch)
		require.NoError(t, err)

		_, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Patch(context.TODO(), certificates.ProxyCertsConfigMap,
			types.JSONPatchType, patchData, meta.PatchOptions{})
		require.NoErrorf(t, err, "unable to patch %s", certificates.ProxyCertsConfigMap)
		err = tc.waitForValidTrustedCAConfigMap()
		assert.NoError(t, err, "error testing handling of invalid ConfigMap")
	})
}

// waitForValidTrustedCAConfigMap returns a reference to the ConfigMap that matches the given name.
// If a ConfigMap with valid contents is not found within the time limit, an error is returned.
func (tc *testContext) waitForValidTrustedCAConfigMap() error {
	trustedCA := &core.ConfigMap{}
	err := wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		var err error
		trustedCA, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
			certificates.ProxyCertsConfigMap, meta.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Retry if the Get() results in a IsNotFound error
				return false, nil
			}
			return false, fmt.Errorf("error retrieving ConfigMap %s: %w", certificates.ProxyCertsConfigMap, err)
		}
		// Here, we've retreived a ConfigMap but still need to ensure it is valid.
		// If it's not valid, retry in hopes that WMCO will replace it with a valid one as expected.
		return trustedCA.GetLabels()[controllers.InjectionRequestLabel] == "true", nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for ConfigMap %s/%s: %w", wmcoNamespace, certificates.ProxyCertsConfigMap, err)
	}
	return nil
}

// getSystemEnvVar returns the value corresponding to the input proxy ENV var as set in the registry
func (tc *testContext) getSystemEnvVar(addr, variableName string) (map[string]string, error) {
	command := fmt.Sprintf("Get-ChildItem -Path Env: | Where-Object -Property Name -eq '%s' | Format-List ",
		variableName)
	return tc.getEnvVar(addr, variableName, command)
}

// getProxyEnvVarsFromService returns a map of all environment variables present in a service's config
func (tc *testContext) getProxyEnvVarsFromService(addr, svcName, jobName string) (map[string]string, error) {
	command := fmt.Sprintf("Get-Process %s | ForEach-Object { $_.StartInfo.EnvironmentVariables.GetEnumerator() "+
		"| Format-List }",
		svcName)
	return tc.getEnvVar(addr, jobName, command)
}

func (tc *testContext) getEnvVar(addr, name, command string) (map[string]string, error) {
	out, err := tc.runPowerShellSSHJob(fmt.Sprintf("get-%s-env-vars",
		strings.ToLower(strings.ReplaceAll(name, "_", "-"))), command, addr)
	if err != nil {
		return nil, fmt.Errorf("error running SSH job: %w", err)
	}
	return parseWindowsEnvVars(out), nil
}

// configureClusterNoProxy configures the cluster-wide proxy's bypass list with specific comma-separated URLs
func (tc *testContext) configureClusterNoProxy(addresses []string) error {
	commaSeparatedList := strings.Join(addresses[:], ",")
	patches := []*patch.JSONPatch{patch.NewJSONPatch("replace", "/spec/noProxy", commaSeparatedList)}
	patchData, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("unable to generate patch request body for %v: %w", patches, err)
	}
	_, err = tc.client.Config.ConfigV1().Proxies().Patch(context.TODO(), "cluster", types.JSONPatchType, patchData,
		meta.PatchOptions{})
	if err != nil {
		return fmt.Errorf("unable to patch %s: %w", string(patchData), err)
	}
	return nil
}

// configureUserCABundle configures the cluster-wide proxy with additional user-provided certificates
func (tc *testContext) configureUserCABundle() error {
	if err := tc.createUserCABundle(); err != nil {
		return err
	}
	return tc.addProxyTrustedCA()
}

// createUserCABundle creates a ConfigMap with an additional trusted CA bundle
func (tc *testContext) createUserCABundle() error {
	cert, err := generateCertificate()
	if err != nil {
		return fmt.Errorf("unable to generate additional certs: %w", err)
	}
	userCABundleCM := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{
			Name:      userCABundleName,
			Namespace: userCABundleNamespace,
		},
		Data: map[string]string{
			certificates.CABundleKey: cert,
		},
	}
	_, err = tc.client.K8s.CoreV1().ConfigMaps(userCABundleNamespace).Create(context.TODO(), userCABundleCM, meta.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating user-provided CA ConfigMap: %w", err)
	}
	return nil
}

// addProxyTrustedCA adds the user-provided CA bundle to the cluster-wide proxy config
func (tc *testContext) addProxyTrustedCA() error {
	return tc.patchProxyTrustedCA(userCABundleName)
}

// removeProxyTrustedCA removes the user-provided CA bundle from the cluster-wide proxy config
func (tc *testContext) removeProxyTrustedCA() error {
	return tc.patchProxyTrustedCA("")
}

// patchProxyTrustedCA patches the proxy to use the given CA bundle, which CNO will merge with other proxy certs.
// An empty parameter means the proxy will be configured with no user-provided additional certificates.
func (tc *testContext) patchProxyTrustedCA(configMapName string) error {
	patches := []*patch.JSONPatch{patch.NewJSONPatch("replace", "/spec/trustedCA/name", configMapName)}
	patchData, err := json.Marshal(patches)
	if err != nil {
		return fmt.Errorf("invalid patch data %v: %w", patches, err)
	}
	_, err = tc.client.Config.ConfigV1().Proxies().Patch(context.TODO(), "cluster", types.JSONPatchType, patchData,
		meta.PatchOptions{})
	if err != nil {
		return fmt.Errorf("unable to patch proxy with trustedCA: %w", err)
	}
	return nil
}

// generateCertificate generates a new self-signed PEM-encoded certificate
func generateCertificate() (string, error) {
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(33),
		Subject: pkix.Name{
			Organization:  []string{"New Test Cert Org."},
			Country:       []string{"US"},
			Province:      []string{"MA"},
			Locality:      []string{"Boston"},
			StreetAddress: []string{"New Test Cert St."},
			PostalCode:    []string{"02115"},
		},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		SubjectKeyId: []byte{1, 2, 3, 4, 6},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, cert, cert, &certPrivKey.PublicKey, certPrivKey)
	if err != nil {
		return "", err
	}
	certPEM := new(bytes.Buffer)
	pem.Encode(certPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	})
	return certPEM.String(), nil
}

// parseWindowsEnvVars parses the Powershell output listing all environment variables with their name, value pairs
// and returns a map of ENV vars to their corresponding values.
// Sample input:
// Name  : HTTP_PROXY
// Value : http://dev:d3436c0b817f7ca8e23f7b47be49945d@10.0.1.10:3128/
// Name  : SHELL
// Value : c:\windows\system32\cmd.exe
func parseWindowsEnvVars(pwshOutput string) map[string]string {
	var valueLines []string
	var value string
	var currentVarName string
	proxyEnvVars := make(map[string]string)
	lines := strings.Split(pwshOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				currentVarName = strings.TrimSpace(parts[1])
			}
		} else if strings.HasPrefix(line, "Value") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				valueLine := strings.TrimSpace(strings.TrimPrefix(parts[1], "Value:"))
				valueLines = []string{valueLine}
			} // case when a long ENV var value like NO_PROXY is split into multiple elements
		} else if line != "" {
			valueLines = append(valueLines, line)
		}
		if len(valueLines) > 0 {
			value = strings.Join(valueLines, "")
			value = strings.ReplaceAll(value, ";", ",")
			proxyEnvVars[currentVarName] = value
		}
	}
	return proxyEnvVars
}
