package winc

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var (
	mcoNamespace     = "openshift-machine-api"
	capiNamespace    = "openshift-cluster-api"
	wmcoNamespace    = "openshift-windows-machine-config-operator"
	wmcoDeployment   = "deployment.apps/windows-machine-config-operator"
	iaasPlatform     string
	windowsNodeLabel = "kubernetes.io/os=windows"
	linuxNodeLabel   = "kubernetes.io/os=linux"
	defaultWindowsMS = "windows"
	machineLabel     = "machine.openshift.io/os-id=Windows"
)

// checkVersionAnnotationReady returns true if the WMCO version annotation is set on the node.
func checkVersionAnnotationReady(oc *exutil.CLI, windowsNodeName string) (bool, error) {
	msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", windowsNodeName, "-o=jsonpath={.metadata.annotations.windowsmachineconfig\\.openshift\\.io\\/version}").Output()
	if msg == "" {
		return false, err
	}
	return true, err
}

// getWindowsHostNames returns the hostnames of all Windows nodes in the cluster.
func getWindowsHostNames(oc *exutil.CLI) []string {
	winHostNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].status.addresses[?(@.type==\"Hostname\")].address}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if winHostNames == "" {
		return []string{}
	}
	return strings.Split(winHostNames, " ")
}

// getWindowsInternalIPs returns the internal IP addresses of all Windows nodes.
func getWindowsInternalIPs(oc *exutil.CLI) []string {
	winInternalIPs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].status.addresses[?(@.type==\"InternalIP\")].address}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if winInternalIPs == "" {
		return []string{}
	}
	return strings.Split(winInternalIPs, " ")
}

// truncatedVersion extracts the major.minor version (e.g. "go1.22") from a possibly quoted string.
func truncatedVersion(s string) string {
	re := regexp.MustCompile(`(\w+\.\d+)`)
	if m := re.FindString(strings.TrimSpace(s)); m != "" {
		return m
	}
	return strings.TrimSpace(s)
}

// getMetricsFromCluster computes expected metric values directly from cluster state for comparison.
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

// getWMCOVersionFromLogs extracts the WMCO version string from operator pod logs.
func getWMCOVersionFromLogs(oc *exutil.CLI) (string, error) {
	log, err := oc.AsAdmin().WithoutNamespace().
		Run("logs").Args(wmcoDeployment,
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

// matchKubeletVersion compares two kubelet versions, tolerating patch-level differences for z-stream releases.
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

// extractMetricValue parses a Prometheus query result JSON and returns the metric value.
func extractMetricValue(queryResult string) string {
	jsonResult := gjson.Parse(queryResult)
	status := jsonResult.Get("status").String()
	o.Expect(status).Should(o.Equal("success"), "Query execution failed: %s", status)
	metricValue := jsonResult.Get("data.result.0.value.1").String()
	return metricValue
}

// getKubeletVersionWithRetry fetches the kubelet version for nodes matching the label, retrying up to 5 times.
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
	return "", fmt.Errorf("failed to get kubelet version after retries: %w", err)
}

// getContainerdVersion returns the containerd version reported by a node's containerRuntimeVersion field.
func getContainerdVersion(oc *exutil.CLI, nodeIP string) string {
	msg, err := oc.AsAdmin().WithoutNamespace().
		Run("get").Args("node", nodeIP,
		"-o=jsonpath={.status.nodeInfo.containerRuntimeVersion}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())

	parts := strings.Split(string(msg), "containerd://")
	if len(parts) < 2 {
		e2e.Logf("containerd version not reported for node %s", nodeIP)
		return ""
	}
	return "v" + parts[1]
}

// getValueFromText finds a line containing searchVal and returns the text after the delimiter.
func getValueFromText(body []byte, searchVal string) string {
	lines := strings.Split(string(body), "\n")
	for _, field := range lines {
		if strings.Contains(field, searchVal) {
			return strings.TrimSpace(strings.Split(field, searchVal)[1])
		}
	}
	e2e.Logf("value for %q not found in text", searchVal)
	return ""
}

// isNone returns true if the cluster platform is None or no Windows MachineSets exist.
func isNone(oc *exutil.CLI) bool {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
	if err == nil && strings.ToLower(output) == "none" {
		return true
	}

	machineSets, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("machinesets", "-n", "openshift-machine-api", "-o=jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		e2e.Logf("Unable to query machinesets, assuming platform none: %v", err)
		return true
	}

	for _, ms := range strings.Split(machineSets, " ") {
		if strings.Contains(ms, "winworker") || strings.Contains(ms, defaultWindowsMS) {
			return false
		}
	}

	e2e.Logf("No Windows MachineSets found (checked for 'winworker' or '%s'), treating as platform none", defaultWindowsMS)
	return true
}

// execInPod runs a command inside a pod via oc exec, replacing SSH/bastion access (WINC-1931).
func execInPod(oc *exutil.CLI, namespace, resource string, cmd ...string) (string, error) {
	args := append([]string{"-n", namespace, resource, "--"}, cmd...)
	return oc.AsAdmin().WithoutNamespace().Run("exec").Args(args...).Output()
}

// extractInstanceID parses JSON output of nodes or machines and returns a map of name to instance ID.
func extractInstanceID(jsonData, resourceType string) (map[string]string, error) {
	e2e.Logf("Processing %s JSON data to extract provider IDs...", resourceType)

	items := gjson.Get(jsonData, "items")
	if !items.Exists() || len(items.Array()) == 0 {
		return nil, fmt.Errorf("no %s found", resourceType)
	}

	providerIDs := make(map[string]string)
	re := regexp.MustCompile(`.*/([^/]+)$`)

	for _, item := range items.Array() {
		name := item.Get("metadata.name").String()
		providerID := item.Get("spec.providerID").String()

		matches := re.FindStringSubmatch(providerID)
		if len(matches) != 2 {
			return nil, fmt.Errorf("invalid providerID format for %s %s: %s", resourceType, name, providerID)
		}

		instanceID := matches[1]
		e2e.Logf("Mapped %s %s to instance ID %s", resourceType, name, instanceID)
		providerIDs[name] = instanceID
	}

	if len(providerIDs) == 0 {
		return nil, fmt.Errorf("no valid %s found after parsing", resourceType)
	}

	e2e.Logf("Successfully retrieved %s provider IDs", resourceType)
	return providerIDs, nil
}

// waitUntilWMCOStatusChanged polls WMCO logs until the given message appears.
func waitUntilWMCOStatusChanged(oc *exutil.CLI, message string, sinceTime string) {
	pollInterval := 15 * time.Second
	timeout := 35 * time.Minute
	normalizedMessage := strings.ToLower(strings.ReplaceAll(message, " ", ""))

	waitLogErr := wait.Poll(pollInterval, timeout, func() (bool, error) {
		var logs string
		var err error

		if sinceTime == "" {
			logs, err = oc.AsAdmin().WithoutNamespace().Run("logs").
				Args(wmcoDeployment, "-n", wmcoNamespace).Output()
		} else {
			logs, err = oc.AsAdmin().WithoutNamespace().Run("logs").
				Args(wmcoDeployment, "-n", wmcoNamespace, "--since="+sinceTime).Output()
		}

		if err != nil {
			e2e.Logf("Error retrieving WMCO logs: %v", err)
			return false, nil
		}

		logLines := strings.Split(logs, "\n")
		for _, line := range logLines {
			normalizedLine := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(line), " ", ""))
			if strings.Contains(normalizedLine, normalizedMessage) {
				e2e.Logf("Message found in WMCO logs: %v", line)
				return true, nil
			}
		}

		e2e.Logf("Message '%v' not found in WMCO logs. Continuing to poll...", message)
		return false, nil
	})

	compat_otp.AssertWaitPollNoErr(waitLogErr, fmt.Sprintf("Failed to find '%v' in WMCO logs after %v", message, timeout))
}

// derivePublicKeyFromSecret reads the cloud-private-key secret and derives the SSH public key.
func derivePublicKeyFromSecret(oc *exutil.CLI) string {
	encodedKey, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"secret", "cloud-private-key", "-n", wmcoNamespace,
		"-o=jsonpath={.data.private-key\\.pem}").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get cloud-private-key secret")
	o.Expect(encodedKey).NotTo(o.BeEmpty(), "cloud-private-key secret has no private-key.pem data")

	privateKeyBytes, err := base64.StdEncoding.DecodeString(encodedKey)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to decode private key from secret")

	signer, err := ssh.ParsePrivateKey(privateKeyBytes)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to parse private key")

	pubKey := base64.StdEncoding.EncodeToString(signer.PublicKey().Marshal())
	e2e.Logf("Derived public key from cloud-private-key secret")
	return pubKey
}

// isBYOH returns true if the node has the BYOH label set to "true".
func isBYOH(oc *exutil.CLI, nodeName string) bool {
	byohLabel, err := oc.AsAdmin().WithoutNamespace().Run("get").
		Args("node", nodeName, "-o=jsonpath={.metadata.labels.windowsmachineconfig\\.openshift\\.io/byoh}").Output()
	return err == nil && strings.TrimSpace(byohLabel) == "true"
}
