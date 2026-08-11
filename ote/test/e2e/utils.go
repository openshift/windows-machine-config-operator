package winc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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
)

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

func getRandomString(length int) string {
	o.Expect(length).To(o.BeNumerically(">", 0), "getRandomString requires a positive length")
	buff := make([]byte, length)
	_, err := rand.Read(buff)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to generate random bytes")
	str := base64.StdEncoding.EncodeToString(buff)
	return str[:length]
}

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

func deleteProject(oc *exutil.CLI, namespace string) {
	oc.DeleteSpecifiedNamespaceAsAdmin(namespace)
}

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

func haveMetricsServer(oc *exutil.CLI) bool {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("apiservice", "v1beta1.metrics.k8s.io").Output()
	return err == nil && strings.Contains(output, "True")
}

func getWindowsBuildID(oc *exutil.CLI, nodeID string) (string, error) {
	build, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("node", nodeID, "-o=jsonpath={.metadata.labels.node\\.kubernetes\\.io\\/windows-build}").Output()
	return build, err
}

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

func buildInvokeWebRequestCommand(url string) string {
	return fmt.Sprintf(
		"$r = Invoke-WebRequest -Uri %s -UseBasicParsing -ErrorAction SilentlyContinue; "+
			"if ($r.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($r.Content) } else { $r.Content }",
		url)
}

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

func checkWindowsServiceRunning(oc *exutil.CLI, nodeName, image, serviceName string) (bool, error) {
	cmd := fmt.Sprintf("Get-Service '%s' | Select-Object -ExpandProperty Status", serviceName)
	output, err := runDebugNodePS(oc, nodeName, image, cmd)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == "Running", nil
}

func getServiceBinPath(oc *exutil.CLI, nodeName, image, serviceName string) (string, error) {
	cmd := fmt.Sprintf("Get-CimInstance -ClassName Win32_Service | Where-Object { $_.Name -eq '%s' } | Select-Object -ExpandProperty PathName", serviceName)
	output, err := runDebugNodePS(oc, nodeName, image, cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func setServiceBinPath(oc *exutil.CLI, nodeName, image, serviceName, binPath string) error {
	cmd := fmt.Sprintf(`sc.exe config %s binPath= "%s"`, serviceName, binPath)
	output, err := runDebugNodePS(oc, nodeName, image, cmd)
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
