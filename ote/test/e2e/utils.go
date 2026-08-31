package winc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
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

// waitVersionAnnotationReady polls until the WMCO version annotation is set on the node.
func waitVersionAnnotationReady(oc *exutil.CLI, windowsNodeName string, interval, timeout time.Duration) {
	err := wait.Poll(interval, timeout, func() (bool, error) {
		return checkVersionAnnotationReady(oc, windowsNodeName)
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "Timed out waiting for version annotation on node %s", windowsNodeName)
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

// waitWindowsNodesReady polls until the expected number of Windows nodes report Ready status.
func waitWindowsNodesReady(oc *exutil.CLI, expectedCount int, timeout time.Duration) {
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
				{"key": "os", "value": "Windows", "effect": "NoSchedule"},
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

	pollErr := wait.Poll(5*time.Second, 3*time.Minute, func() (bool, error) {
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
func createProject(oc *exutil.CLI, namespace string) {
	exists := oc.AsAdmin().WithoutNamespace().Run("get").Args("namespace", namespace).Execute()
	if exists == nil {
		e2e.Logf("Namespace %s already exists, skipping creation", namespace)
		return
	}
	oc.CreateSpecifiedNamespaceAsAdmin(namespace)
	err := compat_otp.SetNamespacePrivileged(oc, namespace)
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

// checkConnectivity repeatedly curls the given IP on port 80 and verifies the Windows web server
// response. Runs until the context is cancelled. Used with runInBackground for load-testing.
func checkConnectivity(ctx context.Context, IP string, delay int) error {
	url := "http://" + net.JoinHostPort(IP, "80")
	timeout := strconv.Itoa(delay)
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
		if err != nil {
			return fmt.Errorf("curl to %s failed: %v (output: %s)", url, err, string(out))
		}
		if !strings.Contains(string(out), "Windows Container Web Server") {
			return fmt.Errorf("unexpected response from LB %s: %s", url, string(out))
		}
		e2e.Logf("Checked LB connectivity of %s", url)
	}
}

// getServiceClusterIP returns the ClusterIP assigned to a Service.
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
	output, err := runDebugNodePS(oc, nodeName, image, cmd)
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
		for _, ms := range strings.Split(machineSets, " ") {
			if strings.Contains(ms, "winworker") || strings.Contains(ms, defaultWindowsMS) || strings.HasSuffix(ms, "-wm") {
				return ms
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
	msJSON, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machinesets.machine.openshift.io", sourceName, "-n", mcoNamespace, "-o=json").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get source MachineSet %s", sourceName)

	msJSON = strings.ReplaceAll(msJSON, sourceName, cloneName)

	tmpFile, err := os.CreateTemp("", "machineset-*.json")
	o.Expect(err).NotTo(o.HaveOccurred())
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(msJSON)
	o.Expect(err).NotTo(o.HaveOccurred())
	tmpFile.Close()

	err = oc.AsAdmin().WithoutNamespace().Run("apply").Args("-f", tmpFile.Name()).Execute()
	o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create cloned MachineSet %s", cloneName)

	err = oc.AsAdmin().WithoutNamespace().Run("scale").Args(
		"--replicas=0", "machinesets.machine.openshift.io", cloneName, "-n", mcoNamespace).Execute()
	o.Expect(err).NotTo(o.HaveOccurred())
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

// getWMCOTimestamp returns the start time of the running WMCO pod.
func getWMCOTimestamp(oc *exutil.CLI) string {
	wmcoTime, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "--selector", "name="+wmcoDeploymentName, "--field-selector=status.phase=Running", "-o=jsonpath={.items[0].status.startTime}", "-n", wmcoNamespace).Output()
	if err != nil || wmcoTime == "" {
		return ""
	}
	return wmcoTime
}

// checkWMCORestarted polls until the WMCO pod start time differs from the given startTime.
func checkWMCORestarted(oc *exutil.CLI, startTime string) (bool, error) {
	if startTime == "" {
		return false, fmt.Errorf("empty restart baseline: must capture WMCO timestamp before triggering restart")
	}
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
		return false, fmt.Errorf("error waiting for WMCO restart: %w", pollErr)
	}
	return restartDetected, nil
}

// restoreAPIServerTLS restores the original TLS configuration on apiserver/cluster.
func restoreAPIServerTLS(oc *exutil.CLI, origAdherence, origTLSProfile string) {
	if origAdherence == "" {
		if err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("apiserver/cluster", "--type=json",
			"-p", `[{"op":"remove","path":"/spec/tlsAdherence"}]`).Execute(); err != nil {
			e2e.Logf("Warning: could not remove tlsAdherence: %v", err)
		}
	} else {
		if err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("apiserver/cluster", "--type=merge",
			"-p", fmt.Sprintf(`{"spec":{"tlsAdherence":"%s"}}`, origAdherence)).Execute(); err != nil {
			e2e.Logf("Warning: could not restore tlsAdherence: %v", err)
		}
	}
	if origTLSProfile == "" {
		if err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("apiserver/cluster", "--type=json",
			"-p", `[{"op":"remove","path":"/spec/tlsSecurityProfile"}]`).Execute(); err != nil {
			e2e.Logf("Warning: could not remove tlsSecurityProfile: %v", err)
		}
	} else {
		if err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("apiserver/cluster", "--type=merge",
			"-p", fmt.Sprintf(`{"spec":{"tlsSecurityProfile":%s}}`, origTLSProfile)).Execute(); err != nil {
			e2e.Logf("Warning: could not restore tlsSecurityProfile: %v", err)
		}
	}
}

// createTLSCheckerPod creates a temporary Linux pod for running openssl commands
// and waits for it to reach Running state. Uses the cluster-local tools image
// from the OpenShift payload to support disconnected environments.
func createTLSCheckerPod(oc *exutil.CLI) string {
	podName := "tls-checker-" + getRandomString(5)

	toolsImage, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"istag", "tools:latest", "-n", "openshift",
		"-o=jsonpath={.image.dockerImageReference}").Output()
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get tools image from cluster")
	o.Expect(toolsImage).NotTo(o.BeEmpty(), "tools imagestream reference is empty")

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: checker
    image: %s
    command: ["sleep", "1800"]
  restartPolicy: Never`, podName, wmcoNamespace, toolsImage)

	err = createResourceFromString(oc, wmcoNamespace, manifest)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to create TLS checker pod")

	pollErr := wait.Poll(5*time.Second, 120*time.Second, func() (bool, error) {
		phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"pod", podName, "-n", wmcoNamespace,
			"-o=jsonpath={.status.phase}").Output()
		if err != nil {
			return false, nil
		}
		return phase == "Running", nil
	})
	o.Expect(pollErr).NotTo(o.HaveOccurred(), "TLS checker pod did not reach Running state")
	return podName
}

// deleteTLSCheckerPod deletes the TLS checker pod.
func deleteTLSCheckerPod(oc *exutil.CLI, podName string) {
	err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
		"pod", podName, "-n", wmcoNamespace,
		"--grace-period=0", "--force", "--ignore-not-found").Execute()
	if err != nil {
		e2e.Logf("Warning: failed to delete TLS checker pod %s: %v", podName, err)
	}
}

// runTLSCheck runs openssl s_client from the checker pod against the given host:port.
// tlsFlag can be "-tls1_2", "-tls1_3", or empty for default negotiation.
func runTLSCheck(oc *exutil.CLI, checkerPod, host, port, tlsFlag string) (string, error) {
	tlsArg := ""
	if tlsFlag != "" {
		tlsArg = " " + tlsFlag
	}
	cmd := fmt.Sprintf("echo | openssl s_client -connect %s%s 2>&1 || true", net.JoinHostPort(host, port), tlsArg)
	return execInPod(oc, wmcoNamespace, "pod/"+checkerPod, "bash", "-c", cmd)
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
