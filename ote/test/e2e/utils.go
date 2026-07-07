package winc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/tidwall/gjson"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var (
	mcoNamespace     = "openshift-machine-api"
	capiNamespace    = "openshift-cluster-api"
	wmcoNamespace    = "openshift-windows-machine-config-operator"
	privateKey       = ""
	publicKey        = ""
	iaasPlatform     string
	windowsNodeLabel = "kubernetes.io/os=windows"
	linuxNodeLabel   = "kubernetes.io/os=linux"
)

func checkVersionAnnotationReady(oc *exutil.CLI, windowsNodeName string) (bool, error) {
	msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", windowsNodeName, "-o=jsonpath={.metadata.annotations.windowsmachineconfig\\.openshift\\.io\\/version}").Output()
	if msg == "" {
		return false, err
	}
	return true, err
}

func getWindowsHostNames(oc *exutil.CLI) []string {
	winHostNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].status.addresses[?(@.type==\"Hostname\")].address}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if winHostNames == "" {
		return []string{}
	}
	return strings.Split(winHostNames, " ")
}

func getWindowsInternalIPs(oc *exutil.CLI) []string {
	winInternalIPs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].status.addresses[?(@.type==\"InternalIP\")].address}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if winInternalIPs == "" {
		return []string{}
	}
	return strings.Split(winInternalIPs, " ")
}

func removeOuterQuotes(s string) string {
	if len(s) >= 2 {
		if c := s[len(s)-1]; s[0] == c && (c == '"' || c == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func truncatedVersion(s string) string {
	s = removeOuterQuotes(s)
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return s
	}
	return parts[0] + "." + parts[1]
}

func getMetricsFromCluster(oc *exutil.CLI, metric string) string {
	retValue := 0
	if strings.Contains(metric, "node_instance_type_count") {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", "node.openshift.io/os_id=Windows", "-o=jsonpath={.items[*].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		retValue = len(strings.Fields(output))
	} else if strings.Contains(metric, "capacity_cpu_cores") {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", "node.openshift.io/os_id=Windows", "-o=jsonpath={.items[*].status.capacity.cpu}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, cpuVal := range strings.Fields(output) {
			cpuCast, convErr := strconv.Atoi(cpuVal)
			o.Expect(convErr).NotTo(o.HaveOccurred())
			retValue += cpuCast
		}
	} else {
		e2e.Failf("Metric %s not supported yet", metric)
	}
	return strconv.Itoa(retValue)
}

func getWMCOVersionFromLogs(oc *exutil.CLI) (string, error) {
	log, err := oc.AsAdmin().WithoutNamespace().
		Run("logs").Args("deployment.apps/windows-machine-config-operator",
		"-n", wmcoNamespace).Output()
	if err != nil {
		return "", fmt.Errorf("fetching WMCO logs: %w", err)
	}

	patterns := []*regexp.Regexp{
		regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`"Version"\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`operator\s+version\s+([^\s"']+)`),
	}
	for _, re := range patterns {
		if m := re.FindStringSubmatch(log); len(m) >= 2 {
			return m[1], nil
		}
	}

	return "", fmt.Errorf("WMCO version string not found in operator logs")
}

func matchKubeletVersion(oc *exutil.CLI, version1, version2 string) bool {
	version1Parts := strings.Split(strings.Split(strings.TrimPrefix(version1, "v"), "+")[0], ".")
	version2Parts := strings.Split(strings.Split(strings.TrimPrefix(version2, "v"), "+")[0], ".")
	if len(version1Parts) < 3 || len(version2Parts) < 3 {
		return false
	}

	wmcoLogVersion, err := getWMCOVersionFromLogs(oc)
	if err != nil {
		e2e.Logf("Error getting WMCO version from logs: %v", err)
		return false
	}
	if strings.HasSuffix(strings.Split(wmcoLogVersion, "-")[0], ".0.0") {
		return version1Parts[0] == version2Parts[0] && version1Parts[1] == version2Parts[1] && version1Parts[2] == version2Parts[2]
	}
	return version1Parts[0] == version2Parts[0] && version1Parts[1] == version2Parts[1]
}

func extractMetricValue(queryResult string) string {
	jsonResult := gjson.Parse(queryResult)
	status := jsonResult.Get("status").String()
	o.Expect(status).Should(o.Equal("success"), "Query execution failed: %s", status)
	metricValue := jsonResult.Get("data.result.0.value.1").String()
	return metricValue
}

func getKubeletVersionWithRetry(oc *exutil.CLI, label string) (string, error) {
	var version string
	var err error
	for i := 0; i < 5; i++ {
		version, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l="+label, "-o=jsonpath={.items[0].status.nodeInfo.kubeletVersion}").Output()
		if err == nil && version != "" {
			return version, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("failed to get kubelet version after retries: %v", err)
}
