package winc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	wicdConfigMap    = "windows-services"
	trustedCACM      = "trusted-ca"
	proxyCAConfigMap = "trusted-ca"
	windowsWorkloads = "win-webserver"
	defaultNamespace = "winc-test"
)

type ConfigMapPayload struct {
	Data struct {
		CaBundleCrt string `json:"ca-bundle.crt"`
	} `json:"data"`
}

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

func waitForProxyStatus(oc *exutil.CLI) {
	e2e.Logf("Waiting for proxy status to be reconciled by cluster-network-operator")
	waitErr := wait.Poll(10*time.Second, 4*time.Minute, func() (bool, error) {
		statusMap := getEnvVarProxyMap(oc)
		return len(statusMap) > 0, nil
	})
	o.Expect(waitErr).NotTo(o.HaveOccurred(), "proxy status was not reconciled within 4 minutes")
}

func getClusterProxy(oc *exutil.CLI, value string) string {
	clusterProxy, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("proxies", "-o=jsonpath={.items[*]."+value+"}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	return clusterProxy
}

func getProxySpec(oc *exutil.CLI) map[string]interface{} {
	spec := make(map[string]interface{})
	spec["HTTPS_PROXY"] = getClusterProxy(oc, "spec.httpsProxy")
	spec["HTTP_PROXY"] = getClusterProxy(oc, "spec.httpProxy")
	spec["NO_PROXY"] = getClusterProxy(oc, "spec.noProxy")
	return spec
}

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

func getPayloadMap(payload string) map[string]interface{} {
	var m map[string]interface{}
	json.Unmarshal([]byte(payload), &m)
	return m
}

func waitForWICDConfigMapUpdate(oc *exutil.CLI, windowsServicesCM string, expectedValues map[string]interface{}) map[string]interface{} {
	var wicdProxies map[string]interface{}
	pollErr := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
		wicdPayload, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("cm", windowsServicesCM, "-n", wmcoNamespace, "-o=jsonpath={.data.environmentVars}").Output()
		if err != nil || wicdPayload == "" {
			e2e.Logf("Waiting for WICD ConfigMap update: %v", err)
			return false, nil
		}
		wicdProxies = getPayloadMap(wicdPayload)
		if compareMaps(expectedValues, wicdProxies) {
			e2e.Logf("WICD ConfigMap updated with expected values")
			return true, nil
		}
		e2e.Logf("Waiting for WICD ConfigMap to update")
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, "WICD ConfigMap did not update with expected values within 5 minutes")
	return wicdProxies
}

func waitForWICDConfigMapContains(oc *exutil.CLI, windowsServicesCM string, key string, substring string) {
	pollErr := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
		jsonOutput, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("cm", windowsServicesCM, "-ojsonpath={.data.environmentVars}", "-n", wmcoNamespace).Output()
		if err != nil {
			e2e.Logf("Waiting for WICD ConfigMap: %v", err)
			return false, nil
		}
		value := gjson.Get(jsonOutput, key)
		if strings.Contains(fmt.Sprint(value), substring) {
			e2e.Logf("WICD ConfigMap %s contains %s", key, substring)
			return true, nil
		}
		e2e.Logf("Waiting for %s in WICD ConfigMap %s, current: %s", substring, key, value)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("WICD ConfigMap %s did not contain %s within 5 minutes", key, substring))
}

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

// redactProxyURL redacts user:password credentials from proxy URLs for safe logging
func redactProxyURL(proxyURL string) string {
	if proxyURL == "" {
		return ""
	}
	// Match http://user:pass@host or https://user:pass@host
	re := regexp.MustCompile(`(https?://)([^:]+):([^@]+)@`)
	return re.ReplaceAllString(proxyURL, "${1}***:***@")
}

func compareMaps(map1, map2 map[string]interface{}) bool {
	if len(map1) != len(map2) {
		return false
	}
	for key := range map1 {
		val1 := compileEnvVars(fmt.Sprint(map1[key]))
		val2 := compileEnvVars(fmt.Sprint(map2[key]))
		value := strings.ReplaceAll(val2, ";", ",")
		if val1 != value {
			e2e.Logf("Proxy variable %s values differ: expected=%s actual=%s", key, redactProxyURL(val1), redactProxyURL(val2))
			return false
		}
	}
	return true
}

func getWindowsNodeNames(oc *exutil.CLI) []string {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].metadata.name}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if strings.TrimSpace(output) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(output), " ")
}

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
			o.Expect(strings.TrimSpace(msg)).Should(o.ContainSubstring(proxyStr), "proxy %v not found on %v", key, nodeName)
		}
	}
}

const (
	proxyPropagationPollInterval = 20 * time.Second
	proxyPropagationPollTimeout  = 5 * time.Minute
)

type proxyPropagationPollConfig struct {
	interval time.Duration
	timeout  time.Duration
}

type hostProcessContextRunner func(context.Context, *exutil.CLI, string, string, string, ...bool) (string, error)

func defaultProxyPropagationPollConfig() proxyPropagationPollConfig {
	return proxyPropagationPollConfig{
		interval: proxyPropagationPollInterval,
		timeout:  proxyPropagationPollTimeout,
	}
}

func contextTerminationError(ctx context.Context) error {
	err := ctx.Err()
	if err == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if cause == nil || cause == err {
		return err
	}
	return fmt.Errorf("%w: %w", err, cause)
}

// waitForProxyOnNodes gives all proxy checks across all supplied nodes one five-minute polling budget. Each
// read-only HostProcess check receives the remaining polling context. HostProcess pod cleanup is deliberately
// outside that budget and retains its separate 30-second UID-protected allowance after cancellation.
func waitForProxyOnNodes(oc *exutil.CLI, winNodes []string, wicdProxies map[string]interface{}) {
	pollErr := waitForProxyOnNodesWithContext(g.GinkgoT().Context(), oc, winNodes, wicdProxies,
		runHostProcessPSWithContext, defaultProxyPropagationPollConfig())
	compat_otp.AssertWaitPollNoErr(pollErr, "proxy values did not propagate to all Windows nodes within 5 minutes")
}

func waitForProxyOnNodesWithContext(ctx context.Context, oc *exutil.CLI, winNodes []string,
	wicdProxies map[string]interface{}, runHostProcess hostProcessContextRunner,
	config proxyPropagationPollConfig) error {
	pollErr := wait.PollUntilContextTimeout(ctx, config.interval, config.timeout, false,
		func(pollCtx context.Context) (bool, error) {
			if err := contextTerminationError(pollCtx); err != nil {
				return false, err
			}
			for _, nodeName := range winNodes {
				for key, proxy := range wicdProxies {
					if err := contextTerminationError(pollCtx); err != nil {
						return false, err
					}
					proxyStr := fmt.Sprint(proxy)
					if proxyStr == "" {
						continue
					}
					cmd := fmt.Sprintf("[System.Environment]::GetEnvironmentVariable('%v', 'Machine')", key)
					msg, err := runHostProcess(pollCtx, oc, nodeName, windowsDebugImage, cmd)
					if err != nil {
						if ctxErr := contextTerminationError(pollCtx); ctxErr != nil {
							return false, ctxErr
						}
						if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
							return false, err
						}
						e2e.Logf("Waiting for %v on %v: error: %v", key, nodeName, err)
						return false, nil
					}
					if err := contextTerminationError(pollCtx); err != nil {
						return false, err
					}
					if !strings.Contains(strings.TrimSpace(msg), proxyStr) {
						e2e.Logf("Waiting for proxy variable %s on node %s: expected=%s actual=%s",
							key, nodeName, redactProxyURL(proxyStr), redactProxyURL(strings.TrimSpace(msg)))
						return false, nil
					}
				}
			}
			if err := contextTerminationError(pollCtx); err != nil {
				return false, err
			}
			e2e.Logf("All proxy values propagated to all Windows nodes")
			return true, nil
		})
	if err := contextTerminationError(ctx); err != nil {
		return err
	}
	return pollErr
}

func getWMCOTimestamp(oc *exutil.CLI) string {
	wmcoTime, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "--selector", "name="+wmcoDeploymentName, "--field-selector=status.phase=Running", "-o=jsonpath={.items[0].status.startTime}", "-n", wmcoNamespace).Output()
	if err != nil || wmcoTime == "" {
		return ""
	}
	return wmcoTime
}

func checkWMCORestarted(oc *exutil.CLI, startTime string) (bool, error) {
	var restartDetected bool
	pollErr := wait.Poll(20*time.Second, 6*time.Minute, func() (bool, error) {
		actualWMCOTime := getWMCOTimestamp(oc)
		if actualWMCOTime == "" {
			e2e.Logf("WMCO pod timestamp unavailable (pod transitioning), waiting...")
			return false, nil
		}
		if startTime != actualWMCOTime {
			e2e.Logf("WMCO restarted (old: %s, new: %s)", startTime, actualWMCOTime)
			restartDetected = true
			return true, nil
		}
		e2e.Logf("WMCO did not restart yet, waiting...")
		return false, nil
	})
	if pollErr != nil {
		if pollErr == wait.ErrWaitTimeout {
			e2e.Logf("WMCO did not restart within 6 minutes (this is expected for some proxy changes)")
			return false, nil
		}
		return false, fmt.Errorf("error checking WMCO restart: %w", pollErr)
	}
	return restartDetected, nil
}

func generateClusterProxy(httpProxy, httpsProxy, noProxy string) string {
	return fmt.Sprintf(`apiVersion: config.openshift.io/v1
kind: Proxy
metadata:
  name: cluster
spec:
  httpProxy: %s
  httpsProxy: %s
  noProxy: %s
  trustedCA:
    name: user-ca-bundle
`, httpProxy, httpsProxy, noProxy)
}

func restoreProxyEnvironment(oc *exutil.CLI, clusterEnvVars map[string]interface{}) {
	e2e.Logf("Starting proxy environment restore")
	wmcoStartTime := getWMCOTimestamp(oc)
	httpProxy := fmt.Sprint(clusterEnvVars["HTTP_PROXY"])
	httpsProxy := fmt.Sprint(clusterEnvVars["HTTPS_PROXY"])
	noProxy := fmt.Sprint(clusterEnvVars["NO_PROXY"])

	pollErr := wait.Poll(10*time.Second, 2*time.Minute, func() (bool, error) {
		applyErr := createResourceFromString(oc, "", generateClusterProxy(httpProxy, httpsProxy, noProxy))
		if applyErr != nil {
			e2e.Logf("retrying proxy restore: %v", applyErr)
			return false, nil
		}
		return true, nil
	})
	if pollErr != nil {
		e2e.Logf("Warning: proxy restore did not complete within 2 minutes: %v", pollErr)
		return
	}

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
	if len(patches) > 0 {
		patchJSON := "[" + strings.Join(patches, ",") + "]"
		e2e.Logf("Removing empty proxy fields: %s", patchJSON)
		err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", patchJSON).Execute()
		if err != nil {
			e2e.Logf("Warning: failed to remove empty proxy fields: %v", err)
		}
	}

	restarted, _ := checkWMCORestarted(oc, wmcoStartTime)
	if restarted {
		e2e.Logf("WMCO restarted after proxy restore, waiting for WICD propagation")
	} else {
		e2e.Logf("WMCO did not restart after proxy restore")
	}
	winNodes := getWindowsInternalIPs(oc)
	waitWindowsNodesReady(oc, len(winNodes), 15*time.Minute)
	expectedProxies := getEnvVarProxyMap(oc)
	winNodeNames := getWindowsNodeNames(oc)
	waitForProxyOnNodes(oc, winNodeNames, expectedProxies)
	e2e.Logf("Proxy environment restored successfully")
}
