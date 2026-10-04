package winc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var (
	mcoNamespace       = "openshift-machine-api"
	capiNamespace      = "openshift-cluster-api"
	wmcoNamespace      = "openshift-windows-machine-config-operator"
	wmcoDeployment     = "deployment.apps/windows-machine-config-operator"
	wmcoDeploymentName = "windows-machine-config-operator"
	iaasPlatform       string
	windowsNodeLabel   = "kubernetes.io/os=windows"
	linuxNodeLabel     = "kubernetes.io/os=linux"

	machineLabel      = "machine.openshift.io/os-id=Windows"
	windowsDebugImage = "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022"
	linuxDebugImage   = "registry.access.redhat.com/ubi9/ubi:latest"
	defaultWindowsMS  = "windows"
)

// Service represents a Windows service entry from the WICD windows-services ConfigMap.
type Service struct {
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	Bootstrap    bool     `json:"bootstrap"`
	Priority     int      `json:"priority"`
	Dependencies []string `json:"dependencies,omitempty"`
}

// checkVersionAnnotationReady returns true if the WMCO version annotation is set on the node.
func checkVersionAnnotationReady(oc *exutil.CLI, windowsNodeName string) (bool, error) {
	msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", windowsNodeName, "-o=jsonpath={.metadata.annotations.windowsmachineconfig\\.openshift\\.io\\/version}").Output()
	if msg == "" {
		return false, err
	}
	return true, err
}

func waitVersionAnnotationReady(oc *exutil.CLI, windowsNodeName string, interval, timeout time.Duration) {
	err := wait.Poll(interval, timeout, func() (bool, error) {
		return checkVersionAnnotationReady(oc, windowsNodeName)
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Timed out waiting for version annotation on node %s", windowsNodeName)
}

// reconfigurationStableChecks is how many consecutive clean polls waitWMCOReconfigurationComplete
// requires. WMCO does not cordon the node the instant it is asked to reconfigure: a node observed
// converged can start a fresh pass seconds later, so the absence of in-progress markers on a
// single poll cannot distinguish "finished" from "not started yet".
const reconfigurationStableChecks = 6

// waitWMCOReconfigurationComplete blocks until WMCO has finished reconfiguring the given Windows
// node and left it that way. A restored version annotation is not a sufficient completion signal:
// WICD rewrites it early, while WMCO still holds the node cordoned and labelled upgrading, pending
// a reboot, and with its CNI torn down. Pods scheduled onto a node in that state sit in
// ContainerCreating with "cni plugin not initialized" until they time out. Any test that triggers a
// reconfiguration must call this before returning, or it hands an unusable node to whatever runs
// next.
func waitWMCOReconfigurationComplete(oc *exutil.CLI, windowsNodeName string, timeout time.Duration) {
	const versionKey = `{.metadata.annotations.windowsmachineconfig\.openshift\.io/version}`
	const desiredVersionKey = `{.metadata.annotations.windowsmachineconfig\.openshift\.io/desired-version}`
	// The scalars come first so the maps, whose values are free-form, cannot contain the separator.
	const jsonPath = `{.spec.unschedulable}|{.status.conditions[?(@.type=="Ready")].status}|` +
		versionKey + `|` + desiredVersionKey + `|{.metadata.labels}{.metadata.annotations}`

	stable := 0
	pollErr := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"node", windowsNodeName, "-o=jsonpath="+jsonPath).Output()
		if err != nil {
			e2e.Logf("Error getting node %s: %v", windowsNodeName, err)
			stable = 0
			return false, nil
		}
		fields := strings.SplitN(output, "|", 5)
		if len(fields) != 5 {
			stable = 0
			return false, nil
		}
		unschedulable, ready, version, desiredVersion, metadata := fields[0], fields[1], fields[2], fields[3], fields[4]

		// An absent reboot-required annotation and upgrading label are both rendered as a missing
		// map key, and reboot-required carries an empty value, so match on the key names.
		switch {
		case unschedulable == "true":
			e2e.Logf("Node %s is still cordoned by WMCO", windowsNodeName)
		case ready != "True":
			e2e.Logf("Node %s is not Ready yet (Ready=%q)", windowsNodeName, ready)
		case version == "" || version != desiredVersion:
			e2e.Logf("Node %s has not converged yet (version=%q desired-version=%q)", windowsNodeName, version, desiredVersion)
		case strings.Contains(metadata, "windowsmachineconfig.openshift.io/upgrading"):
			e2e.Logf("Node %s is still labelled upgrading", windowsNodeName)
		case strings.Contains(metadata, "windowsmachineconfig.openshift.io/reboot-required"):
			e2e.Logf("Node %s is still pending a reboot", windowsNodeName)
		default:
			stable++
			e2e.Logf("Node %s looks reconfigured (%d/%d consecutive checks)", windowsNodeName, stable, reconfigurationStableChecks)
			return stable >= reconfigurationStableChecks, nil
		}
		stable = 0
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr,
		fmt.Sprintf("node %s did not settle after WMCO reconfiguration within %v", windowsNodeName, timeout))
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

// getValueFromText searches line-delimited text for a key and returns the value after the key.
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

// isNone returns true if the cluster platform is None or no Windows machines exist.
func isNone(oc *exutil.CLI) bool {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
	e2e.Logf("isNone: platform=%q err=%v", output, err)
	if err == nil && strings.ToLower(output) == "none" {
		return true
	}

	machines, mErr := oc.AsAdmin().WithoutNamespace().Run("get").Args("machines", "-l", machineLabel, "-n", "openshift-machine-api", "-o=jsonpath={.items[*].metadata.name}").Output()
	e2e.Logf("isNone: Windows machines (label %s)=%q err=%v", machineLabel, machines, mErr)

	if mErr != nil {
		e2e.Logf("Unable to query Windows machines: %v", mErr)
		return true
	}

	if strings.TrimSpace(machines) != "" {
		return false
	}

	e2e.Logf("No Windows machines found (label %s), treating as platform none", machineLabel)
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

// getNodeNameFromIP resolves a node's hostname from its InternalIP address using a Go template query.
func getNodeNameFromIP(oc *exutil.CLI, nodeIP string) string {
	goTpl := fmt.Sprintf(
		`{{range .items}}{{$name := .metadata.name}}{{range .status.addresses}}`+
			`{{if and (eq .type "InternalIP") (eq .address "%s")}}{{$name}}{{end}}`+
			`{{end}}{{end}}`, nodeIP)
	nodeName, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-o=go-template="+goTpl).Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to resolve node name for IP %s", nodeIP)
	nodeName = strings.TrimSpace(nodeName)
	o.Expect(nodeName).NotTo(o.BeEmpty(), "no node found with InternalIP %s", nodeIP)
	return nodeName
}

// debugWindowsNodesNotReady captures comprehensive debug information when Windows nodes fail to become Ready
func debugWindowsNodesNotReady(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("===== DEBUG: Windows Nodes Not Ready - Capturing Diagnostic Information (cancellable) =====")

	// 1. Get detailed status for all Windows nodes
	e2e.Logf("=== Windows Node Status ===")
	nodeOutput, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel,
		"-o=custom-columns=NAME:.metadata.name,STATUS:.status.conditions[?(@.type==\"Ready\")].status,REASON:.status.conditions[?(@.type==\"Ready\")].reason,MESSAGE:.status.conditions[?(@.type==\"Ready\")].message").Output()
	if err != nil {
		e2e.Logf("ERROR: Failed to get node status: %v", err)
	} else {
		e2e.Logf("Node status:\n%s", nodeOutput)
	}

	// 2. Get all node conditions for each Windows node (summary only, no sensitive data)
	e2e.Logf("=== Windows Node Conditions ===")
	nodeNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel,
		"-o=jsonpath={.items[*].metadata.name}").Output()
	if err == nil {
		for _, nodeName := range strings.Fields(nodeNames) {
			e2e.Logf("--- Node: %s ---", nodeName)
			conditionsOutput, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"node", nodeName,
				"-o=jsonpath={.status.conditions[*].type}").Output()
			if err == nil {
				e2e.Logf("Condition types: %s", conditionsOutput)
			}
		}
	}

	// 3. Get WMCO operator pod status (not logs, to avoid sensitive data)
	e2e.Logf("=== WMCO Operator Pod Status ===")
	wmcoPods, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pods", "-n", wmcoNamespace,
		"-l", "app=windows-machine-config-operator",
		"-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase").Output()
	if err != nil {
		e2e.Logf("ERROR: Failed to get WMCO pod status: %v", err)
	} else {
		e2e.Logf("WMCO pods:\n%s", wmcoPods)
	}

	// 4. Get event counts for Windows nodes (avoid raw messages with sensitive data)
	e2e.Logf("=== Events for Windows Nodes (summary) ===")
	if nodeNames != "" {
		for _, nodeName := range strings.Fields(nodeNames) {
			events, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"events", "--all-namespaces",
				"--field-selector", fmt.Sprintf("involvedObject.name=%s", nodeName),
				"--sort-by=.lastTimestamp",
				"-o=custom-columns=TYPE:.type,REASON:.reason").Output()
			if err == nil {
				e2e.Logf("Events for %s:\n%s", nodeName, events)
			}
		}
	}

	// 5. Get WICD pod status (if running)
	e2e.Logf("=== WICD Pod Status ===")
	wicdPods, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pods", "-n", wmcoNamespace,
		"-l", "app=windows-instance-config-daemon",
		"-o=custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,READY:.status.conditions[?(@.type==\"Ready\")].status").Output()
	if err != nil {
		e2e.Logf("ERROR: Failed to get WICD pods: %v", err)
	} else {
		e2e.Logf("WICD pods:\n%s", wicdPods)
	}

	// 6. Check Machine status (if using Machine API)
	e2e.Logf("=== Machine Status ===")
	machineOutput, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machines", "-n", "openshift-machine-api",
		"-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase,NODE:.status.nodeRef.name,PROVIDERSTATE:.status.providerStatus.instanceState").Output()
	if err != nil {
		e2e.Logf("INFO: No machines or failed to query: %v", err)
	} else {
		e2e.Logf("Machines:\n%s", machineOutput)
	}

	// 7. Get Windows service status from NotReady nodes (avoid raw logs with sensitive data)
	e2e.Logf("=== Windows Node Service Status ===")
	if nodeNames != "" {
		for _, nodeName := range strings.Fields(nodeNames) {
			// Check context cancellation before each node capture
			select {
			case <-ctx.Done():
				e2e.Logf("Diagnostic timeout reached, stopping node captures")
				break
			default:
			}

			// Check if node is NotReady
			nodeReady, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"node", nodeName,
				"-o=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}").Output()
			if err == nil && strings.TrimSpace(nodeReady) != "True" {
				e2e.Logf("--- Service status from NotReady node: %s ---", nodeName)

				// Get Windows service status (from host, not container)
				serviceStatus, err := runHostProcessPS(oc, nodeName, windowsDebugImage,
					"Get-Service kubelet,windows-instance-config-daemon,containerd | Format-Table -AutoSize | Out-String -Width 200")
				if err != nil {
					e2e.Logf("ERROR: Failed to get service status from %s: %v", nodeName, err)
				} else {
					e2e.Logf("Windows service status on %s:\n%s", nodeName, serviceStatus)
				}
			}
		}
	}

	e2e.Logf("===== END DEBUG =====")
}

// waitWindowsNodesReady polls until the expected number of Windows nodes report Ready status.
// After 5 minutes of waiting, it captures debug information with a 2-minute deadline to avoid
// consuming the caller's entire timeout budget.
func waitWindowsNodesReady(oc *exutil.CLI, expectedCount int, timeout time.Duration) {
	debugCaptured := false
	debugThreshold := 5 * time.Minute
	diagnosticTimeout := 2 * time.Minute
	startTime := time.Now()

	pollErr := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"nodes", "-l", windowsNodeLabel,
			"-o=jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}").Output()
		if err != nil {
			e2e.Logf("Error querying Windows nodes: %v", err)
			return false, nil
		}
		statuses := strings.Fields(output)
		readyCount := 0
		for _, s := range statuses {
			if s == "True" {
				readyCount++
			}
		}
		e2e.Logf("Windows nodes ready: %d/%d", readyCount, expectedCount)

		// Capture debug info if we've been waiting too long and haven't captured yet
		if !debugCaptured && time.Since(startTime) > debugThreshold && readyCount < expectedCount {
			e2e.Logf("WARNING: Windows nodes not ready after %v, capturing debug information (timeout: %v)...", debugThreshold, diagnosticTimeout)
			ctx, cancel := context.WithTimeout(context.Background(), diagnosticTimeout)
			debugWindowsNodesNotReady(ctx, oc)
			cancel()
			debugCaptured = true
		}

		return readyCount >= expectedCount, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("timed out waiting for %d Windows nodes to be Ready after %v", expectedCount, timeout))
}

// getLatestServicesCMData returns the services JSON data from the most recently created windows-services ConfigMap
func getLatestServicesCMData(oc *exutil.CLI) (string, error) {
	cmNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"configmap", "-n", wmcoNamespace,
		"-o=jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		return "", fmt.Errorf("failed to list ConfigMaps: %w", err)
	}
	var latestCM string
	for _, name := range strings.Fields(cmNames) {
		if strings.HasPrefix(name, "windows-services-") {
			latestCM = name
		}
	}
	if latestCM == "" {
		return "", fmt.Errorf("no windows-services ConfigMap found in %s", wmcoNamespace)
	}
	servicesData, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"configmap", latestCM, "-n", wmcoNamespace,
		"-o=jsonpath={.data.services}").Output()
	if err != nil {
		return "", fmt.Errorf("failed to get services data from ConfigMap %s: %w", latestCM, err)
	}
	return servicesData, nil
}

// getServiceCommand returns the command for the named service from the services ConfigMap JSON data
func getServiceCommand(servicesJSON, serviceName string) string {
	result := gjson.Parse(servicesJSON)
	for _, svc := range result.Array() {
		if svc.Get("name").String() == serviceName {
			return svc.Get("path").String()
		}
	}
	return ""
}

// waitWindowsNodeReady polls until a specific Windows node reports Ready status.
func waitWindowsNodeReady(oc *exutil.CLI, nodeName string, timeout time.Duration) {
	pollErr := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		status, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"node", nodeName,
			"-o=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}").Output()
		if err != nil {
			e2e.Logf("Node %s not found yet: %v", nodeName, err)
			return false, nil
		}
		return strings.TrimSpace(status) == "True", nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("timed out waiting for node %s to be Ready after %v", nodeName, timeout))
}

// runDebugNodePS runs a PowerShell command on a Windows node via oc debug node.
// Suitable for host filesystem operations (e.g. Test-Path, Get-Content on C:\host\...) but
// NOT for Windows service queries -- oc debug does not create a HostProcess container, so
// Get-Service, sc.exe, etc. only see the container's SCM. Use runHostProcessPS for those.
func runDebugNodePS(oc *exutil.CLI, nodeName, image, psCommand string) (string, error) {
	output, err := oc.AsAdmin().WithoutNamespace().Run("debug").Args(
		"node/"+nodeName,
		"-n", wmcoNamespace,
		"--image="+image,
		"--", "pwsh", "-Command", psCommand).Output()
	if err != nil {
		return "", fmt.Errorf("oc debug node/%s failed: %w\noutput: %s", nodeName, err, output)
	}
	var cleaned []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Starting pod") ||
			strings.HasPrefix(trimmed, "Removing debug pod") ||
			trimmed == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n"), nil
}

// runHostProcessPS runs a PowerShell command on a Windows node using a HostProcess container.
// Unlike runDebugNodePS, this creates an explicit HostProcess pod with hostProcess=true and
// runAsUserName="NT AUTHORITY\SYSTEM", giving the process full access to the host's Service
// Control Manager, processes, and registry. Required for Get-Service, sc.exe, Stop-Service,
// Get-CimInstance, and any other command that needs to interact with host-level Windows
// services or WMI. The pod is created via oc run with JSON overrides that set both pod-level
// and container-level securityContext for HostProcess, polled until completion, and cleaned
// up on exit. Pass waitForCompletion=false for fire-and-forget operations where the pod is
// expected to self-destruct (e.g. stopping containerd).
func runHostProcessPS(oc *exutil.CLI, nodeName, image, psCommand string, waitForCompletion ...bool) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random suffix: %w", err)
	}
	suffix := fmt.Sprintf("%x", b)
	nodeSafe := strings.ReplaceAll(nodeName, ".", "-")
	if len(nodeSafe) > 20 {
		nodeSafe = nodeSafe[:20]
	}
	podName := fmt.Sprintf("hpc-%s-%s", nodeSafe, suffix)

	hostProcess := true
	runAsUser := "NT AUTHORITY\\SYSTEM"
	winOpts := map[string]interface{}{
		"hostProcess":   hostProcess,
		"runAsUserName": runAsUser,
	}
	overrides := map[string]interface{}{
		"spec": map[string]interface{}{
			"hostNetwork": true,
			"os":          map[string]string{"name": "windows"},
			"nodeSelector": map[string]string{
				"kubernetes.io/hostname": nodeName,
			},
			"tolerations": []map[string]string{
				{"key": "os", "operator": "Equal", "value": "Windows", "effect": "NoSchedule"},
			},
			"securityContext": map[string]interface{}{
				"windowsOptions": winOpts,
			},
			"containers": []map[string]interface{}{
				{
					"name":    podName,
					"image":   image,
					"command": []string{"powershell.exe", "-Command", psCommand},
					"securityContext": map[string]interface{}{
						"windowsOptions": winOpts,
					},
				},
			},
		},
	}
	overridesJSON, err := json.Marshal(overrides)
	if err != nil {
		return "", fmt.Errorf("failed to marshal HostProcess pod overrides: %w", err)
	}

	e2e.Logf("[HostProcess] Creating pod %s on node %s", podName, nodeName)
	e2e.Logf("[HostProcess] Command: %s", psCommand)
	e2e.Logf("[HostProcess] Overrides: %s", string(overridesJSON))

	createOutput, err := oc.AsAdmin().WithoutNamespace().Run("run").Args(
		podName,
		"-n", wmcoNamespace,
		"--image="+image,
		"--restart=Never",
		"--override-type=merge",
		"--overrides="+string(overridesJSON),
	).Output()
	if err != nil {
		e2e.Logf("[HostProcess] Failed to create pod %s: %v, output: %s", podName, err, createOutput)
		return "", fmt.Errorf("failed to create HostProcess pod on %s: %w", nodeName, err)
	}
	e2e.Logf("[HostProcess] Pod %s created successfully", podName)

	cleanupPod := func() {
		e2e.Logf("[HostProcess] Cleaning up pod %s", podName)
		if delErr := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"pod", podName, "-n", wmcoNamespace, "--ignore-not-found", "--wait=false").Execute(); delErr != nil {
			e2e.Logf("[HostProcess] Warning: failed to delete pod %s: %v", podName, delErr)
		}
	}

	if len(waitForCompletion) > 0 && !waitForCompletion[0] {
		e2e.Logf("[HostProcess] Pod %s created, returning without waiting (fire-and-forget)", podName)
		defer cleanupPod()
		return "", nil
	}

	defer cleanupPod()

	pollErr := wait.Poll(1*time.Second, 10*time.Minute, func() (bool, error) {
		phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"pod", podName, "-n", wmcoNamespace, "-o=jsonpath={.status.phase}").Output()
		if err != nil {
			e2e.Logf("[HostProcess] Poll: error getting phase for %s: %v", podName, err)
			return false, nil
		}
		p := strings.TrimSpace(phase)
		e2e.Logf("[HostProcess] Poll: pod %s phase=%s", podName, p)
		return p == "Succeeded" || p == "Failed", nil
	})
	if pollErr != nil {
		describeOutput, _ := oc.AsAdmin().WithoutNamespace().Run("describe").Args(
			"pod", podName, "-n", wmcoNamespace).Output()
		e2e.Logf("[HostProcess] Pod %s timed out. Describe:\n%s", podName, describeOutput)
		return "", fmt.Errorf("HostProcess pod %s did not complete: %w", podName, pollErr)
	}

	phase, phaseErr := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pod", podName, "-n", wmcoNamespace, "-o=jsonpath={.status.phase}").Output()
	if phaseErr != nil {
		return "", fmt.Errorf("failed to get HostProcess pod %s phase: %w", podName, phaseErr)
	}
	e2e.Logf("[HostProcess] Pod %s final phase: %s", podName, strings.TrimSpace(phase))

	output, logErr := oc.AsAdmin().WithoutNamespace().Run("logs").Args(
		podName, "-n", wmcoNamespace).Output()
	if logErr != nil {
		return "", fmt.Errorf("failed to get HostProcess pod logs: %w", logErr)
	}
	e2e.Logf("[HostProcess] Pod %s output: %s", podName, strings.TrimSpace(output))

	if strings.TrimSpace(phase) == "Failed" {
		return "", fmt.Errorf("HostProcess command failed on %s: %s", nodeName, output)
	}

	return strings.TrimSpace(output), nil
}

// createResourceFromString writes a YAML manifest to a temp file and applies it via oc apply.
func createResourceFromString(oc *exutil.CLI, namespace, manifest string) error {
	tempFile, err := os.CreateTemp("", "manifest-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempFileName := tempFile.Name()
	defer os.Remove(tempFileName)

	if _, err := tempFile.WriteString(manifest); err != nil {
		tempFile.Close()
		return fmt.Errorf("failed to write manifest to temp file: %w", err)
	}
	tempFile.Close()

	var applyErr error
	if namespace != "" {
		_, applyErr = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", tempFileName, "-n", namespace).Output()
	} else {
		_, applyErr = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", tempFileName).Output()
	}
	if applyErr != nil {
		return fmt.Errorf("failed to apply manifest: %w", applyErr)
	}
	return nil
}

// getRandomString returns a cryptographically random base64 string of the given length.
func getRandomString(length int) string {
	o.Expect(length).To(o.BeNumerically(">", 0), "getRandomString requires a positive length")
	buff := make([]byte, length)
	_, err := rand.Read(buff)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to generate random bytes")
	str := base64.StdEncoding.EncodeToString(buff)
	return str[:length]
}

// createProject creates a namespace if it does not already exist and sets privileged SCC.
// A namespace left behind by a previous attempt may still be Terminating, and the API server
// rejects every object created in one ("unable to create new content in namespace ... because it
// is being terminated"). Such a namespace is not reusable, so wait for it to disappear first.
func createProject(oc *exutil.CLI, namespace string) {
	phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"namespace", namespace, "-o=jsonpath={.status.phase}").Output()
	if err == nil {
		if strings.TrimSpace(phase) != "Terminating" {
			e2e.Logf("Namespace %s already exists, skipping creation", namespace)
			return
		}
		e2e.Logf("Namespace %s is Terminating, waiting for deletion to complete before recreating it", namespace)
		pollErr := wait.Poll(5*time.Second, 5*time.Minute, func() (bool, error) {
			gone := oc.AsAdmin().WithoutNamespace().Run("get").Args("namespace", namespace).Execute() != nil
			return gone, nil
		})
		compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("namespace %s did not finish terminating", namespace))
	}
	oc.CreateSpecifiedNamespaceAsAdmin(namespace)
	err = compat_otp.SetNamespacePrivileged(oc, namespace)
	o.Expect(err).NotTo(o.HaveOccurred())
}

// deleteProject deletes the given namespace.
func deleteProject(oc *exutil.CLI, namespace string) {
	oc.DeleteSpecifiedNamespaceAsAdmin(namespace)
}

// generateWindowsWebServerYAML returns a YAML manifest for a Windows web server Deployment
// (and optionally a LoadBalancer Service). Used to deploy Windows workloads for connectivity
// and scaling tests.
func generateWindowsWebServerYAML(name, namespace, image string, replicas int, includeService bool, resourceLimits, runtimeClassName string) string {
	var sb strings.Builder
	if includeService {
		sb.WriteString(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  labels:
    app: %s
spec:
  ports:
  - port: 80
    targetPort: 80
  selector:
    app: %s
  type: LoadBalancer
---
`, name, name, name))
	}
	sb.WriteString(fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: %s
  name: %s
spec:
  selector:
    matchLabels:
      app: %s
  replicas: %d
  template:
    metadata:
      labels:
        app: %s
      name: %s
    spec:
`, name, name, name, replicas, name, name))
	if runtimeClassName != "" {
		sb.WriteString(fmt.Sprintf("      runtimeClassName: %s\n", runtimeClassName))
	}
	sb.WriteString(`      tolerations:
      - key: "os"
        value: "Windows"
        operator: Equal
        effect: "NoSchedule"
      - key: "os"
        value: "windows"
        operator: Equal
        effect: "NoSchedule"
      containers:
      - name: windowswebserver
        image: ` + image + `
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsNonRoot: false
          windowsOptions:
            runAsUserName: "ContainerAdministrator"
`)
	if resourceLimits != "" {
		sb.WriteString(fmt.Sprintf(`        resources:
          limits:
            cpu: %s
            memory: 1Gi
          requests:
            cpu: %s
            memory: 512Mi
`, resourceLimits, resourceLimits))
	}
	sb.WriteString(`        command:
        - pwsh.exe
        - -command
        - $listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start();Write-Host('Listening at http://*:80/'); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $content='<html><body><H1>Windows Container Web Server</H1></body></html>'; $buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; $response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); };
      nodeSelector:
        kubernetes.io/os: windows
`)
	return sb.String()
}

// generateHPAYAML returns a YAML manifest for a HorizontalPodAutoscaler targeting a Deployment.
func generateHPAYAML(name, namespace, deploymentName string, minReplicas, maxReplicas, stabilizationWindow int, metricName, averageValue string) string {
	yaml := fmt.Sprintf(`apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: %s
  namespace: %s
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: %s
  minReplicas: %d
  maxReplicas: %d
  metrics:
  - type: Resource
    resource:
      name: %s
      target:
        type: AverageValue
        averageValue: %s
`, name, namespace, deploymentName, minReplicas, maxReplicas, metricName, averageValue)
	if stabilizationWindow > 0 {
		yaml += fmt.Sprintf(`  behavior:
    scaleDown:
      stabilizationWindowSeconds: %d
`, stabilizationWindow)
	}
	return yaml
}

// generateRuntimeClassYAML returns a YAML manifest for a Windows RuntimeClass with node selector and tolerations.
func generateRuntimeClassYAML(name, buildID string) string {
	return fmt.Sprintf(`apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: %s
handler: runhcs-wcow-process
scheduling:
  nodeSelector:
    kubernetes.io/os: windows
    node.kubernetes.io/windows-build: "%s"
  tolerations:
  - key: os
    value: Windows
    effect: NoSchedule
`, name, buildID)
}

// waitForDeploymentReady polls until all replicas of a Deployment are ready, or returns an error
// on timeout, ImagePullBackOff, or CrashLoopBackOff. Logs diagnostic info on failure.
func waitForDeploymentReady(oc *exutil.CLI, deploymentName, namespace string, timeout time.Duration) error {
	var lastPodStatus string
	imagePullBackOffCount := 0

	err := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("deployment", deploymentName,
			"-n", namespace,
			"-o", "jsonpath={.status.replicas},{.status.readyReplicas}").Output()
		if err != nil {
			return false, nil
		}

		parts := strings.Split(strings.TrimSpace(output), ",")
		if len(parts) != 2 {
			return false, nil
		}

		replicas, _ := strconv.Atoi(parts[0])
		readyReplicas, _ := strconv.Atoi(parts[1])

		if replicas > 0 && replicas == readyReplicas {
			return true, nil
		}

		podStatus, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods",
			"-n", namespace,
			"-l", "app="+deploymentName,
			"-o", "jsonpath={.items[*].status.containerStatuses[*].state}").Output()

		if err == nil && podStatus != "" {
			lastPodStatus = podStatus

			if strings.Contains(podStatus, "ImagePullBackOff") || strings.Contains(podStatus, "ErrImagePull") {
				imagePullBackOffCount++
				if imagePullBackOffCount >= 3 {
					return false, fmt.Errorf("ImagePullBackOff detected - image cannot be pulled")
				}
			} else {
				imagePullBackOffCount = 0
			}

			if strings.Contains(podStatus, "CrashLoopBackOff") {
				return false, fmt.Errorf("CrashLoopBackOff detected - container crashing on start")
			}
		}

		return false, nil
	})

	if err != nil {
		e2e.Logf("Deployment %s failed to become ready in namespace %s", deploymentName, namespace)
		e2e.Logf("Last pod status: %s", lastPodStatus)

		podList, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("pods",
			"-n", namespace,
			"-l", "app="+deploymentName,
			"-o", "wide").Output()
		e2e.Logf("Pod list:\n%s", podList)

		events, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args("events",
			"-n", namespace,
			"--field-selector", "involvedObject.kind=Pod",
			"--sort-by", ".lastTimestamp").Output()
		e2e.Logf("Pod events:\n%s", events)

		deployStatus, _ := oc.AsAdmin().WithoutNamespace().Run("describe").Args("deployment", deploymentName,
			"-n", namespace).Output()
		e2e.Logf("Deployment status:\n%s", deployStatus)
	}

	if err != nil {
		return fmt.Errorf("deployment %s in namespace %s did not become ready within %v: %w", deploymentName, namespace, timeout, err)
	}
	return nil
}

// checkWorkloadCreated returns true if the Deployment has exactly the expected number of ready replicas.
func checkWorkloadCreated(oc *exutil.CLI, name, namespace string, replicaCount int) bool {
	readyReplicas, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"deployment", name, "-n", namespace, "-o=jsonpath={.status.readyReplicas}",
	).Output()

	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return replicaCount == 0
		}
		e2e.Logf("Failed to get deployment %s, retrying: %v", name, err)
		return false
	}

	if readyReplicas == "" {
		return replicaCount == 0
	}

	numberOfWorkloads, err := strconv.Atoi(readyReplicas)
	if err != nil {
		e2e.Logf("Could not parse readyReplicas count '%s' for deployment %s: %v", readyReplicas, name, err)
		return false
	}

	return numberOfWorkloads == replicaCount
}

// scaleDeployment scales a Deployment to the given replica count and waits until the desired
// number of ready replicas is reached (up to 30 minutes).
func scaleDeployment(oc *exutil.CLI, deploymentName string, replicas int, namespace string) error {
	_, err := oc.AsAdmin().WithoutNamespace().Run("scale").
		Args("--replicas="+strconv.Itoa(replicas), "deployment", deploymentName, "-n", namespace).Output()
	if err != nil {
		return fmt.Errorf("failed to scale deployment %s to %d replicas: %w", deploymentName, replicas, err)
	}

	pollErr := wait.Poll(20*time.Second, 30*time.Minute, func() (bool, error) {
		return checkWorkloadCreated(oc, deploymentName, namespace, replicas), nil
	})
	if pollErr != nil {
		return fmt.Errorf("deployment %s did not reach %d replicas within 30 minutes: %w", deploymentName, replicas, pollErr)
	}

	return nil
}

// getExternalIP polls for the LoadBalancer external IP (or hostname on AWS) assigned to a Service.
func getExternalIP(iaasPlatform string, oc *exutil.CLI, deploymentName string, namespace string) (string, error) {
	var cmdArgs []string
	if iaasPlatform == "azure" || iaasPlatform == "gcp" {
		cmdArgs = []string{"get", "service", deploymentName, "-o=jsonpath={.status.loadBalancer.ingress[0].ip}", "-n", namespace}
	} else {
		cmdArgs = []string{"get", "service", deploymentName, "-o=jsonpath={.status.loadBalancer.ingress[0].hostname}", "-n", namespace}
	}

	lbTimeout := 5 * time.Minute
	var extIP string
	pollErr := wait.Poll(2*time.Second, lbTimeout, func() (bool, error) {
		output, err := oc.AsAdmin().WithoutNamespace().Run(cmdArgs[0]).Args(cmdArgs[1:]...).Output()
		if err != nil {
			e2e.Logf("Error retrieving external IP, retrying: %v", err)
			return false, nil
		}
		extIP = output
		e2e.Logf("%v ExternalIP is %v", iaasPlatform, extIP)
		if extIP == "" {
			e2e.Logf("External IP is empty, trying next round")
			return false, nil
		}
		return true, nil
	})

	if pollErr != nil {
		return "", fmt.Errorf("failed to get LoadBalancer IP after %v: %w", lbTimeout, pollErr)
	}
	return extIP, nil
}

// haveMetricsServer returns true if the metrics API service (v1beta1.metrics.k8s.io) is available.
func haveMetricsServer(oc *exutil.CLI) bool {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("apiservice", "v1beta1.metrics.k8s.io").Output()
	return err == nil && strings.Contains(output, "True")
}

// getWindowsBuildID returns the Windows build number label from a node (e.g. "10.0.20348").
func getWindowsBuildID(oc *exutil.CLI, nodeID string) (string, error) {
	build, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("node", nodeID, "-o=jsonpath={.metadata.labels.node\\.kubernetes\\.io\\/windows-build}").Output()
	return build, err
}

// maxLBConnectivityFailures is how many consecutive failed checks checkConnectivity tolerates
// before reporting the load balancer as broken. A freshly created cloud load balancer has a DNS
// name that is not yet consistently resolvable, so an isolated curl failure says nothing about
// whether the service stayed up. Only a sustained run of failures does.
const maxLBConnectivityFailures = 3

// checkConnectivity repeatedly curls the given IP on port 80 and verifies the Windows web server
// response. Runs until the context is cancelled. Used with runInBackground for load-testing.
func checkConnectivity(ctx context.Context, IP string, delay int) error {
	url := "http://" + net.JoinHostPort(IP, "80")
	timeout := strconv.Itoa(delay)
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		time.Sleep(time.Duration(delay) * time.Second)
		curl := exec.CommandContext(ctx, "curl", "--connect-timeout", timeout, "-s", url)
		out, err := curl.Output()
		if ctx.Err() != nil {
			return nil
		}

		var checkErr error
		switch {
		case err != nil:
			checkErr = fmt.Errorf("curl to %s failed: %v (output: %s)", url, err, string(out))
		case !strings.Contains(string(out), "Windows Container Web Server"):
			checkErr = fmt.Errorf("unexpected response from LB %s: %s", url, string(out))
		}
		if checkErr == nil {
			failures = 0
			e2e.Logf("Checked LB connectivity of %s", url)
			continue
		}

		failures++
		if failures >= maxLBConnectivityFailures {
			return fmt.Errorf("LB %s failed %d consecutive connectivity checks, last error: %w",
				url, failures, checkErr)
		}
		e2e.Logf("LB connectivity check %d/%d failed, retrying: %v", failures, maxLBConnectivityFailures, checkErr)
	}
}

// generateLinuxWebServerYAML returns a YAML manifest for a Linux web server Deployment using python http.server.
func generateLinuxWebServerYAML(name, namespace, image string, replicas int) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  labels:
    app: %s
  name: %s
spec:
  selector:
    matchLabels:
      app: %s
  replicas: %d
  template:
    metadata:
      labels:
        app: %s
      name: %s
    spec:
      containers:
      - name: linux-webserver
        image: %s
        ports:
        - containerPort: 8080
        command:
        - /bin/bash
        - -c
        - |
          cat > /tmp/index.html <<'HTMLEOF'
          <html><body><H1>Linux Container Web Server</H1></body></html>
          HTMLEOF
          cd /tmp && /usr/libexec/platform-python -m http.server 8080
      nodeSelector:
        kubernetes.io/os: linux
`, name, name, name, replicas, name, name, image)
}

// getWorkloadsNames returns the pod names for all pods belonging to the given Deployment, sorted by host IP.
func getWorkloadsNames(oc *exutil.CLI, deploymentName string, namespace string) ([]string, error) {
	workloads, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "--selector", "app="+deploymentName, "--sort-by=.status.hostIP", "-o=jsonpath={.items[*].metadata.name}", "-n", namespace).Output()
	if err != nil {
		return nil, err
	}
	pods := strings.Fields(workloads)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no pods found for deployment %s in namespace %s", deploymentName, namespace)
	}
	return pods, nil
}

// getWorkloadsIP returns the pod IPs for all pods belonging to the given Deployment, sorted by host IP.
func getWorkloadsIP(oc *exutil.CLI, deploymentName string, namespace string) ([]string, error) {
	workloads, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "--selector", "app="+deploymentName, "--sort-by=.status.hostIP", "-o=jsonpath={.items[*].status.podIP}", "-n", namespace).Output()
	if err != nil {
		return nil, err
	}
	ips := strings.Fields(workloads)
	if len(ips) == 0 {
		return nil, fmt.Errorf("no pod IPs found for deployment %s in namespace %s", deploymentName, namespace)
	}
	return ips, nil
}

// buildInvokeWebRequestCommand returns a PowerShell command that fetches a URL and decodes the response as UTF-8.
func buildInvokeWebRequestCommand(url string) string {
	return fmt.Sprintf(
		"$r = Invoke-WebRequest -Uri %s -UseBasicParsing -ErrorAction SilentlyContinue; "+
			"if ($r.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($r.Content) } else { $r.Content }",
		url)
}

// runInBackground launches a check function (e.g. checkConnectivity) in a goroutine and returns
// a channel that receives the error when the function exits. Cancels the context on error.
func runInBackground(ctx context.Context, cancel context.CancelFunc, check func(context.Context, string, int) error, val string, delay int) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		defer g.GinkgoRecover()
		err := check(ctx, val, delay)
		if err != nil {
			cancel()
			e2e.Logf("Error during invocation of %v(%v,%v): %v", runtime.FuncForPC(reflect.ValueOf(check).Pointer()).Name(), val, delay, err.Error())
		}
		errCh <- err
	}()
	return errCh
}

// getLatestServicesCMName returns the name of the last windows-services-* ConfigMap found in the
// WMCO namespace. The iteration order matches OTP's popItemFromList (last match wins).
func getLatestServicesCMName(oc *exutil.CLI) (string, error) {
	cmNames, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"configmap", "-n", wmcoNamespace,
		"-o=jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		return "", fmt.Errorf("failed to list ConfigMaps: %w", err)
	}
	var latestCM string
	for _, name := range strings.Fields(cmNames) {
		if strings.HasPrefix(name, "windows-services-") {
			latestCM = name
		}
	}
	if latestCM == "" {
		return "", fmt.Errorf("no windows-services ConfigMap found in %s", wmcoNamespace)
	}
	return latestCM, nil
}

// waitForServicesCM polls until getLatestServicesCMName returns the expected ConfigMap name.
// Used after deleting/recreating CMs to verify WMCO reconciles the correct version.
func waitForServicesCM(oc *exutil.CLI, expectedCMName string, timeout time.Duration) {
	pollErr := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		cmName, err := getLatestServicesCMName(oc)
		if err != nil || cmName == "" {
			return false, nil
		}
		if cmName == expectedCMName {
			return true, nil
		}
		e2e.Logf("ConfigMap %v does not match expected %v", cmName, expectedCMName)
		return false, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("Expected windows-services ConfigMap %s not found after %v", expectedCMName, timeout))
}

// generateWICDConfigMapYAML returns a YAML manifest for a WICD windows-services ConfigMap.
// Ported from OTP generators.go GenerateWICDConfigMap.
func generateWICDConfigMapYAML(name, servicesJSON string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: openshift-windows-machine-config-operator
data:
  services: '%s'
`, name, servicesJSON)
}

// checkWindowsServiceRunning returns true if the named Windows service is in Running state on the
// given node. Uses a HostProcess pod to query the host's Service Control Manager.
func checkWindowsServiceRunning(oc *exutil.CLI, nodeName, image, serviceName string) (bool, error) {
	cmd := fmt.Sprintf("Get-Service '%s' | Select-Object -ExpandProperty Status", serviceName)
	output, err := runHostProcessPS(oc, nodeName, image, cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == "Running", nil
}

// getServiceBinPath returns the PathName (binary path) of a Windows service via WMI CIM query.
// Uses a HostProcess pod to access the host's Service Control Manager.
func getServiceBinPath(oc *exutil.CLI, nodeName, image, serviceName string) (string, error) {
	cmd := fmt.Sprintf("Get-CimInstance -ClassName Win32_Service | Where-Object { $_.Name -eq '%s' } | Select-Object -ExpandProperty PathName", serviceName)
	output, err := runHostProcessPS(oc, nodeName, image, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// setServiceBinPath modifies the binary path of a Windows service using sc.exe config.
// Uses a HostProcess pod to access the host's Service Control Manager.
func setServiceBinPath(oc *exutil.CLI, nodeName, image, serviceName, binPath string) error {
	cmd := fmt.Sprintf(`sc.exe config %s binPath= "%s"`, serviceName, binPath)
	output, err := runHostProcessPS(oc, nodeName, image, cmd)
	if err != nil {
		return fmt.Errorf("sc.exe config failed: %w (output: %s)", err, output)
	}
	if !strings.Contains(output, "SUCCESS") {
		return fmt.Errorf("sc.exe config did not report SUCCESS: %s", output)
	}
	return nil
}
func stopWindowsService(oc *exutil.CLI, nodeName, image, serviceName string) (string, error) {
	cmd := fmt.Sprintf("Stop-Service '%s' -Force -ErrorAction SilentlyContinue; (Get-Service '%s').Status", serviceName, serviceName)
	output, err := runHostProcessPS(oc, nodeName, image, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// getAvailabilityZone returns the availability zone of the cluster's MachineSet or nodes.
func getAvailabilityZone(oc *exutil.CLI) string {
	zone, err := getMachineSetZone(oc)
	if err == nil && zone != "" {
		return zone
	}
	for _, label := range []string{windowsNodeLabel, linuxNodeLabel} {
		if z, err := getZoneFromNodes(oc, label); err == nil && z != "" {
			return z
		}
	}
	return ""
}

func getMachineSetZone(oc *exutil.CLI) (string, error) {
	var zoneQuery string
	if iaasPlatform == "gcp" {
		zoneQuery = "-o=jsonpath={.items[0].spec.template.spec.providerSpec.value.zone}"
	} else {
		zoneQuery = "-o=jsonpath={.items[0].spec.template.spec.providerSpec.value.placement.availabilityZone}"
	}
	return oc.AsAdmin().WithoutNamespace().Run("get").Args("machineset", "-n", mcoNamespace, zoneQuery).Output()
}

func getZoneFromNodes(oc *exutil.CLI, nodeLabel string) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", nodeLabel,
		"-o=jsonpath={.items[0].metadata.labels.topology\\.kubernetes\\.io/zone}").Output()
}

// getWindowsMachineSetName returns the name of the Windows MachineSet in the cluster.
// When looking for the default MachineSet, it queries the cluster directly to find
// one containing "winworker" or the defaultWindowsMS keyword.
func getWindowsMachineSetName(oc *exutil.CLI, name, platform, zone string) string {
	if name == defaultWindowsMS {
		machineSets, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"machinesets", "-n", mcoNamespace, "-o=jsonpath={.items[*].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		// Try pattern matching first
		for _, ms := range strings.Split(machineSets, " ") {
			if strings.Contains(ms, "winworker") || strings.Contains(ms, defaultWindowsMS) || strings.HasSuffix(ms, "-wm") {
				return ms
			}
		}

		// Fallback: Try CI e2e pattern (contains "-e2e" but not "worker")
		// CI e2e jobs use pattern like "ci-op-xxx-e2e" for Windows MachineSets
		for _, ms := range strings.Split(machineSets, " ") {
			if strings.Contains(ms, "-e2e") && !strings.Contains(ms, "worker") {
				// Verify this is actually a Windows MachineSet by checking the label
				msLabels, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
					"machineset", ms, "-n", mcoNamespace,
					"-o=jsonpath={.spec.template.metadata.labels.machine\\.openshift\\.io/os-id}").Output()
				if err == nil && strings.TrimSpace(msLabels) == "Windows" {
					e2e.Logf("Found Windows MachineSet using CI e2e pattern: %s", ms)
					return ms
				}
			}
		}

		// Final fallback: Get MachineSet from Windows node's Machine annotation
		// This handles cases where MachineSet naming is non-standard
		winHostNames := getWindowsHostNames(oc)
		if len(winHostNames) > 0 {
			machineAnnotation, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"node", winHostNames[0], "-o=jsonpath={.metadata.annotations.machine\\.openshift\\.io/machine}").Output()
			if err == nil && machineAnnotation != "" {
				// Extract Machine name from "openshift-machine-api/machine-name"
				parts := strings.Split(strings.TrimSpace(machineAnnotation), "/")
				if len(parts) == 2 {
					machineName := parts[1]
					// Get MachineSet from Machine's ownerReferences or labels
					machineSetLabel, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
						"machine", machineName, "-n", mcoNamespace,
						"-o=jsonpath={.metadata.labels.machine\\.openshift\\.io/cluster-api-machineset}").Output()
					if err == nil && machineSetLabel != "" {
						e2e.Logf("Found Windows MachineSet from node Machine annotation: %s", machineSetLabel)
						return strings.TrimSpace(machineSetLabel)
					}
				}
			}
		}

		e2e.Failf("Windows MachineSet not found in cluster. Found: %s", machineSets)
	}

	machinesetName := name
	if (platform == "vsphere" || platform == "nutanix") && name == "windows" {
		machinesetName = "winworker"
	}

	if platform == "aws" || platform == "gcp" {
		if zone == "" {
			zone = "us-central1-a"
		}
		infrastructureID, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"infrastructure", "cluster", "-o=jsonpath={.status.infrastructureName}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		switch platform {
		case "aws":
			machinesetName = infrastructureID + "-" + machinesetName + "-worker-" + zone
		case "gcp":
			zoneParts := strings.Split(zone, "-")
			if len(zoneParts) < 3 {
				e2e.Failf("GCP zone should have at least 3 segments, got: %s", zone)
			}
			machinesetName = infrastructureID + "-" + machinesetName + "-worker-" + zoneParts[2]
		}
	}

	return machinesetName
}

// getMachineSetReplicas returns the desired replica count of the given MachineSet. Tests that scale
// a MachineSet must capture this before scaling so cleanup restores the cluster's original size
// instead of assuming a fixed count.
func getMachineSetReplicas(oc *exutil.CLI, machineSetName string) int {
	replicas, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machinesets.machine.openshift.io", machineSetName, "-n", mcoNamespace,
		"-o=jsonpath={.spec.replicas}").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get replicas of MachineSet %s", machineSetName)

	count, err := strconv.Atoi(strings.TrimSpace(replicas))
	o.Expect(err).NotTo(o.HaveOccurred(), "Unexpected replica count %q for MachineSet %s", replicas, machineSetName)
	return count
}

// scaleWindowsMachineSet scales the Windows MachineSet to the specified replica count.
func scaleWindowsMachineSet(oc *exutil.CLI, machineSetName string, deadTime, replicas int, skipWait bool) {
	err := oc.AsAdmin().WithoutNamespace().Run("scale").Args(
		"--replicas="+strconv.Itoa(replicas),
		"machinesets.machine.openshift.io", machineSetName,
		"-n", mcoNamespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to scale Windows MachineSet")

	if !skipWait {
		waitForMachinesetReady(oc, machineSetName, deadTime, replicas)
	}
}

// cloneWindowsMachineSet creates a copy of the existing Windows MachineSet with a different name
// and zero replicas.
func cloneWindowsMachineSet(oc *exutil.CLI, sourceName, cloneName string) {
	// The caller deletes cloneName during cleanup. If the names match, that cleanup
	// destroys the cluster's own Windows MachineSet and every node it owns.
	o.Expect(cloneName).NotTo(o.Equal(sourceName),
		"clone MachineSet name must differ from source %s", sourceName)

	msJSON, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machinesets.machine.openshift.io", sourceName, "-n", mcoNamespace, "-o=json").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get source MachineSet %s", sourceName)

	msJSON = strings.ReplaceAll(msJSON, sourceName, cloneName)

	var ms map[string]interface{}
	err = json.Unmarshal([]byte(msJSON), &ms)
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to parse source MachineSet %s", sourceName)

	// Drop server-managed fields so the API server accepts this as a new object,
	// and start at zero replicas so no machine is provisioned before the caller scales up.
	delete(ms, "status")
	if metadata, ok := ms["metadata"].(map[string]interface{}); ok {
		for _, field := range []string{"resourceVersion", "uid", "creationTimestamp", "generation", "selfLink", "managedFields"} {
			delete(metadata, field)
		}
		if annotations, ok := metadata["annotations"].(map[string]interface{}); ok {
			delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
		}
	}
	if spec, ok := ms["spec"].(map[string]interface{}); ok {
		spec["replicas"] = 0
	}

	cloneJSON, err := json.Marshal(ms)
	o.Expect(err).NotTo(o.HaveOccurred())

	tmpFile, err := os.CreateTemp("", "machineset-*.json")
	o.Expect(err).NotTo(o.HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write(cloneJSON)
	o.Expect(err).NotTo(o.HaveOccurred())
	tmpFile.Close()

	// create, not apply: apply would silently mutate an existing MachineSet instead of cloning.
	err = oc.AsAdmin().WithoutNamespace().Run("create").Args("-f", tmpFile.Name()).Execute()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create cloned MachineSet %s", cloneName)
}

// extractPrivateKeyToFile reads the cloud-private-key secret and writes it to a temp file.
// Returns the file path. Caller is responsible for cleanup.
func extractPrivateKeyToFile(oc *exutil.CLI) string {
	encodedKey, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"secret", "cloud-private-key", "-n", wmcoNamespace,
		"-o=jsonpath={.data.private-key\\.pem}").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get cloud-private-key secret")
	o.Expect(encodedKey).NotTo(o.BeEmpty(), "cloud-private-key has no private-key.pem data")

	keyBytes, err := base64.StdEncoding.DecodeString(encodedKey)
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to decode private key")

	tmpFile, err := os.CreateTemp("", "cloud-private-key-*.pem")
	o.Expect(err).NotTo(o.HaveOccurred())
	_, err = tmpFile.Write(keyBytes)
	o.Expect(err).NotTo(o.HaveOccurred())
	tmpFile.Close()
	os.Chmod(tmpFile.Name(), 0600)

	e2e.Logf("Extracted private key to %s", tmpFile.Name())
	return tmpFile.Name()
}

// waitForMachinesetReady polls until the MachineSet has the expected number of ready replicas.
func waitForMachinesetReady(oc *exutil.CLI, machineSetName string, timeout, replicas int) {
	err := wait.Poll(1*time.Minute, time.Duration(timeout)*time.Minute, func() (bool, error) {
		readyReplicas, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"machineset", machineSetName, "-n", mcoNamespace,
			"-o=jsonpath={.status.readyReplicas}").Output()
		if err != nil {
			e2e.Logf("Error getting machineset %s: %v", machineSetName, err)
			return false, err
		}
		readyReplicasInt, _ := strconv.Atoi(readyReplicas)
		e2e.Logf("Waiting for machineset %s: %d/%d ready replicas", machineSetName, readyReplicasInt, replicas)
		return readyReplicasInt >= replicas, nil
	})
	if err != nil {
		e2e.Failf("machineset %s did not reach %d ready replicas within %d minutes", machineSetName, replicas, timeout)
	}
}

func getServiceClusterIP(oc *exutil.CLI, serviceName, namespace string) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"service", serviceName, "-o=jsonpath={.spec.clusterIP}", "-n", namespace).Output()
}

// generateClusterIPServiceYAML returns a YAML manifest for a ClusterIP Service.
func generateClusterIPServiceYAML(name, namespace, appLabel string, port int) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  ports:
  - port: %d
    targetPort: %d
  selector:
    app: %s
  type: ClusterIP
`, name, namespace, port, port, appLabel)
}

// generateWindowsDaemonSetYAML returns a YAML manifest for a Windows DaemonSet.
func generateWindowsDaemonSetYAML(name, namespace, appLabel, image string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: %s
  namespace: %s
spec:
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      nodeSelector:
        kubernetes.io/os: windows
      tolerations:
      - key: "os"
        operator: "Equal"
        value: "Windows"
        effect: "NoSchedule"
      containers:
      - name: %s
        image: %s
        command:
        - pwsh.exe
        - -Command
        - "while ($true) { Start-Sleep -Seconds 30 }"
`, name, namespace, appLabel, appLabel, name, image)
}

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

func waitForProxyOnNodes(oc *exutil.CLI, winNodes []string, wicdProxies map[string]interface{}) {
	pollErr := wait.Poll(20*time.Second, 5*time.Minute, func() (bool, error) {
		for _, nodeName := range winNodes {
			for key, proxy := range wicdProxies {
				proxyStr := fmt.Sprint(proxy)
				if proxyStr == "" {
					continue
				}
				cmd := fmt.Sprintf("[System.Environment]::GetEnvironmentVariable('%v', 'Machine')", key)
				msg, err := runHostProcessPS(oc, nodeName, windowsDebugImage, cmd)
				if err != nil {
					e2e.Logf("Waiting for %v on %v: error: %v", key, nodeName, err)
					return false, nil
				}
				if !strings.Contains(strings.TrimSpace(msg), proxyStr) {
					e2e.Logf("Waiting for proxy variable %s on node %s: expected=%s actual=%s",
						key, nodeName, redactProxyURL(proxyStr), redactProxyURL(strings.TrimSpace(msg)))
					return false, nil
				}
			}
		}
		e2e.Logf("All proxy values propagated to all Windows nodes")
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, "proxy values did not propagate to all Windows nodes within 5 minutes")
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

func popItemFromList(oc *exutil.CLI, value string, keywordSearch string, namespace string) (string, error) {
	rawList, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(value, "-n", namespace, "-o=jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		return "", err
	}
	for _, val := range strings.Split(rawList, " ") {
		if strings.Contains(val, keywordSearch) {
			return val, nil
		}
	}
	return "", nil
}

func waitForCM(oc *exutil.CLI, cmName string, cmType string, namespace string) {
	pollErr := wait.Poll(10*time.Second, 600*time.Second, func() (bool, error) {
		windowsCM, err := popItemFromList(oc, "configmap", cmType, namespace)
		if err != nil || windowsCM == "" {
			return false, nil
		}
		return windowsCM == cmName, nil
	})
	if pollErr != nil {
		e2e.Failf("Expected configmap %v not found after 10 minutes", cmName)
	}
}

func deleteResource(oc *exutil.CLI, resourceType string, resourceName string, namespace string) {
	err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(resourceType, resourceName, "-n", namespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
}

func checkUserCertificatesOnNodes(oc *exutil.CLI, commonName string, expectedCount int) {
	winNodes := getWindowsNodeNames(oc)
	for _, nodeName := range winNodes {
		e2e.Logf("Waiting for %d user certificate(s) with CN '%s' on node %s", expectedCount, commonName, nodeName)
		cmd := fmt.Sprintf("(Get-ChildItem -Path Cert:\\LocalMachine\\Root | Where-Object {$_.Subject -eq '%s'}).Count", commonName)

		pollErr := wait.Poll(10*time.Second, 10*time.Minute, func() (bool, error) {
			msg, err := runHostProcessPS(oc, nodeName, windowsDebugImage, cmd)
			if err != nil {
				e2e.Logf("Error checking certificates on node %s: %v", nodeName, err)
				return false, nil
			}
			numOfCerts := 0
			if trimmed := strings.TrimSpace(msg); trimmed != "" {
				found := regexp.MustCompile(`\d+`).FindString(trimmed)
				if found != "" {
					numOfCerts, _ = strconv.Atoi(found)
				}
			}
			if numOfCerts == expectedCount {
				e2e.Logf("Found %d certificate(s) on node %s", numOfCerts, nodeName)
				return true, nil
			}
			e2e.Logf("Waiting for certificates on node %s: expected %d, found %d", nodeName, expectedCount, numOfCerts)
			return false, nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "certificate count did not reach %d on node %s within 10 minutes", expectedCount, nodeName)
	}
}

func getConfigMapData(oc *exutil.CLI, cm string, dataKey string, namespace string) string {
	dataValue, err := oc.AsAdmin().WithoutNamespace().Run("get").
		Args("configmap", cm, "-o=jsonpath={.data."+dataKey+"}", "-n", namespace).Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get cm %v data key %v", cm, dataKey)
	return dataValue
}

func removeOuterQuotes(s string) string {
	if len(s) >= 2 {
		if c := s[len(s)-1]; s[0] == c && (c == '"' || c == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func configureCertificateToJSONPatch(oc *exutil.CLI, payload, configmap, namespace string) {
	// Collapse the blank line introduced when appending a certificate to the existing bundle
	payload = strings.Replace(payload, "\n\n", "\n", 1)
	// Marshal the payload so the PEM line breaks are escaped as \n and survive the patch.
	// Stripping them instead yields a single-line blob that is not valid PEM, which makes
	// WICD reject the whole ca-bundle.crt and import no certificates at all.
	var configMapPayload ConfigMapPayload
	configMapPayload.Data.CaBundleCrt = payload
	jsonBytes, err := json.Marshal(configMapPayload)
	o.Expect(err).NotTo(o.HaveOccurred(), "error marshalling ConfigMap patch")
	jsonPayload := string(jsonBytes)
	cmd := oc.AsAdmin().WithoutNamespace().Run("patch").Args("configmap", configmap, "-n", namespace, "-p", jsonPayload)
	output, err := cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitError.Stderr))
			err = fmt.Errorf("%v: %s", err, stderr)
		}
		o.Expect(err).NotTo(o.HaveOccurred(), "error patching ConfigMap. Output: %s", output)
	}
}

func extractStatusCode(output string) int {
	re := regexp.MustCompile(`HTTP/\d+\.?\d*\s+(\d{3})`)
	matches := re.FindStringSubmatch(output)
	if len(matches) >= 2 {
		statusCode, err := strconv.Atoi(matches[1])
		if err == nil {
			return statusCode
		}
	}
	return 0
}

func testTraffic(oc *exutil.CLI, testURL string, winNodes []string) {
	command := fmt.Sprintf("cmd.exe /c curl -vsIL %v", testURL)
	for _, nodeName := range winNodes {
		output, err := runHostProcessPS(oc, nodeName, windowsDebugImage, command)
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to execute curl on %v", nodeName)
		statusCode := extractStatusCode(output)
		o.Expect(statusCode).To(o.Equal(200), "expected status code 200 on %v from %v, but got %d", testURL, nodeName, statusCode)
	}
}

const (
	byohPoolConfigMap        = "windows-node-pool"
	byohInstancesConfigMap   = "windows-instances"
	byohPoolRequiredEnv      = "WMCO_BYOH_NODE_POOL_REQUIRED"
	byohResetPolicy          = "wmco-deconfigure-v1"
	byohPrivateKeySecret     = "cloud-private-key"
	byohPrivateKeyDataKey    = "private-key.pem"
	byohRegistrationOwner    = "windowsmachineconfig.openshift.io/test-registration-"
	byohPublicKeyHashAnno    = "windowsmachineconfig.openshift.io/pub-key-hash"
	byohUsernameAnno         = "windowsmachineconfig.openshift.io/username"
	byohVersionAnno          = "windowsmachineconfig.openshift.io/version"
	byohDesiredVersionAnno   = "windowsmachineconfig.openshift.io/desired-version"
	byohWorkloadDeployment   = "win-webserver"
	byohOperatorPodSelector  = "name=windows-machine-config-operator"
	byohOperatorContainer    = "manager"
	byohPoolPollTimeout      = 20 * time.Minute
	byohDeconfigurationLimit = 20 * time.Minute
	byohFixtureLimit         = 90 * time.Minute
	byohCleanupLimit         = 30 * time.Minute
	byohFallbackLimit        = 2 * time.Minute
)

var byohIDMSGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1",
	Resource: "imagedigestmirrorsets"}

var errBYOHRegistrationConflict = errors.New("registration address is already present in windows-instances")

type byohAddressType string

const (
	byohAddressIP  byohAddressType = "ip"
	byohAddressDNS byohAddressType = "dns"
)

type byohPoolEntry struct {
	Status, Username         string
	AddressType              byohAddressType
	Platform, TestID         string
	AllocatedAt, LastUpdated string
	HostID, ResetPolicy      string
	Disposable               bool
	SSHAddress               string
}

type byohHostRequest struct {
	AddressType byohAddressType
	Disposable  bool
}

type byohHostLease struct {
	Address, Username, SSHAddress string
	AddressType                   byohAddressType
	HostID, LeaseID               string
	Disposable                    bool
	NodeName                      string
	NodeUID                       types.UID
	NodeResourceVersion           string
	ClaimedAliases                []string
	ReconcileStarted              time.Time
	DeconfigurationStarted        time.Time
	RegistrationAttempted         bool
	RegistrationOwned             bool
	Registered, Unregistered      bool
	ResetVerified, CleanupDone    bool
	PreserveInstancesConfigMap    bool
	QuarantineRequired            bool
}

type byohInstancesState struct {
	observed          bool
	initiallyExisted  bool
	initialUID        types.UID
	ownedUID          types.UID
	originalOwnership map[string]*string
}

type byohTestFixture struct {
	oc              *exutil.CLI
	coreClient      corev1client.CoreV1Interface
	appsClient      appsv1client.AppsV1Interface
	dynamicClient   dynamic.Interface
	ctx             context.Context
	cancel          context.CancelFunc
	leases          []*byohHostLease
	workloadNS      string
	idmsName        string
	idmsLeaseID     string
	idmsAttempted   bool
	idmsOwned       bool
	idmsUID         types.UID
	instancesState  byohInstancesState
	secretSnapshot  *corev1.Secret
	secretLease     *byohHostLease
	secretRestored  bool
	originalKeyHash string
	waitForLeaseLog func(context.Context, *byohHostLease, string, time.Time) error
}

func newBYOHTestFixture(oc *exutil.CLI) *byohTestFixture {
	ctx, cancel := context.WithTimeout(context.Background(), byohFixtureLimit)
	return &byohTestFixture{oc: oc, coreClient: oc.AdminKubeClient().CoreV1(),
		appsClient: oc.AdminKubeClient().AppsV1(), dynamicClient: oc.AdminDynamicClient(), ctx: ctx, cancel: cancel}
}

// requireBYOHPool is deliberately the first operation in every Batch 9 case. Generic jobs opt out by
// not setting the environment variable; an opted-in job treats every inventory problem as a failure.
func (f *byohTestFixture) requireBYOHPool() error {
	if os.Getenv(byohPoolRequiredEnv) != "true" {
		g.Skip(fmt.Sprintf("%s is not true; the dedicated BYOH pool fixture is not enabled", byohPoolRequiredEnv))
	}
	cm, err := f.coreClient.ConfigMaps(wmcoNamespace).Get(f.ctx, byohPoolConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("required BYOH node pool is unavailable: %w", err)
	}
	_, err = validateBYOHPoolInventory(cm.Data)
	return err
}

func parseBYOHPoolEntry(address, raw string) (byohPoolEntry, error) {
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return byohPoolEntry{}, fmt.Errorf("pool entry has an invalid field")
		}
		key, value := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if _, exists := fields[key]; exists {
			return byohPoolEntry{}, fmt.Errorf("pool entry has a duplicate field")
		}
		fields[key] = value
	}
	allowed := map[string]bool{
		"status": true, "username": true, "address-type": true, "platform": true, "test-id": true,
		"allocated-at": true, "last-updated": true, "host-id": true, "reset-policy": true,
		"disposable": true, "ssh-address": true,
	}
	for key := range fields {
		if !allowed[key] {
			return byohPoolEntry{}, fmt.Errorf("pool entry contains an unsupported field")
		}
	}
	for key := range allowed {
		if _, present := fields[key]; !present {
			return byohPoolEntry{}, fmt.Errorf("pool entry is missing required field %s", key)
		}
	}
	for _, key := range []string{"status", "username", "address-type", "platform", "last-updated", "host-id",
		"reset-policy", "disposable", "ssh-address"} {
		if fields[key] == "" {
			return byohPoolEntry{}, fmt.Errorf("pool entry is missing required field %s", key)
		}
	}
	entry := byohPoolEntry{
		Status: fields["status"], Username: fields["username"], AddressType: byohAddressType(fields["address-type"]),
		Platform: fields["platform"], TestID: fields["test-id"], AllocatedAt: fields["allocated-at"],
		LastUpdated: fields["last-updated"], HostID: fields["host-id"], ResetPolicy: fields["reset-policy"],
		SSHAddress: fields["ssh-address"],
	}
	var err error
	entry.Disposable, err = strconv.ParseBool(fields["disposable"])
	if err != nil {
		return byohPoolEntry{}, fmt.Errorf("pool entry has invalid disposable value")
	}
	if entry.AddressType != byohAddressIP && entry.AddressType != byohAddressDNS {
		return byohPoolEntry{}, fmt.Errorf("pool entry has unsupported address type")
	}
	isIP := net.ParseIP(address) != nil
	if (entry.AddressType == byohAddressIP) != isIP {
		return byohPoolEntry{}, fmt.Errorf("pool entry address does not match its declared type")
	}
	switch entry.Status {
	case "available", "unavailable":
		if entry.TestID != "" || entry.AllocatedAt != "" {
			return byohPoolEntry{}, fmt.Errorf("unowned pool entry contains lease metadata")
		}
	case "allocated", "releasing":
		if entry.TestID == "" || entry.AllocatedAt == "" {
			return byohPoolEntry{}, fmt.Errorf("owned pool entry is missing lease metadata")
		}
		if _, err := time.Parse(time.RFC3339, entry.AllocatedAt); err != nil {
			return byohPoolEntry{}, fmt.Errorf("pool entry has invalid allocation timestamp")
		}
	default:
		return byohPoolEntry{}, fmt.Errorf("pool entry has unsupported status")
	}
	if _, err := time.Parse(time.RFC3339, entry.LastUpdated); err != nil {
		return byohPoolEntry{}, fmt.Errorf("pool entry has invalid update timestamp")
	}
	if !entry.Disposable && entry.ResetPolicy != byohResetPolicy {
		return byohPoolEntry{}, fmt.Errorf("reusable pool entry has incompatible reset policy")
	}
	return entry, nil
}

func validateBYOHPoolInventory(data map[string]string) (map[string]byohPoolEntry, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("required BYOH node pool is empty")
	}
	entries := make(map[string]byohPoolEntry, len(data))
	for address, raw := range data {
		entry, err := parseBYOHPoolEntry(address, raw)
		if err != nil {
			return nil, fmt.Errorf("required BYOH node pool is malformed: %w", err)
		}
		entries[address] = entry
	}
	byHost := map[string]byohPoolEntry{}
	for _, entry := range entries {
		if existing, ok := byHost[entry.HostID]; ok {
			if existing.Username != entry.Username || existing.Platform != entry.Platform ||
				existing.SSHAddress != entry.SSHAddress || existing.Disposable != entry.Disposable ||
				existing.ResetPolicy != entry.ResetPolicy {
				return nil, fmt.Errorf("aliases for one physical host disagree on immutable metadata")
			}
			if existing.Status != entry.Status || existing.TestID != entry.TestID ||
				existing.AllocatedAt != entry.AllocatedAt {
				return nil, fmt.Errorf("aliases for one physical host disagree on lease ownership")
			}
		} else {
			byHost[entry.HostID] = entry
		}
	}
	return entries, nil
}

func serializeBYOHPoolEntry(entry byohPoolEntry) string {
	return fmt.Sprintf("status: %s\nusername: %s\naddress-type: %s\nplatform: %s\ntest-id: %s\n"+
		"allocated-at: %s\nlast-updated: %s\nhost-id: %s\nreset-policy: %s\ndisposable: %t\nssh-address: %s\n",
		entry.Status, entry.Username, entry.AddressType, entry.Platform, entry.TestID, entry.AllocatedAt,
		entry.LastUpdated, entry.HostID, entry.ResetPolicy, entry.Disposable, entry.SSHAddress)
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func newBYOHLeaseID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate lease identity: %w", err)
	}
	return fmt.Sprintf("winc-1974-%x", random), nil
}

func claimBYOHHosts(data map[string]string, requests []byohHostRequest, leaseID string,
	now time.Time) (map[string]string, []*byohHostLease, error) {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return nil, nil, err
	}
	addresses := make([]string, 0, len(entries))
	for address := range entries {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	selectedHosts := map[string]bool{}
	leases := make([]*byohHostLease, 0, len(requests))
	for _, request := range requests {
		var selected string
		for _, address := range addresses {
			entry := entries[address]
			if selectedHosts[entry.HostID] || entry.AddressType != request.AddressType ||
				entry.Disposable != request.Disposable || entry.Status != "available" {
				continue
			}
			aliasesAvailable := true
			for _, alias := range entries {
				if alias.HostID == entry.HostID && alias.Status != "available" {
					aliasesAvailable = false
					break
				}
			}
			if aliasesAvailable {
				selected = address
				break
			}
		}
		if selected == "" {
			return nil, nil, fmt.Errorf("required BYOH pool has insufficient compatible physical hosts")
		}
		entry := entries[selected]
		aliases := []string{}
		for address, alias := range entries {
			if alias.HostID == entry.HostID {
				aliases = append(aliases, address)
			}
		}
		sort.Strings(aliases)
		leases = append(leases, &byohHostLease{Address: selected, Username: entry.Username,
			SSHAddress: entry.SSHAddress, AddressType: entry.AddressType, HostID: entry.HostID,
			LeaseID: leaseID, Disposable: entry.Disposable, ClaimedAliases: aliases})
		selectedHosts[entry.HostID] = true
	}
	updated := copyStringMap(data)
	stamp := now.UTC().Format(time.RFC3339)
	for _, lease := range leases {
		for _, alias := range lease.ClaimedAliases {
			entry := entries[alias]
			entry.Status, entry.TestID, entry.AllocatedAt, entry.LastUpdated = "allocated", leaseID, stamp, stamp
			updated[alias] = serializeBYOHPoolEntry(entry)
		}
	}
	return updated, leases, nil
}

func updateBYOHPoolLeases(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	requests []byohHostRequest) ([]*byohHostLease, error) {
	leaseID, err := newBYOHLeaseID()
	if err != nil {
		return nil, err
	}
	var leases []*byohHostLease
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updated, selected, err := claimBYOHHosts(cm.Data, requests, leaseID, time.Now())
		if err != nil {
			return err
		}
		// Retain the selected aliases before attempting the write. A successful API mutation can
		// still be followed by a client-side timeout, and cleanup must know what to reconcile.
		leases = selected
		cm.Data = updated
		if _, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		claimErr := fmt.Errorf("claim BYOH physical hosts: %w", err)
		if len(leases) == 0 {
			return nil, claimErr
		}
		if reconcileErr := reconcileBYOHPoolClaim(ctx, configMaps, leases); reconcileErr != nil {
			for _, lease := range leases {
				lease.QuarantineRequired = true
			}
			quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
			return leases, errors.Join(claimErr,
				fmt.Errorf("reconcile ambiguous BYOH claim: %w", reconcileErr),
				wrapOptionalError("quarantine ambiguous BYOH claim", quarantineErr))
		}
		// The live object proves that the ambiguous request was applied. Return the ownership
		// state together with the original error so the fixture aborts and cleanup unregisters or
		// quarantines it rather than proceeding as though the write were unambiguous.
		return leases, claimErr
	}
	if err := reconcileBYOHPoolClaim(ctx, configMaps, leases); err != nil {
		verifyErr := fmt.Errorf("verify BYOH physical-host claim: %w", err)
		for _, lease := range leases {
			lease.QuarantineRequired = true
		}
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return leases, errors.Join(verifyErr, wrapOptionalError("quarantine failed BYOH claim", quarantineErr))
	}
	return leases, nil
}

func wrapOptionalError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func verifyBYOHPoolClaim(data map[string]string, leases []*byohHostLease) error {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		for _, alias := range lease.ClaimedAliases {
			entry, ok := entries[alias]
			if !ok || entry.HostID != lease.HostID || entry.Status != "allocated" || entry.TestID != lease.LeaseID {
				return fmt.Errorf("BYOH physical-host claim ownership mismatch")
			}
		}
	}
	return nil
}

func reconcileBYOHPoolClaim(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	var lastErr error
	err := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			lastErr = err
			return err
		}
		if err := verifyBYOHPoolClaim(cm.Data, leases); err != nil {
			lastErr = err
			return err
		}
		lastErr = nil
		return nil
	})
	if err != nil {
		return lastErr
	}
	return nil
}

func transitionBYOHHosts(data map[string]string, leases []*byohHostLease, target string,
	clearOwnership bool, now time.Time) (map[string]string, error) {
	entries, err := validateBYOHPoolInventory(data)
	if err != nil {
		return nil, err
	}
	updated := copyStringMap(data)
	stamp := now.UTC().Format(time.RFC3339)
	for _, lease := range leases {
		for _, alias := range lease.ClaimedAliases {
			entry, ok := entries[alias]
			if !ok || entry.HostID != lease.HostID || entry.TestID != lease.LeaseID ||
				(entry.Status != "allocated" && entry.Status != "releasing") {
				return nil, fmt.Errorf("BYOH host ownership changed before transition")
			}
			entry.Status, entry.LastUpdated = target, stamp
			if clearOwnership {
				entry.TestID, entry.AllocatedAt = "", ""
			}
			updated[alias] = serializeBYOHPoolEntry(entry)
		}
	}
	return updated, nil
}

func mutateBYOHHostStatus(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, target string, clearOwnership bool) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohPoolConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		updated, err := transitionBYOHHosts(cm.Data, leases, target, clearOwnership, time.Now())
		if err != nil {
			return err
		}
		cm.Data = updated
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
}

func releaseBYOHHosts(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	for _, lease := range leases {
		if !lease.ResetVerified || lease.Disposable {
			return fmt.Errorf("BYOH physical host cannot be returned without verified reusable reset")
		}
	}
	if err := mutateBYOHHostStatus(ctx, configMaps, leases, "releasing", false); err != nil {
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return errors.Join(fmt.Errorf("mark BYOH physical hosts releasing: %w", err),
			wrapOptionalError("quarantine after release failure", quarantineErr))
	}
	if err := mutateBYOHHostStatus(ctx, configMaps, leases, "available", true); err != nil {
		quarantineErr := quarantineBYOHHosts(ctx, configMaps, leases)
		return errors.Join(fmt.Errorf("release BYOH physical hosts: %w", err),
			wrapOptionalError("quarantine after release failure", quarantineErr))
	}
	for _, lease := range leases {
		lease.CleanupDone = true
	}
	return nil
}

func quarantineBYOHHosts(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease) error {
	if len(leases) == 0 {
		return nil
	}
	err := mutateBYOHHostStatus(ctx, configMaps, leases, "unavailable", true)
	if err == nil {
		for _, lease := range leases {
			lease.CleanupDone = true
		}
	}
	return err
}

func (f *byohTestFixture) allocateBYOHHosts(requests ...byohHostRequest) ([]*byohHostLease, error) {
	leases, err := updateBYOHPoolLeases(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), requests)
	f.leases = append(f.leases, leases...)
	return leases, err
}

func registrationOwnershipAnnotation(address string) string {
	digest := sha256.Sum256([]byte(address))
	return byohRegistrationOwner + fmt.Sprintf("%x", digest[:8])
}

func registrationEntriesOwned(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) bool {
	if cm == nil {
		return false
	}
	if state != nil {
		expectedUID := state.ownedUID
		if expectedUID == "" && state.initiallyExisted {
			expectedUID = state.initialUID
		}
		if expectedUID != "" && cm.UID != expectedUID {
			return false
		}
	}
	for _, lease := range leases {
		if cm.Data[lease.Address] != "username="+lease.Username ||
			cm.Annotations[registrationOwnershipAnnotation(lease.Address)] != lease.LeaseID {
			return false
		}
	}
	return true
}

func registrationObjectMatchesRetainedUID(cm *corev1.ConfigMap, state *byohInstancesState) bool {
	if cm == nil || state == nil {
		return false
	}
	expectedUID := state.ownedUID
	if expectedUID == "" && state.initiallyExisted {
		expectedUID = state.initialUID
	}
	return expectedUID != "" && cm.UID == expectedUID
}

func markRegistrationOwned(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	if cm == nil || cm.UID == "" || !registrationEntriesOwned(cm, leases, state) {
		return fmt.Errorf("windows-instances ownership evidence or object identity does not match")
	}
	for _, lease := range leases {
		if lease.LeaseID == "" {
			return fmt.Errorf("cannot register a BYOH host without a lease identity")
		}
		lease.RegistrationOwned, lease.Registered = true, true
	}
	if state != nil {
		state.ownedUID = cm.UID
	}
	return nil
}

func addRegistrationOwnership(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	if state.originalOwnership == nil {
		state.originalOwnership = map[string]*string{}
	}
	for _, lease := range leases {
		key := registrationOwnershipAnnotation(lease.Address)
		if value, exists := cm.Annotations[key]; exists {
			if value == lease.LeaseID && cm.Data[lease.Address] == "username="+lease.Username {
				continue
			}
			return fmt.Errorf("%w: registration ownership metadata already exists", errBYOHRegistrationConflict)
		}
		if _, captured := state.originalOwnership[key]; !captured {
			state.originalOwnership[key] = nil
		}
		cm.Annotations[key] = lease.LeaseID
	}
	return nil
}

func restoreRegistrationOwnership(cm *corev1.ConfigMap, leases []*byohHostLease,
	state *byohInstancesState) error {
	for _, lease := range leases {
		key := registrationOwnershipAnnotation(lease.Address)
		if cm.Annotations[key] != lease.LeaseID {
			return fmt.Errorf("owned windows-instances registration metadata is missing or changed")
		}
		original, captured := state.originalOwnership[key]
		if !captured || original == nil {
			delete(cm.Annotations, key)
		} else {
			cm.Annotations[key] = *original
		}
	}
	if len(cm.Annotations) == 0 {
		cm.Annotations = nil
	}
	return nil
}

func reconcileBYOHRegistration(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState) error {
	var cm *corev1.ConfigMap
	err := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		current, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cm = current
		if !registrationEntriesOwned(cm, leases, state) {
			return fmt.Errorf("windows-instances does not contain the exact attempted registration and ownership evidence")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return markRegistrationOwned(cm, leases, state)
}

func inspectBYOHRegistrationConflict(ctx context.Context, configMaps corev1client.ConfigMapInterface) error {
	return retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		_, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		return err
	})
}

func registerBYOHInstances(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState) error {
	writeAttempted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if state != nil && !state.observed {
			state.observed = true
			if err == nil {
				state.initiallyExisted, state.initialUID = true, cm.UID
			} else if !apierrors.IsNotFound(err) {
				return err
			}
		}
		if apierrors.IsNotFound(err) {
			data := map[string]string{}
			for _, lease := range leases {
				data[lease.Address] = "username=" + lease.Username
			}
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
				Namespace: wmcoNamespace}, Data: data}
			if err := addRegistrationOwnership(cm, leases, state); err != nil {
				return err
			}
			writeAttempted = true
			created, err := configMaps.Create(ctx, cm, metav1.CreateOptions{})
			if err == nil {
				return markRegistrationOwned(created, leases, state)
			}
			return err
		}
		if err != nil {
			return err
		}
		if state != nil && state.initiallyExisted && !registrationObjectMatchesRetainedUID(cm, state) {
			return fmt.Errorf("%w: windows-instances object was replaced during registration",
				errBYOHRegistrationConflict)
		}
		if state != nil && !state.initiallyExisted && writeAttempted {
			if registrationEntriesOwned(cm, leases, state) {
				return markRegistrationOwned(cm, leases, state)
			}
			return fmt.Errorf("%w: windows-instances was concurrently created without exact ownership evidence",
				errBYOHRegistrationConflict)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if writeAttempted && registrationEntriesOwned(cm, leases, state) {
			return markRegistrationOwned(cm, leases, state)
		}
		for _, lease := range leases {
			if _, exists := cm.Data[lease.Address]; exists {
				return errBYOHRegistrationConflict
			}
			cm.Data[lease.Address] = "username=" + lease.Username
		}
		if err := addRegistrationOwnership(cm, leases, state); err != nil {
			return err
		}
		writeAttempted = true
		updated, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return markRegistrationOwned(updated, leases, state)
		}
		return err
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, errBYOHRegistrationConflict) {
		return errors.Join(fmt.Errorf("write windows-instances: %w", err),
			wrapOptionalError("inspect conflicting windows-instances state",
				inspectBYOHRegistrationConflict(ctx, configMaps)))
	}
	if writeAttempted {
		reconcileErr := reconcileBYOHRegistration(ctx, configMaps, leases, state)
		if reconcileErr != nil {
			return errors.Join(fmt.Errorf("write windows-instances: %w", err),
				fmt.Errorf("reconcile ambiguous windows-instances write: %w", reconcileErr))
		}
	}
	return fmt.Errorf("write windows-instances: %w", err)
}

func (f *byohTestFixture) registerBYOHInstances(leases []*byohHostLease) error {
	started := time.Now().UTC()
	for _, lease := range leases {
		lease.RegistrationAttempted = true
	}
	if err := registerBYOHInstances(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), leases,
		&f.instancesState); err != nil {
		for _, lease := range leases {
			if !lease.RegistrationOwned {
				lease.QuarantineRequired = true
			}
		}
		return err
	}
	for _, lease := range leases {
		lease.ReconcileStarted = started
	}
	return nil
}

func (f *byohTestFixture) registerBYOHInstancesPreservingConfigMap(leases []*byohHostLease) error {
	for _, lease := range leases {
		lease.PreserveInstancesConfigMap = true
	}
	return f.registerBYOHInstances(leases)
}

func unregisterBYOHInstances(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState, preserveConfigMap bool,
	beforeMutation, mutationProvenUnapplied func()) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if state == nil || state.ownedUID == "" || cm.UID != state.ownedUID {
			return fmt.Errorf("windows-instances object identity changed before unregister")
		}
		for _, lease := range leases {
			if !lease.RegistrationOwned {
				return fmt.Errorf("refusing to remove an unowned windows-instances entry")
			}
			expected := "username=" + lease.Username
			if value, ok := cm.Data[lease.Address]; !ok || value != expected {
				return fmt.Errorf("owned windows-instances entry is missing or changed")
			}
			delete(cm.Data, lease.Address)
		}
		if err := restoreRegistrationOwnership(cm, leases, state); err != nil {
			return err
		}
		if len(cm.Data) == 0 && !state.initiallyExisted && !preserveConfigMap {
			if beforeMutation != nil {
				beforeMutation()
			}
			return configMaps.Delete(ctx, cm.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
				UID: &state.ownedUID, ResourceVersion: &cm.ResourceVersion}})
		}
		if beforeMutation != nil {
			beforeMutation()
		}
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		return nil
	}
	// A delete/update can be applied even when the response is lost. Only absence of every owned
	// entry is safe evidence that unregister completed; unrelated concurrent data is preserved.
	cm, getErr := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) && state != nil && state.ownedUID != "" && !state.initiallyExisted &&
		!preserveConfigMap {
		return nil
	}
	if getErr == nil {
		if state == nil || cm.UID != state.ownedUID {
			return errors.Join(err, fmt.Errorf("windows-instances object identity changed during unregister"))
		}
		removed := true
		for _, lease := range leases {
			if _, present := cm.Data[lease.Address]; present ||
				cm.Annotations[registrationOwnershipAnnotation(lease.Address)] == lease.LeaseID {
				removed = false
			}
		}
		if removed {
			return nil
		}
		if registrationEntriesOwned(cm, leases, state) && mutationProvenUnapplied != nil {
			mutationProvenUnapplied()
		}
	}
	return errors.Join(err, wrapOptionalError("reconcile ambiguous windows-instances removal", getErr))
}

type byohLookupIP func(string) ([]net.IP, error)

func nodeMatchesLease(node *corev1.Node, lease *byohHostLease, lookup byohLookupIP) (bool, error) {
	if node == nil || lease == nil {
		return false, nil
	}
	wanted := map[string]bool{}
	if lease.AddressType == byohAddressIP {
		wanted[lease.Address] = true
	} else {
		resolved, err := lookup(lease.Address)
		if err != nil {
			return false, fmt.Errorf("resolve required DNS registration address: %w", err)
		}
		for _, address := range resolved {
			if ip := address.To4(); ip != nil {
				wanted[ip.String()] = true
			}
		}
	}
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && wanted[address.Address] {
			return true, nil
		}
	}
	return false, nil
}

func exactLeaseNode(items []corev1.Node, lease *byohHostLease, lookup byohLookupIP,
	rejectedUID types.UID) (*corev1.Node, bool, error) {
	matches := make([]*corev1.Node, 0, 1)
	for i := range items {
		matchesLease, err := nodeMatchesLease(&items[i], lease, lookup)
		if err != nil {
			return nil, false, err
		}
		if matchesLease {
			matches = append(matches, &items[i])
		}
	}
	if len(matches) > 1 {
		return nil, false, fmt.Errorf("registration address matches multiple Windows Nodes")
	}
	if len(matches) == 0 || !nodeReadyAndConverged(matches[0]) || matches[0].UID == rejectedUID {
		return nil, false, nil
	}
	return matches[0], true, nil
}

func nodeReadyAndConverged(node *corev1.Node) bool {
	if node == nil || node.Spec.Unschedulable || node.Labels["windowsmachineconfig.openshift.io/byoh"] != "true" {
		return false
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
		}
	}
	version, desired := node.Annotations[byohVersionAnno], node.Annotations[byohDesiredVersionAnno]
	return ready && version != "" && version == desired
}

func waitForLeaseNodeWithOptions(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease,
	lookup byohLookupIP, rejectedUID types.UID, interval, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true,
		func(ctx context.Context) (bool, error) {
			list, err := nodes.List(ctx, metav1.ListOptions{LabelSelector: windowsNodeLabel})
			if err != nil {
				return false, err
			}
			node, ready, err := exactLeaseNode(list.Items, lease, lookup, rejectedUID)
			if err != nil || !ready {
				return false, err
			}
			lease.NodeName, lease.NodeUID, lease.NodeResourceVersion = node.Name, node.UID, node.ResourceVersion
			return true, nil
		})
}

func waitForLeaseNode(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease,
	rejectedUID types.UID) error {
	return waitForLeaseNodeWithOptions(ctx, nodes, lease, net.LookupIP, rejectedUID, 10*time.Second, byohPoolPollTimeout)
}

func distinctLeaseNodes(leases []*byohHostLease) error {
	names, uids := map[string]bool{}, map[types.UID]bool{}
	for _, lease := range leases {
		if lease.NodeName == "" || lease.NodeUID == "" {
			return fmt.Errorf("selected physical host has no retained Node identity")
		}
		if names[lease.NodeName] || uids[lease.NodeUID] {
			return fmt.Errorf("distinct physical-host leases resolved to the same Node identity")
		}
		names[lease.NodeName], uids[lease.NodeUID] = true, true
	}
	return nil
}

func (f *byohTestFixture) waitForLeaseNodeReady(leases ...*byohHostLease) error {
	for _, lease := range leases {
		if err := waitForLeaseNode(f.ctx, f.coreClient.Nodes(), lease, ""); err != nil {
			lease.QuarantineRequired = true
			return fmt.Errorf("selected BYOH host did not become Ready and converged: %w", err)
		}
	}
	if err := distinctLeaseNodes(leases); err != nil {
		for _, lease := range leases {
			lease.QuarantineRequired = true
		}
		return err
	}
	return nil
}

func waitForNodeUIDGone(ctx context.Context, nodes corev1client.NodeInterface, nodeName string,
	uid types.UID, interval, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		node, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return node.UID != uid, nil
	})
}

func waitForNodeDeleted(ctx context.Context, nodes corev1client.NodeInterface, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("cannot wait for deletion of an unidentified Node")
	}
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, byohDeconfigurationLimit, true,
		func(ctx context.Context) (bool, error) {
			_, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
}

func wmcoLogMatchesLeaseSince(logs string, lease *byohHostLease, message string, since time.Time) bool {
	normalizedMessage := strings.ToLower(strings.ReplaceAll(message, " ", ""))
	for _, line := range strings.Split(logs, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		loggedAt, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil || loggedAt.Before(since) {
			continue
		}
		normalized := strings.ToLower(strings.ReplaceAll(line, " ", ""))
		if strings.Contains(normalized, normalizedMessage) &&
			((lease.NodeName != "" && containsExactLogIdentity(line, lease.NodeName)) ||
				containsExactLogIdentity(line, lease.Address)) {
			return true
		}
	}
	return false
}

func containsExactLogIdentity(line, identity string) bool {
	if identity == "" {
		return false
	}
	for offset := 0; offset <= len(line)-len(identity); {
		index := strings.Index(line[offset:], identity)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !isLogIdentityByte(line[index-1])
		after := index + len(identity)
		afterOK := after == len(line) || !isLogIdentityByte(line[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + 1
	}
	return false
}

func isLogIdentityByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '.' || value == '_' || value == ':' || value == '-'
}

func readWMCOLogsSince(ctx context.Context, coreClient corev1client.CoreV1Interface, since time.Time) (string, error) {
	pods, err := coreClient.Pods(wmcoNamespace).List(ctx, metav1.ListOptions{LabelSelector: byohOperatorPodSelector})
	if err != nil {
		return "", fmt.Errorf("list WMCO operator Pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no WMCO operator Pod matched the fixed selector")
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	var logs strings.Builder
	for i := range pods.Items {
		options := &corev1.PodLogOptions{Container: byohOperatorContainer, Timestamps: true,
			SinceTime: &metav1.Time{Time: since}}
		output, err := coreClient.Pods(wmcoNamespace).GetLogs(pods.Items[i].Name, options).DoRaw(ctx)
		if err != nil {
			return "", fmt.Errorf("read WMCO operator Pod logs: %w", err)
		}
		logs.Write(output)
		logs.WriteByte('\n')
	}
	return logs.String(), nil
}

func waitForWMCOLogForLeaseSince(ctx context.Context, coreClient corev1client.CoreV1Interface,
	lease *byohHostLease, message string, since time.Time) error {
	return wait.PollUntilContextTimeout(ctx, 15*time.Second, byohDeconfigurationLimit, true,
		func(ctx context.Context) (bool, error) {
			logs, err := readWMCOLogsSince(ctx, coreClient, since)
			if err != nil {
				return false, err
			}
			return wmcoLogMatchesLeaseSince(logs, lease, message, since), nil
		})
}

func (f *byohTestFixture) waitForWMCOLogForLeaseSince(ctx context.Context, lease *byohHostLease,
	message string, since time.Time) error {
	if f.waitForLeaseLog != nil {
		return f.waitForLeaseLog(ctx, lease, message, since)
	}
	return waitForWMCOLogForLeaseSince(ctx, f.coreClient, lease, message, since)
}

type sshCommandSession interface {
	CombinedOutput(string) ([]byte, error)
	Close() error
}

type windowsSSHClient struct {
	client     *ssh.Client
	newSession func() (sshCommandSession, error)
	closeFn    func() error
}

func sshErrorClass(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "authenticate") {
		return "authentication"
	}
	if strings.Contains(lower, "host key") {
		return "host-key"
	}
	return "transport"
}

func newWindowsSSHClient(ctx context.Context, lease *byohHostLease, privateKey []byte) (*windowsSSHClient, error) {
	if lease == nil || lease.SSHAddress == "" || lease.Username == "" {
		return nil, fmt.Errorf("incomplete BYOH SSH fixture")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}
	config := &ssh.ClientConfig{
		User: lease.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// The source fixture cannot verify host keys until the companion pool publishes trust
		// material. Do not claim verified trust or invent keys here.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // #nosec G106 -- explicit companion contract gap.
		Timeout:         30 * time.Second,
	}
	connection, err := (&net.Dialer{Timeout: config.Timeout}).DialContext(ctx, "tcp",
		net.JoinHostPort(lease.SSHAddress, "22"))
	if err != nil {
		return nil, fmt.Errorf("connect to selected BYOH host over SSH (%s)", sshErrorClass(ctx, err))
	}
	return newWindowsSSHClientFromConnection(ctx, connection, net.JoinHostPort(lease.SSHAddress, "22"), config)
}

func newWindowsSSHClientFromConnection(ctx context.Context, connection net.Conn, address string,
	config *ssh.ClientConfig) (*windowsSSHClient, error) {
	closeOnCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	var handshakeDeadline time.Time
	if config != nil && config.Timeout > 0 {
		handshakeDeadline = time.Now().Add(config.Timeout)
	}
	if deadline, ok := ctx.Deadline(); ok && (handshakeDeadline.IsZero() || deadline.Before(handshakeDeadline)) {
		handshakeDeadline = deadline
	}
	if !handshakeDeadline.IsZero() {
		_ = connection.SetDeadline(handshakeDeadline)
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection,
		address, config)
	closeOnCancel()
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("establish selected BYOH SSH session (%s)", sshErrorClass(ctx, err))
	}
	if ctx.Err() != nil {
		_ = clientConnection.Close()
		return nil, fmt.Errorf("establish selected BYOH SSH session (%s)", sshErrorClass(ctx, ctx.Err()))
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConnection, channels, requests)
	return &windowsSSHClient{client: client,
		newSession: func() (sshCommandSession, error) { return client.NewSession() }, closeFn: client.Close}, nil
}

func (c *windowsSSHClient) close() error {
	if c == nil {
		return nil
	}
	if c.closeFn != nil {
		return c.closeFn()
	}
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, len(encoded)*2)
	for i, value := range encoded {
		bytes[i*2], bytes[i*2+1] = byte(value), byte(value>>8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

func runPowerShellOverSSH(ctx context.Context, client *windowsSSHClient, script string) (string, error) {
	if client == nil || client.newSession == nil {
		return "", fmt.Errorf("SSH client is not initialized")
	}
	type sessionResult struct {
		session sshCommandSession
		err     error
	}
	sessionCh := make(chan sessionResult, 1)
	go func() {
		session, err := client.newSession()
		sessionCh <- sessionResult{session: session, err: err}
	}()
	var session sshCommandSession
	select {
	case <-ctx.Done():
		_ = client.close()
		result := <-sessionCh
		if result.session != nil {
			_ = result.session.Close()
		}
		return "", fmt.Errorf("create selected BYOH SSH session (%s): %w", sshErrorClass(ctx, ctx.Err()), ctx.Err())
	case result := <-sessionCh:
		if result.err != nil {
			return "", fmt.Errorf("create selected BYOH SSH session (%s)", sshErrorClass(ctx, result.err))
		}
		session = result.session
	}
	defer session.Close()
	type result struct {
		output []byte
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		output, err := session.CombinedOutput("powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " +
			encodePowerShell(script))
		resultCh <- result{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		// Closing the transport first guarantees CombinedOutput is released. Join the worker before
		// closing the session or returning so cancellation cannot leak a command goroutine.
		_ = client.close()
		<-resultCh
		_ = session.Close()
		return "", fmt.Errorf("PowerShell command canceled (%s): %w", sshErrorClass(ctx, ctx.Err()), ctx.Err())
	case result := <-resultCh:
		if result.err != nil {
			return "", fmt.Errorf("PowerShell command failed (%s)", sshErrorClass(ctx, result.err))
		}
		return strings.TrimSpace(string(result.output)), nil
	}
}

func waitForSSHReady(ctx context.Context, lease *byohHostLease, privateKey []byte) error {
	return wait.PollUntilContextTimeout(ctx, 15*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			client, err := newWindowsSSHClient(ctx, lease, privateKey)
			if err != nil {
				return false, nil
			}
			defer client.close()
			_, err = runPowerShellOverSSH(ctx, client, "Write-Output 'ready'")
			return err == nil, nil
		})
}

var byohManagedServices = []string{
	"windows_exporter", "kube-proxy", "hybrid-overlay-node", "kubelet",
	"windows-instance-config-daemon", "containerd",
}

var byohManagedDirectories = []string{
	`C:\Temp`, `C:\k\cni`, `C:\k\cni\config`, `C:\var\log`, `C:\var\log\kubelet`,
	`C:\var\log\csi-proxy`, `C:\var\log\kube-proxy`, `C:\var\log\wicd`,
	`C:\var\log\hybrid-overlay`, `C:\k\containerd`, `C:\var\log\containerd`,
	`C:\var\log\windows-exporter`, `C:\k\containerd\registries`, `C:\k\etc\kubernetes\manifests`,
	`C:\k`, `C:\k\tls`, `C:\k\wicd-certs`,
}

func managedServicesRunningScript() string {
	return "$names=@('" + strings.Join(byohManagedServices, "','") +
		"'); foreach($name in $names){$svc=Get-Service -Name $name -ErrorAction Stop; if($svc.Status -ne 'Running'){exit 1}}"
}

func managedServicesStoppedScript() string {
	return "$names=@('" + strings.Join(byohManagedServices, "','") +
		"'); foreach($name in $names){$svc=Get-Service -Name $name -ErrorAction SilentlyContinue; if($svc -and $svc.Status -ne 'Stopped'){exit 1}}"
}

func managedDirectoriesRemovedScript() string {
	return "$paths=@('" + strings.Join(byohManagedDirectories, "','") +
		"'); foreach($path in $paths){if(Test-Path -LiteralPath $path){exit 1}}"
}

func assertManagedServicesRunning(ctx context.Context, client *windowsSSHClient) error {
	_, err := runPowerShellOverSSH(ctx, client, managedServicesRunningScript())
	return err
}

func assertManagedServicesStopped(ctx context.Context, client *windowsSSHClient) error {
	_, err := runPowerShellOverSSH(ctx, client, managedServicesStoppedScript())
	return err
}

func assertManagedDirectoriesRemoved(ctx context.Context, client *windowsSSHClient) error {
	_, err := runPowerShellOverSSH(ctx, client, managedDirectoriesRemovedScript())
	return err
}

func assertPathState(ctx context.Context, client *windowsSSHClient, path string, present bool) error {
	expected := "$false"
	if present {
		expected = "$true"
	}
	_, err := runPowerShellOverSSH(ctx, client, fmt.Sprintf("if((Test-Path -LiteralPath '%s') -ne %s){exit 1}",
		strings.ReplaceAll(path, "'", "''"), expected))
	return err
}

func (f *byohTestFixture) cloudPrivateKey(ctx context.Context) ([]byte, error) {
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("required cloud private key Secret is unavailable: %w", err)
	}
	key := secret.Data[byohPrivateKeyDataKey]
	if len(key) == 0 {
		return nil, fmt.Errorf("required cloud private key Secret has no private key")
	}
	return append([]byte(nil), key...), nil
}

func (f *byohTestFixture) sshClient(ctx context.Context, lease *byohHostLease) (*windowsSSHClient, error) {
	key, err := f.cloudPrivateKey(ctx)
	if err != nil {
		return nil, err
	}
	return newWindowsSSHClient(ctx, lease, key)
}

func (f *byohTestFixture) assertLeasePath(lease *byohHostLease, path string, present bool) error {
	client, err := f.sshClient(f.ctx, lease)
	if err != nil {
		return err
	}
	defer client.close()
	return assertPathState(f.ctx, client, path, present)
}

func generateBYOHWindowsWebServerYAML(name string, replicas int, nodeNames []string) string {
	quoted := make([]string, len(nodeNames))
	for i, nodeName := range nodeNames {
		quoted[i] = fmt.Sprintf("            - %q", nodeName)
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  labels:
    app: %s
spec:
  replicas: %d
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchFields:
              - key: metadata.name
                operator: In
                values:
%s
      tolerations:
      - key: os
        operator: Equal
        value: Windows
        effect: NoSchedule
      containers:
      - name: windowswebserver
        image: %s
        imagePullPolicy: IfNotPresent
        securityContext:
          runAsNonRoot: false
          windowsOptions:
            runAsUserName: ContainerAdministrator
        command:
        - pwsh.exe
        - -command
        - "$listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start(); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $response.Close() }"
      nodeSelector:
        kubernetes.io/os: windows
`, name, name, replicas, name, name, strings.Join(quoted, "\n"), windowsDebugImage)
}

func (f *byohTestFixture) createBYOHWebServer(namespace string, leases []*byohHostLease) error {
	if _, err := f.coreClient.Namespaces().Create(f.ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"type": "byoh-node"}},
	}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create isolated BYOH workload namespace: %w", err)
	}
	f.workloadNS = namespace
	if err := compat_otp.SetNamespacePrivileged(f.oc, namespace); err != nil {
		return fmt.Errorf("allow privileged Windows workload namespace: %w", err)
	}
	nodes := make([]string, 0, len(leases))
	for _, lease := range leases {
		if lease.NodeName == "" {
			return fmt.Errorf("cannot constrain workload to an unidentified BYOH Node")
		}
		nodes = append(nodes, lease.NodeName)
	}
	if err := createResourceFromString(f.oc, namespace,
		generateBYOHWindowsWebServerYAML(byohWorkloadDeployment, 5, nodes)); err != nil {
		return err
	}
	return f.waitForBYOHWebServer(leases)
}

func (f *byohTestFixture) waitForBYOHWebServer(leases []*byohHostLease) error {
	if err := waitForDeploymentReady(f.oc, byohWorkloadDeployment, f.workloadNS, 15*time.Minute); err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(f.ctx, 10*time.Second, 15*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			deployment, err := f.appsClient.Deployments(f.workloadNS).Get(ctx, byohWorkloadDeployment,
				metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			replicaSets, err := f.appsClient.ReplicaSets(f.workloadNS).List(ctx,
				metav1.ListOptions{LabelSelector: "app=" + byohWorkloadDeployment})
			if err != nil {
				return false, err
			}
			currentReplicaSetUID, found := currentDeploymentReplicaSetUID(deployment, replicaSets.Items)
			if !found {
				return false, nil
			}
			pods, err := f.coreClient.Pods(f.workloadNS).List(ctx,
				metav1.ListOptions{LabelSelector: "app=" + byohWorkloadDeployment})
			if err != nil {
				return false, err
			}
			return currentBYOHWorkloadReady(ctx, f.coreClient.Nodes(), pods.Items, currentReplicaSetUID, leases, 5)
		})
}

func currentDeploymentReplicaSetUID(deployment *appsv1.Deployment, replicaSets []appsv1.ReplicaSet) (types.UID, bool) {
	if deployment == nil {
		return "", false
	}
	revision := deployment.Annotations["deployment.kubernetes.io/revision"]
	if revision == "" {
		return "", false
	}
	wantedTemplate := deployment.Spec.Template.DeepCopy()
	if wantedTemplate.Labels != nil {
		delete(wantedTemplate.Labels, "pod-template-hash")
	}
	var current types.UID
	for i := range replicaSets {
		replicaSet := &replicaSets[i]
		if !controlledByUID(replicaSet.OwnerReferences, deployment.UID) ||
			replicaSet.Annotations["deployment.kubernetes.io/revision"] != revision {
			continue
		}
		template := replicaSet.Spec.Template.DeepCopy()
		if template.Labels != nil {
			delete(template.Labels, "pod-template-hash")
		}
		if !reflect.DeepEqual(wantedTemplate, template) || current != "" {
			return "", false
		}
		current = replicaSet.UID
	}
	return current, current != ""
}

func controlledByUID(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid && ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

func podIsReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func currentBYOHWorkloadReady(ctx context.Context, nodes corev1client.NodeInterface, pods []corev1.Pod,
	replicaSetUID types.UID, leases []*byohHostLease, replicas int) (bool, error) {
	allowed := map[string]types.UID{}
	for _, lease := range leases {
		allowed[lease.NodeName] = lease.NodeUID
	}
	current := make([]*corev1.Pod, 0, replicas)
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || !podIsReady(pod) {
			continue
		}
		if controlledByUID(pod.OwnerReferences, replicaSetUID) {
			current = append(current, pod)
		}
	}
	if len(current) != replicas {
		return false, nil
	}
	for _, pod := range current {
		expectedUID, ok := allowed[pod.Spec.NodeName]
		if !ok {
			return false, fmt.Errorf("BYOH workload escaped the selected physical hosts")
		}
		node, err := nodes.Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if node.UID != expectedUID {
			return false, fmt.Errorf("BYOH workload was placed on a replaced Node identity")
		}
	}
	return true, nil
}

func (f *byohTestFixture) deleteWorkload(ctx context.Context) error {
	if f.workloadNS == "" {
		return nil
	}
	timeout := 10 * time.Minute
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx.Err()
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	if err := f.oc.AsAdmin().WithoutNamespace().Run("delete").Args("namespace", f.workloadNS,
		"--ignore-not-found=true", "--wait=true", "--request-timeout="+timeout.String(),
		"--timeout="+timeout.String()).Execute(); err != nil {
		return err
	}
	f.workloadNS = ""
	return nil
}

func (f *byohTestFixture) invalidateVersionAndWait(lease *byohHostLease) error {
	if _, err := f.oc.AsAdmin().WithoutNamespace().Run("annotate").Args("node", lease.NodeName,
		byohVersionAnno+"=invalidVersion", "--overwrite", "--request-timeout=30s").Output(); err != nil {
		return err
	}
	waitWMCOReconfigurationComplete(f.oc, lease.NodeName, 20*time.Minute)
	return nil
}

func (f *byohTestFixture) deleteNodeAndWaitForRecovery(lease *byohHostLease) error {
	if lease.NodeName == "" || lease.NodeUID == "" {
		return fmt.Errorf("cannot prove replacement of an unidentified Node")
	}
	originalName, originalUID := lease.NodeName, lease.NodeUID
	started := time.Now().UTC()
	if err := deleteNodeByRetainedIdentity(f.ctx, f.coreClient.Nodes(), originalName, originalUID); err != nil {
		lease.QuarantineRequired = true
		return err
	}
	if err := waitForNodeUIDGone(f.ctx, f.coreClient.Nodes(), originalName, originalUID,
		10*time.Second, byohPoolPollTimeout); err != nil {
		lease.QuarantineRequired = true
		return fmt.Errorf("original BYOH Node UID did not disappear: %w", err)
	}
	if err := f.waitForWMCOLogForLeaseSince(f.ctx, lease, "transferring files", started); err != nil {
		lease.QuarantineRequired = true
		return err
	}
	lease.NodeName, lease.NodeUID, lease.NodeResourceVersion = "", "", ""
	if err := waitForLeaseNode(f.ctx, f.coreClient.Nodes(), lease, originalUID); err != nil {
		lease.QuarantineRequired = true
		return fmt.Errorf("replacement BYOH Node did not become Ready and converged: %w", err)
	}
	return nil
}

func deleteNodeByRetainedIdentity(ctx context.Context, nodes corev1client.NodeInterface, name string,
	uid types.UID) error {
	if name == "" || uid == "" {
		return fmt.Errorf("cannot delete a Node without retained identity")
	}
	current, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("refresh retained Node identity before deletion: %w", err)
	}
	if current.UID != uid {
		return fmt.Errorf("refusing to delete replacement Node with changed UID")
	}
	if current.ResourceVersion == "" || !nodeReadyAndConverged(current) {
		return fmt.Errorf("refusing to delete retained Node before it is Ready and version-converged")
	}
	return nodes.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &current.ResourceVersion}})
}

func (f *byohTestFixture) createSyntheticIDMS(lease *byohHostLease) error {
	suffix := strings.TrimPrefix(lease.LeaseID, "winc-1974-")
	name := "winc-82694-" + suffix
	f.idmsName, f.idmsLeaseID = name, lease.LeaseID
	ownership, err := createOwnedIDMS(f.ctx, f.dynamicClient.Resource(byohIDMSGVR), name, lease.LeaseID)
	f.idmsAttempted, f.idmsOwned, f.idmsUID = ownership.attempted, ownership.owned, ownership.uid
	return err
}

func idmsOwnedByLease(object *unstructured.Unstructured, leaseID string) bool {
	return object != nil && object.GetAnnotations()["windowsmachineconfig.openshift.io/test-id"] == leaseID
}

type byohIDMSOwnership struct {
	attempted bool
	owned     bool
	uid       types.UID
}

func createOwnedIDMS(ctx context.Context, resource dynamic.ResourceInterface, name, leaseID string) (byohIDMSOwnership, error) {
	if existing, err := resource.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return byohIDMSOwnership{}, fmt.Errorf("refusing to replace pre-existing ImageDigestMirrorSet %q (uid %s)",
			name, existing.GetUID())
	} else if !apierrors.IsNotFound(err) {
		return byohIDMSOwnership{}, fmt.Errorf("check synthetic ImageDigestMirrorSet ownership: %w", err)
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "ImageDigestMirrorSet",
		"metadata": map[string]interface{}{
			"name": name,
			"annotations": map[string]interface{}{
				"windowsmachineconfig.openshift.io/test-id": leaseID,
			},
		},
		"spec": map[string]interface{}{
			"imageDigestMirrors": []interface{}{map[string]interface{}{
				"source":  "registry.redhat.io/openshift4",
				"mirrors": []interface{}{"example.com/my-mirror/openshift"},
			}},
		},
	}}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err == nil {
		if created.GetUID() == "" {
			return byohIDMSOwnership{attempted: true}, fmt.Errorf("created synthetic ImageDigestMirrorSet has no UID")
		}
		return byohIDMSOwnership{attempted: true, owned: true, uid: created.GetUID()}, nil
	}
	ownership := byohIDMSOwnership{attempted: true}
	var current *unstructured.Unstructured
	getErr := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		candidate, candidateErr := resource.Get(ctx, name, metav1.GetOptions{})
		if candidateErr != nil {
			return candidateErr
		}
		if !idmsOwnedByLease(candidate, leaseID) || candidate.GetUID() == "" {
			return fmt.Errorf("synthetic ImageDigestMirrorSet does not contain exact lease ownership evidence")
		}
		current = candidate
		return nil
	})
	if getErr == nil {
		ownership.owned, ownership.uid = true, current.GetUID()
		return ownership, fmt.Errorf("create synthetic ImageDigestMirrorSet returned an ambiguous error: %w", err)
	}
	return ownership, errors.Join(fmt.Errorf("create synthetic ImageDigestMirrorSet: %w", err),
		fmt.Errorf("reconcile ambiguous ImageDigestMirrorSet creation: %w", getErr))
}

func (f *byohTestFixture) waitForMirrorPrerequisite(lease *byohHostLease) error {
	// This proves only that WMCO reconciled the generated hosts.toml. The placeholder mirror is
	// deliberately never contacted, so this is not a mirrored image-pull test.
	client, err := f.sshClient(f.ctx, lease)
	if err != nil {
		return err
	}
	defer client.close()
	return wait.PollUntilContextTimeout(f.ctx, 15*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			output, err := runPowerShellOverSSH(ctx, client,
				`$p='C:\k\containerd\registries\registry.redhat.io\hosts.toml'; if(Test-Path $p){Get-Content -Raw $p}`)
			if err != nil {
				return false, err
			}
			return strings.Contains(output, "example.com/my-mirror/openshift") &&
				strings.Contains(output, "registry.redhat.io"), nil
		})
}

func (f *byohTestFixture) deleteSyntheticIDMSWithContext(ctx context.Context) error {
	if f.idmsName == "" {
		return nil
	}
	if !f.idmsAttempted && !f.idmsOwned {
		f.idmsName, f.idmsLeaseID = "", ""
		return nil
	}
	resource := f.dynamicClient.Resource(byohIDMSGVR)
	current, err := resource.Get(ctx, f.idmsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		f.idmsName, f.idmsLeaseID, f.idmsAttempted, f.idmsOwned, f.idmsUID = "", "", false, false, ""
		return nil
	}
	if err != nil {
		return err
	}
	if !idmsOwnedByLease(current, f.idmsLeaseID) {
		return fmt.Errorf("synthetic ImageDigestMirrorSet ownership changed")
	}
	if f.idmsUID != "" && current.GetUID() != f.idmsUID {
		return fmt.Errorf("synthetic ImageDigestMirrorSet object identity changed")
	}
	if current.GetUID() == "" {
		return fmt.Errorf("synthetic ImageDigestMirrorSet has no retained object identity")
	}
	if f.idmsUID == "" {
		f.idmsUID = current.GetUID()
	}
	if err := resource.Delete(ctx, f.idmsName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: ptrTo(f.idmsUID), ResourceVersion: ptrTo(current.GetResourceVersion())}}); err != nil &&
		!apierrors.IsNotFound(err) {
		return err
	}
	f.idmsName, f.idmsLeaseID, f.idmsAttempted, f.idmsOwned, f.idmsUID = "", "", false, false, ""
	return nil
}

func (f *byohTestFixture) deleteSyntheticIDMS() error {
	return f.deleteSyntheticIDMSWithContext(f.ctx)
}

func ptrTo[T any](value T) *T { return &value }

// These key helpers are adapted from the public, still-unmerged PR 4614. They intentionally
// contain no MachineSet discovery or scaling behavior.
func generateTestPrivateKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate RSA private key: %w", err)
	}
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	tmpFile, err := os.CreateTemp("", "winc-1974-key-*.pem")
	if err != nil {
		return "", fmt.Errorf("create temporary private-key file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			reportTemporaryKeyCleanup("close", tmpFile.Close())
			reportTemporaryKeyCleanup("removal", os.Remove(tmpFile.Name()))
		}
	}()
	if err := tmpFile.Chmod(0600); err != nil {
		return "", fmt.Errorf("restrict temporary private-key file permissions: %w", err)
	}
	if _, err := tmpFile.Write(privateKeyPEM); err != nil {
		return "", fmt.Errorf("write temporary private-key file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temporary private-key file: %w", err)
	}
	cleanup = false
	return tmpFile.Name(), nil
}

func reportTemporaryKeyCleanup(operation string, err error) {
	if err != nil {
		// Do not include the error text: filesystem errors commonly repeat the sensitive temporary path.
		e2e.Logf("temporary private-key %s failed", operation)
	}
}

func publicKeyHashFromPrivateKey(privateKeyPath string) (string, error) {
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}
	return publicKeyHash(privateKey)
}

func publicKeyHash(privateKey []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	authorizedKey := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(authorizedKey))), nil
}

func nodeReadyWithKeyHash(node *corev1.Node, expectedHash string) bool {
	if node == nil || expectedHash == "" || node.Annotations[byohPublicKeyHashAnno] != expectedHash {
		return false
	}
	return nodeReadyAndConverged(node)
}

func targetLeaseNodesReadyWithKeyHash(ctx context.Context, nodes corev1client.NodeInterface,
	leases []*byohHostLease, expectedHash string) (bool, error) {
	if len(leases) == 0 || expectedHash == "" {
		return false, nil
	}
	if err := distinctLeaseNodes(leases); err != nil {
		return false, nil
	}
	for _, lease := range leases {
		if lease.NodeName == "" || lease.NodeUID == "" {
			return false, nil
		}
		node, err := nodes.Get(ctx, lease.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if node.UID != lease.NodeUID || !nodeReadyWithKeyHash(node, expectedHash) {
			return false, nil
		}
	}
	return true, nil
}

func leaseNodeKeyAndUsernameConverged(ctx context.Context, nodes corev1client.NodeInterface,
	lease *byohHostLease, expectedHash string, key []byte, previousCiphertext string,
	requireCiphertextChange bool) (bool, error) {
	if lease == nil || lease.NodeName == "" || lease.NodeUID == "" || expectedHash == "" || len(key) == 0 {
		return false, nil
	}
	node, err := nodes.Get(ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if node.UID != lease.NodeUID || !nodeReadyWithKeyHash(node, expectedHash) {
		return false, nil
	}
	ciphertext := node.Annotations[byohUsernameAnno]
	if ciphertext == "" || (requireCiphertextChange && ciphertext == previousCiphertext) {
		return false, nil
	}
	username, err := decryptBYOHUsername(ciphertext, key)
	return err == nil && username == lease.Username, nil
}

func decryptBYOHUsername(ciphertext string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("decryption passphrase cannot be empty")
	}
	ciphertext = strings.ReplaceAll(ciphertext, "<wmcoMarker>", "\n")
	const startTag = "-----BEGIN ENCRYPTED DATA-----"
	const endTag = "-----END ENCRYPTED DATA-----"
	if !strings.HasPrefix(ciphertext, startTag) {
		ciphertext = startTag + "\n\n" + ciphertext + "\n" + endTag
	}
	block, err := armor.Decode(bytes.NewBufferString(ciphertext))
	if err != nil {
		return "", fmt.Errorf("decode encrypted username")
	}
	attempted := false
	message, err := openpgp.ReadMessage(block.Body, nil, func(_ []openpgp.Key, _ bool) ([]byte, error) {
		if attempted {
			return nil, fmt.Errorf("invalid passphrase")
		}
		attempted = true
		return key, nil
	}, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt username annotation")
	}
	plaintext, err := io.ReadAll(message.UnverifiedBody)
	if err != nil {
		return "", fmt.Errorf("read decrypted username")
	}
	return string(plaintext), nil
}

func privateKeySecretMatches(ctx context.Context, secrets corev1client.SecretInterface, expectedUID types.UID,
	key []byte) error {
	secret, err := secrets.Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if expectedUID == "" || secret.UID != expectedUID {
		return fmt.Errorf("cloud private key Secret object identity changed")
	}
	if !bytes.Equal(secret.Data[byohPrivateKeyDataKey], key) {
		return fmt.Errorf("cloud private key Secret does not contain the expected key")
	}
	return nil
}

func updatePrivateKeySecret(ctx context.Context, secrets corev1client.SecretInterface, expectedUID types.UID,
	key []byte) error {
	writeAttempted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		secret, err := secrets.Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if expectedUID == "" || secret.UID != expectedUID {
			return fmt.Errorf("cloud private key Secret object identity changed")
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[byohPrivateKeyDataKey] = append([]byte(nil), key...)
		writeAttempted = true
		_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		if verifyErr := privateKeySecretMatches(ctx, secrets, expectedUID, key); verifyErr != nil {
			return fmt.Errorf("verify cloud private key Secret update: %w", verifyErr)
		}
		return nil
	}
	if writeAttempted {
		if verifyErr := privateKeySecretMatches(ctx, secrets, expectedUID, key); verifyErr == nil {
			return nil
		} else {
			return errors.Join(fmt.Errorf("update cloud private key Secret: %w", err),
				fmt.Errorf("reconcile ambiguous cloud private key Secret update: %w", verifyErr))
		}
	}
	return fmt.Errorf("update cloud private key Secret: %w", err)
}

func (f *byohTestFixture) rotateBYOHPrivateKey(lease *byohHostLease) error {
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(f.ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("snapshot cloud private key Secret: %w", err)
	}
	originalKey := secret.Data[byohPrivateKeyDataKey]
	if len(originalKey) == 0 {
		return fmt.Errorf("cloud private key Secret has no private key")
	}
	if secret.UID == "" {
		return fmt.Errorf("cloud private key Secret has no object identity")
	}
	node, err := f.coreClient.Nodes().Get(f.ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	originalCiphertext := node.Annotations[byohUsernameAnno]
	if originalCiphertext == "" {
		return fmt.Errorf("selected BYOH Node has no encrypted username annotation")
	}
	originalHash := node.Annotations[byohPublicKeyHashAnno]
	expectedOriginalHash, err := publicKeyHash(originalKey)
	if err != nil {
		return err
	}
	if originalHash == "" || originalHash != expectedOriginalHash {
		return fmt.Errorf("selected BYOH Node does not have the expected original public key hash")
	}
	f.secretSnapshot, f.secretLease, f.secretRestored = secret.DeepCopy(), lease, false
	f.originalKeyHash = originalHash
	keyPath, err := generateTestPrivateKey()
	if err != nil {
		return err
	}
	defer func() { reportTemporaryKeyCleanup("removal", os.Remove(keyPath)) }()
	replacementKey, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	replacementHash, err := publicKeyHashFromPrivateKey(keyPath)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(replacementKey)
	if err != nil {
		return err
	}
	authorizedKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	originalClient, err := newWindowsSSHClient(f.ctx, lease, originalKey)
	if err != nil {
		return err
	}
	encodedPublicKey := base64.StdEncoding.EncodeToString([]byte(authorizedKey))
	_, appendErr := runPowerShellOverSSH(f.ctx, originalClient, fmt.Sprintf(
		`$key=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s')); $path="$env:ProgramData\ssh\administrators_authorized_keys"; if(-not (Select-String -LiteralPath $path -SimpleMatch $key -Quiet)){Add-Content -LiteralPath $path -Value $key}`,
		encodedPublicKey))
	closeErr := originalClient.close()
	if appendErr != nil {
		return appendErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := updatePrivateKeySecret(f.ctx, f.coreClient.Secrets(wmcoNamespace), f.secretSnapshot.UID,
		replacementKey); err != nil {
		return err
	}
	err = wait.PollUntilContextTimeout(f.ctx, 15*time.Second, 15*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			return leaseNodeKeyAndUsernameConverged(ctx, f.coreClient.Nodes(), lease, replacementHash,
				replacementKey, originalCiphertext, true)
		})
	if err != nil {
		return fmt.Errorf("replacement key annotations did not converge: %w", err)
	}
	if err := waitForSSHReady(f.ctx, lease, replacementKey); err != nil {
		return err
	}
	newClient, err := newWindowsSSHClient(f.ctx, lease, replacementKey)
	if err != nil {
		return err
	}
	defer newClient.close()
	return assertManagedServicesRunning(f.ctx, newClient)
}

func (f *byohTestFixture) restoreBYOHPrivateKeyWithContext(ctx context.Context) error {
	if f.secretSnapshot == nil || f.secretRestored {
		return nil
	}
	originalKey := f.secretSnapshot.Data[byohPrivateKeyDataKey]
	if len(originalKey) == 0 || f.secretLease == nil {
		return fmt.Errorf("cloud private key restoration snapshot is incomplete")
	}
	originalHash, err := publicKeyHash(originalKey)
	if err != nil {
		return err
	}
	if f.originalKeyHash == "" || originalHash != f.originalKeyHash {
		return fmt.Errorf("cloud private key restoration hash does not match the captured original hash")
	}
	if err := updatePrivateKeySecret(ctx, f.coreClient.Secrets(wmcoNamespace), f.secretSnapshot.UID,
		originalKey); err != nil {
		return err
	}
	err = wait.PollUntilContextTimeout(ctx, 15*time.Second, 15*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			return leaseNodeKeyAndUsernameConverged(ctx, f.coreClient.Nodes(), f.secretLease, originalHash,
				originalKey, "", false)
		})
	if err != nil {
		return fmt.Errorf("original key annotations did not converge: %w", err)
	}
	if err := waitForSSHReady(ctx, f.secretLease, originalKey); err != nil {
		return err
	}
	f.secretRestored = true
	return nil
}

func (f *byohTestFixture) restoreBYOHPrivateKey() error {
	return f.restoreBYOHPrivateKeyWithContext(f.ctx)
}

func (f *byohTestFixture) verifyLeaseDeconfigured(ctx context.Context, lease *byohHostLease) error {
	if lease.DeconfigurationStarted.IsZero() {
		return fmt.Errorf("deconfiguration mutation timestamp was not captured")
	}
	for _, message := range []string{"deconfiguring", "removing directories", "instance has been deconfigured"} {
		if err := f.waitForWMCOLogForLeaseSince(ctx, lease, message, lease.DeconfigurationStarted); err != nil {
			return err
		}
	}
	if err := waitForNodeDeleted(ctx, f.coreClient.Nodes(), lease.NodeName); err != nil {
		return err
	}
	client, err := f.sshClient(ctx, lease)
	if err != nil {
		return err
	}
	defer client.close()
	if err := assertManagedServicesStopped(ctx, client); err != nil {
		return err
	}
	if err := assertManagedDirectoriesRemoved(ctx, client); err != nil {
		return err
	}
	lease.ResetVerified = true
	return nil
}

func captureDeconfigurationBoundary(lease *byohHostLease) {
	lease.DeconfigurationStarted = time.Now().UTC()
}

func clearDeconfigurationBoundary(lease *byohHostLease) {
	lease.DeconfigurationStarted = time.Time{}
}

func runLeaseDeconfiguration(lease *byohHostLease, mutate, verify func() error) error {
	if !lease.RegistrationOwned {
		return fmt.Errorf("cannot deconfigure a registration that is not provably owned")
	}
	if !lease.Unregistered {
		if err := mutate(); err != nil {
			return err
		}
		if lease.DeconfigurationStarted.IsZero() {
			return fmt.Errorf("deconfiguration mutation timestamp was not captured")
		}
		lease.Registered, lease.Unregistered = false, true
	}
	return verify()
}

func (f *byohTestFixture) deconfigureLeaseWithContext(ctx context.Context, lease *byohHostLease) error {
	mutate := func() error {
		return unregisterBYOHInstances(ctx, f.coreClient.ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, &f.instancesState, lease.PreserveInstancesConfigMap,
			func() { captureDeconfigurationBoundary(lease) },
			func() { clearDeconfigurationBoundary(lease) })
	}
	return runLeaseDeconfiguration(lease, mutate, func() error { return f.verifyLeaseDeconfigured(ctx, lease) })
}

func (f *byohTestFixture) deconfigureLease(lease *byohHostLease) error {
	return f.deconfigureLeaseWithContext(f.ctx, lease)
}

func (f *byohTestFixture) releaseVerified(leases ...*byohHostLease) error {
	return releaseBYOHHosts(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), leases)
}

func runCleanupFallback(ctx context.Context, operation func(context.Context) error) error {
	return operation(ctx)
}

func newBYOHCleanupContexts(total, reserved time.Duration) (context.Context, context.Context, context.CancelFunc) {
	if total < 0 {
		total = 0
	}
	if reserved < 0 {
		reserved = 0
	}
	if reserved > total {
		reserved = total
	}
	outerCtx, outerCancel := context.WithTimeout(context.Background(), total)
	mainCtx, mainCancel := context.WithTimeout(outerCtx, total-reserved)
	return mainCtx, outerCtx, func() {
		mainCancel()
		outerCancel()
	}
}

type byohCleanupOperation struct {
	name string
	ctx  context.Context
	run  func(context.Context) error
}

func runBYOHCleanupOperations(operations []byohCleanupOperation) error {
	errs := make([]error, 0, len(operations))
	for _, operation := range operations {
		if err := operation.run(operation.ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", operation.name, err))
		}
	}
	return errors.Join(errs...)
}

func (f *byohTestFixture) cleanupWithBudget(total, reserved time.Duration) error {
	cleanupCtx, fallbackCtx, cleanupCancel := newBYOHCleanupContexts(total, reserved)
	defer cleanupCancel()
	defer f.cancel()
	var cleanupErrors []error
	var settleAfterIDMS []*byohHostLease
	if err := runBYOHCleanupOperations([]byohCleanupOperation{{name: "delete BYOH workload", ctx: cleanupCtx,
		run: f.deleteWorkload}}); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if err := f.restoreBYOHPrivateKeyWithContext(cleanupCtx); err != nil {
		fallbackErr := runCleanupFallback(fallbackCtx, f.restoreBYOHPrivateKeyWithContext)
		cleanupErrors = append(cleanupErrors, errors.Join(fmt.Errorf("restore cloud private key: %w", err),
			wrapOptionalError("bounded key-restoration fallback", fallbackErr)))
	}
	for _, lease := range f.leases {
		if lease.CleanupDone {
			continue
		}
		if lease.RegistrationOwned && !lease.ResetVerified {
			if err := f.deconfigureLeaseWithContext(cleanupCtx, lease); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("prove BYOH host reset: %w", err))
			}
		}
		if f.idmsName != "" && lease.LeaseID == f.idmsLeaseID {
			// OCP-82694 keeps the IDMS through every deconfiguration/path-removal attempt. Its
			// release or quarantine decision is always deferred until after IDMS cleanup.
			settleAfterIDMS = append(settleAfterIDMS, lease)
			continue
		}
		if lease.Disposable || lease.QuarantineRequired ||
			(lease.RegistrationAttempted && (!lease.RegistrationOwned || !lease.ResetVerified)) {
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine BYOH host: %w", err))
			}
		} else {
			// A physical host allocated but never registered was not mutated by WMCO.
			if !lease.RegistrationAttempted {
				lease.ResetVerified = true
			}
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return releaseBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("release BYOH host: %w", err))
			}
		}
	}
	// OCP-82694 intentionally retains the IDMS through deconfiguration and path removal checks.
	if err := f.deleteSyntheticIDMSWithContext(cleanupCtx); err != nil {
		fallbackErr := runCleanupFallback(fallbackCtx, f.deleteSyntheticIDMSWithContext)
		cleanupErrors = append(cleanupErrors, fmt.Errorf("delete synthetic IDMS: %w", err))
		if fallbackErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("bounded IDMS deletion fallback: %w", fallbackErr))
		}
		for _, lease := range settleAfterIDMS {
			if quarantineErr := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); quarantineErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine host after IDMS cleanup failure: %w", quarantineErr))
			}
		}
	} else {
		for _, lease := range settleAfterIDMS {
			if lease.Disposable || lease.QuarantineRequired || !lease.ResetVerified {
				if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
					return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
				}); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine host after IDMS cleanup: %w", err))
				}
				continue
			}
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return releaseBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("release BYOH host after IDMS cleanup: %w", err))
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func (f *byohTestFixture) cleanup() error {
	return f.cleanupWithBudget(byohCleanupLimit, byohFallbackLimit)
}
