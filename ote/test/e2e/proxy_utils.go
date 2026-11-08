package winc

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// proxyCredentialsRegexp matches userinfo in an HTTP or HTTPS proxy URL. The
// username and password cannot contain URL delimiters or whitespace, while the
// password may contain '@'.
var proxyCredentialsRegexp = regexp.MustCompile(`(https?://)([^:/?#\s@]+):([^/?#\s]+)@`)

// isProxy reports whether the cluster has a proxy configuration that WMCO tests should exercise.
func isProxy(oc *exutil.CLI) bool {
	if iaasPlatform == "nutanix" {
		return false
	}
	spec := getProxySpec(oc)
	for _, v := range spec {
		if s, ok := v.(string); ok && s != "" {
			if strings.Contains(s, "ci.devcluster.openshift.com") {
				return false
			}
			return true
		}
	}
	return false
}

// waitForProxyStatus waits until the cluster-network-operator reconciles proxy status.
func waitForProxyStatus(oc *exutil.CLI) {
	e2e.Logf("Waiting for proxy status to be reconciled by cluster-network-operator")
	waitErr := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 4*time.Minute, true,
		func(context.Context) (bool, error) {
			statusMap := getEnvVarProxyMap(oc)
			return len(statusMap) > 0, nil
		})
	o.Expect(waitErr).NotTo(o.HaveOccurred(), "proxy status was not reconciled within 4 minutes")
}

// getClusterProxy returns one field from the cluster Proxy object.
func getClusterProxy(oc *exutil.CLI, value string) string {
	clusterProxy, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("proxies", "-o=jsonpath={.items[*]."+value+"}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return clusterProxy
}

// getProxySpec returns the cluster proxy specification as environment variable names.
func getProxySpec(oc *exutil.CLI) map[string]interface{} {
	spec := make(map[string]interface{})
	spec["HTTPS_PROXY"] = getClusterProxy(oc, "spec.httpsProxy")
	spec["HTTP_PROXY"] = getClusterProxy(oc, "spec.httpProxy")
	spec["NO_PROXY"] = getClusterProxy(oc, "spec.noProxy")
	return spec
}

// getEnvVarProxyMap returns the cluster proxy status as environment variable names.
func getEnvVarProxyMap(oc *exutil.CLI, replacement ...map[string]string) map[string]interface{} {
	clusterEnvVars := make(map[string]interface{})
	if replacement == nil {
		if v := getClusterProxy(oc, "status.httpsProxy"); v != "" {
			clusterEnvVars["HTTPS_PROXY"] = v
		}
		if v := getClusterProxy(oc, "status.httpProxy"); v != "" {
			clusterEnvVars["HTTP_PROXY"] = v
		}
		if v := getClusterProxy(oc, "status.noProxy"); v != "" {
			clusterEnvVars["NO_PROXY"] = v
		}
	} else {
		for _, m := range replacement {
			for key, value := range m {
				if v := getClusterProxy(oc, value); v != "" {
					clusterEnvVars[key] = v
				}
			}
		}
	}
	return clusterEnvVars
}

// compileEnvVars normalizes PowerShell output into a single comparable value.
func compileEnvVars(pwshOutput string) string {
	var valueLines []string
	var value string
	lines := strings.Split(pwshOutput, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			valueLine := strings.TrimSpace(strings.TrimPrefix(parts[1], "Value:"))
			valueLines = []string{valueLine}
		} else if line != "" {
			valueLines = append(valueLines, line)
		}
		if len(valueLines) > 0 {
			value = strings.Join(valueLines, "")
		}
	}
	return value
}

// compareMaps compares proxy values while redacting credentials in mismatch logs.
func compareMaps(map1, map2 map[string]interface{}) bool {
	if len(map1) != len(map2) {
		return false
	}
	for key := range map1 {
		expectedRaw := fmt.Sprint(map1[key])
		actualRaw := fmt.Sprint(map2[key])
		val1 := compileEnvVars(expectedRaw)
		val2 := compileEnvVars(actualRaw)
		value := strings.ReplaceAll(val2, ";", ",")
		if val1 != value {
			e2e.Logf("Proxy variable %s values differ: expected=%s actual=%s",
				key, redactProxyURL(expectedRaw), redactProxyURL(actualRaw))
			return false
		}
	}
	return true
}

// redactProxyURL redacts credentials from a proxy URL without changing surrounding text.
func redactProxyURL(proxyURL string) string {
	if proxyURL == "" {
		return ""
	}
	return proxyCredentialsRegexp.ReplaceAllString(proxyURL, "${1}***:***@")
}

// checkProxyVarsOnNodes checks that proxy variables exist on every Windows node.
func checkProxyVarsOnNodes(oc *exutil.CLI, winNodes []string, wicdProxies map[string]interface{}) {
	for _, nodeName := range winNodes {
		for key, proxy := range wicdProxies {
			proxyStr := fmt.Sprint(proxy)
			if proxyStr == "" {
				continue
			}
			e2e.Logf("Check %v proxy exists on worker %v", key, nodeName)
			cmd := fmt.Sprintf("[System.Environment]::GetEnvironmentVariable('%v', 'Machine')", key)
			msg, err := runHostProcessPS(oc, nodeName, windowsDebugImage, cmd)
			o.Expect(err).NotTo(o.HaveOccurred())
			trimmed := strings.TrimSpace(msg)
			o.Expect(strings.Contains(trimmed, proxyStr)).To(o.BeTrue(),
				"proxy %v not found on %v: expected=%s actual=%s",
				key, nodeName, redactProxyURL(proxyStr), redactProxyURL(trimmed))
		}
	}
}

// waitForProxyOnNodes waits until proxy variables propagate to every Windows node.
func waitForProxyOnNodes(oc *exutil.CLI, winNodes []string, wicdProxies map[string]interface{}) {
	keys := []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	pollErr := wait.PollUntilContextTimeout(context.Background(), 20*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			for _, nodeName := range winNodes {
				for _, key := range keys {
					proxyStr := proxyValue(wicdProxies, key)
					cmd := fmt.Sprintf("[System.Environment]::GetEnvironmentVariable('%v', 'Machine')", key)
					msg, err := runHostProcessPSWithContext(ctx, oc, nodeName, windowsDebugImage, cmd)
					if err != nil {
						e2e.Logf("Waiting for %v on %v: error: %v", key, nodeName, err)
						return false, nil
					}
					trimmed := strings.TrimSpace(msg)
					if proxyStr == "" {
						if trimmed != "" {
							e2e.Logf("Waiting for proxy variable %s to clear on node %s: actual=%s",
								key, nodeName, redactProxyURL(trimmed))
							return false, nil
						}
						continue
					}
					if !strings.Contains(trimmed, proxyStr) {
						e2e.Logf("Waiting for proxy variable %s on node %s: expected=%s actual=%s",
							key, nodeName, redactProxyURL(proxyStr), redactProxyURL(trimmed))
						return false, nil
					}
				}
			}
			e2e.Logf("All proxy values propagated to all Windows nodes")
			return true, nil
		})
	compat_otp.AssertWaitPollNoErr(pollErr, "proxy values did not propagate to all Windows nodes within 5 minutes")
}

// generateClusterProxy returns a Proxy resource that restores the captured values.
func generateClusterProxy(httpProxy, httpsProxy, noProxy, trustedCAName string) string {
	trustedCABlock := ""
	if trustedCAName != "" {
		trustedCABlock = fmt.Sprintf("\n  trustedCA:\n    name: %s", trustedCAName)
	}
	return fmt.Sprintf(`apiVersion: config.openshift.io/v1
kind: Proxy
metadata:
  name: cluster
spec:
  httpProxy: %s
  httpsProxy: %s
  noProxy: %s%s
`, httpProxy, httpsProxy, noProxy, trustedCABlock)
}

// proxyValue returns a captured proxy value, treating missing or nil values as empty.
func proxyValue(clusterEnvVars map[string]interface{}, key string) string {
	if value, ok := clusterEnvVars[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

// restoreProxyEnvironment restores the original cluster proxy configuration after a test mutates it.
func restoreProxyEnvironment(oc *exutil.CLI, clusterEnvVars map[string]interface{}, trustedCAName string) {
	e2e.Logf("Starting proxy environment restore")
	wmcoStartTime := getWMCORestartState(oc)
	httpProxy := proxyValue(clusterEnvVars, "HTTP_PROXY")
	httpsProxy := proxyValue(clusterEnvVars, "HTTPS_PROXY")
	noProxy := proxyValue(clusterEnvVars, "NO_PROXY")
	trustedCAName = strings.TrimSpace(trustedCAName)

	pollErr := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, 2*time.Minute, true,
		func(context.Context) (bool, error) {
			applyErr := createResourceFromString(oc, "", generateClusterProxy(httpProxy, httpsProxy, noProxy, trustedCAName))
			if applyErr != nil {
				e2e.Logf("retrying proxy restore: %v", applyErr)
				return false, nil
			}
			return true, nil
		})
	compat_otp.AssertWaitPollNoErr(pollErr, "proxy restore did not complete within 2 minutes")

	var patches []string
	if noProxy == "" && getClusterProxy(oc, "spec.noProxy") != "" {
		patches = append(patches, `{"op": "remove", "path": "/spec/noProxy"}`)
	}
	if httpsProxy == "" && getClusterProxy(oc, "spec.httpsProxy") != "" {
		patches = append(patches, `{"op": "remove", "path": "/spec/httpsProxy"}`)
	}
	if httpProxy == "" && getClusterProxy(oc, "spec.httpProxy") != "" {
		patches = append(patches, `{"op": "remove", "path": "/spec/httpProxy"}`)
	}
	if trustedCAName == "" && getClusterProxy(oc, "spec.trustedCA.name") != "" {
		patches = append(patches, `{"op": "remove", "path": "/spec/trustedCA"}`)
	}
	if len(patches) > 0 {
		patchJSON := "[" + strings.Join(patches, ",") + "]"
		e2e.Logf("Removing empty proxy fields: %s", patchJSON)
		err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", patchJSON).Execute()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to remove empty proxy fields")
	}

	restarted, restartErr := checkWMCORestarted(oc, wmcoStartTime)
	if restartErr != nil {
		e2e.Logf("Warning: error checking WMCO restart: %v", restartErr)
	}
	if restarted {
		e2e.Logf("WMCO restarted after proxy restore, waiting for WICD propagation")
	} else {
		e2e.Logf("WMCO did not restart after proxy restore")
	}
	winNodes := getWindowsInternalIPs(oc)
	waitWindowsNodesReady(oc, len(winNodes), 15*time.Minute)
	expectedProxies := getProxySpec(oc)
	winNodeNames := getWindowsNodeNames(oc)
	waitForProxyOnNodes(oc, winNodeNames, expectedProxies)
	e2e.Logf("Proxy environment restored successfully")
}
