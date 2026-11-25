package winc

import (
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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	aws "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
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
	machinesGVR       = schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}
	pubKeyHashAnno    = "windowsmachineconfig.openshift.io/pub-key-hash"
	windowsDebugImage = "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022"
	// TODO: add a Windows Server 2025 debug image (mcr.microsoft.com/powershell:lts-nanoserver-ltsc2025) once
	// Microsoft publishes one. MCR currently has no PowerShell image for Windows Server 2025, so workloads on a
	// 2025 node must be supplied with an image that ships pwsh.exe for that build.
	linuxDebugImage  = "registry.access.redhat.com/ubi9/ubi:latest"
	defaultWindowsMS = "windows"

	tlsRecoveryClusterOperators = []string{"authentication", "kube-apiserver", "openshift-apiserver"}
)

var (
	podLogResourcePattern = regexp.MustCompile(`(?i)(?:^|[\s(])pods/log\s+(?:"([^"]+)"|([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?))(?:[\s):]|$)`)
	cliAPIRefusalPattern  = regexp.MustCompile(`(?i)the connection to the server \S+:\d+ was refused - did you specify the right host or port\?`)
)

const (
	tlsRecoveryPollInterval        = 10 * time.Second
	tlsRecoveryMinimumStablePeriod = 3 * time.Minute
	tlsRecoveryAPITimeout          = 5 * time.Minute
	// tlsRecoveryControlPlaneTimeout bounds the final settle wait after a TLS
	// profile change is reverted. The change re-renders MachineConfigs, so the
	// control-plane nodes roll and the authentication operator stays
	// unavailable well past API readiness; 22 minutes was observed on a
	// three-control-plane cluster after repeated profile changes.
	tlsRecoveryControlPlaneTimeout = 30 * time.Minute
	// tlsEndpointReadyTimeout bounds the WMCO metrics endpoint readiness probe.
	// A TLS profile change restarts WMCO while the control plane re-renders and
	// rolls; retryable probe failures poll for up to this long before the case
	// fails.
	tlsEndpointReadyTimeout        = 10 * time.Minute
	tlsRecoveryRestartTimeout      = 6 * time.Minute
	tlsRecoveryDeploymentTimeout   = 5 * time.Minute
	tlsRecoveryWindowsNodesTimeout = 5 * time.Minute
	// tlsRestorationTimeout is shared by all post-patch recovery phases. The
	// extra minute covers in-flight CLI requests at phase boundaries.
	tlsRestorationTimeout = tlsRecoveryControlPlaneTimeout + tlsRecoveryRestartTimeout +
		tlsRecoveryDeploymentTimeout + tlsRecoveryWindowsNodesTimeout + tlsRecoveryAPITimeout + time.Minute
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

// versionAnnotationReady reports whether the node still exists and carries the WMCO version
// annotation, without failing the spec. WMCO deletes the Machine behind a node it remediates, so a
// node can disappear while a caller is waiting on it.
func versionAnnotationReady(oc *exutil.CLI, windowsNodeName string, interval, timeout time.Duration) bool {
	err := wait.Poll(interval, timeout, func() (bool, error) {
		return checkVersionAnnotationReady(oc, windowsNodeName)
	})
	return err == nil
}

// annotateNodeForReconfiguration asks WMCO to reconfigure a Windows node by setting invalidVersion
// on it and waits for the annotation to be restored. If the node is replaced while the spec runs,
// another current node of the same MachineSet is selected and annotated instead of failing on a
// stale name. When the replacement has not registered yet, the helper waits for it within timeout
// instead of burning the attempts immediately.
func annotateNodeForReconfiguration(oc *exutil.CLI, machineSetName, preferred string, interval, timeout time.Duration) string {
	node := preferred
	for attempt := 0; attempt < 3; attempt++ {
		node = chooseDrainTarget(oc, machineSetName, node)
		if !windowsNodeExists(oc, node) {
			replacement, found := waitForMachineSetNode(oc, machineSetName, timeout)
			if !found {
				e2e.Logf("No node of MachineSet %s registered within %v", machineSetName, timeout)
				break
			}
			node = replacement
		}
		_, err := oc.AsAdmin().WithoutNamespace().Run("annotate").Args(
			"node", node, "--overwrite",
			"windowsmachineconfig.openshift.io/version=invalidVersion").Output()
		if err != nil {
			e2e.Logf("Failed to annotate node %s, re-selecting: %v", node, err)
			continue
		}
		if versionAnnotationReady(oc, node, interval, timeout) {
			return node
		}
		e2e.Logf("Node %s was replaced while waiting for the version annotation, re-selecting", node)
	}
	e2e.Failf("could not trigger a WMCO reconfiguration on a stable Windows node after 3 attempts")
	return ""
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

// windowsNodeAllocatableCPU returns the allocatable CPU of every Windows node, in milliCPU.
func windowsNodeAllocatableCPU(oc *exutil.CLI) []int64 {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].status.allocatable.cpu}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	cpus := []int64{}
	for _, raw := range strings.Fields(out) {
		quantity, parseErr := resource.ParseQuantity(raw)
		o.Expect(parseErr).NotTo(o.HaveOccurred(), "Failed to parse allocatable CPU %q", raw)
		cpus = append(cpus, quantity.MilliValue())
	}
	return cpus
}

// autoscalerWorkloadReplicas returns the workload replica count that leaves exactly one pod
// Pending, so the cluster autoscaler has a reason to add a node. Capacity is computed per node:
// pods are not evenly divisible across nodes, and on vSphere a 3.5 CPU node fits only one 2-CPU
// pod while an Azure 8 vCPU node fits three. A flat node count is therefore too small on Azure
// and a flat total-CPU division is too large on vSphere.
func autoscalerWorkloadReplicas(nodeCPU []int64, podCPU int64) int {
	replicas := len(nodeCPU) + 1
	if podCPU <= 0 {
		return replicas
	}
	capacity := 0
	for _, cpu := range nodeCPU {
		if cpu > 0 {
			capacity += int(cpu / podCPU)
		}
	}
	if capacity+1 > replicas {
		replicas = capacity + 1
	}
	return replicas
}

// scaleDeploymentReplicasOnly changes a deployment's replica count without waiting for the pods to
// become ready. Used when the extra replicas are meant to stay Pending so the cluster autoscaler
// reacts; waiting for them here would block before the autoscaler can add a node.
func scaleDeploymentReplicasOnly(oc *exutil.CLI, deploymentName string, replicas int, namespace string) error {
	_, err := oc.AsAdmin().WithoutNamespace().Run("scale").
		Args("--replicas="+strconv.Itoa(replicas), "deployment", deploymentName, "-n", namespace).Output()
	if err != nil {
		return fmt.Errorf("failed to scale deployment %s to %d replicas: %w", deploymentName, replicas, err)
	}
	return nil
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
// nodeNameForIP resolves the node that owns nodeIP without asserting, so a caller can poll for the
// node while it is still registering.
func nodeNameForIP(oc *exutil.CLI, nodeIP string) (string, error) {
	goTpl := fmt.Sprintf(
		`{{range .items}}{{$name := .metadata.name}}{{range .status.addresses}}`+
			`{{if and (eq .type "InternalIP") (eq .address "%s")}}{{$name}}{{end}}`+
			`{{end}}{{end}}`, nodeIP)
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-o=go-template="+goTpl).Output()
	if err != nil {
		return "", fmt.Errorf("failed to resolve node name for IP %s: %w", nodeIP, err)
	}
	return strings.TrimSpace(out), nil
}

func getNodeNameFromIP(oc *exutil.CLI, nodeIP string) string {
	nodeName, err := nodeNameForIP(oc, nodeIP)
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(nodeName).NotTo(o.BeEmpty(), "no node found with InternalIP %s", nodeIP)
	return nodeName
}

// debugWindowsNodesNotReady captures comprehensive debug information when Windows nodes fail to become Ready
func debugWindowsNodesNotReady(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("===== DEBUG: Windows Nodes Not Ready - Capturing Diagnostic Information (cancellable) =====")
	diagnostics := &windowsNodeDiagnostics{}
	runDiagnostics(ctx, oc, captureWindowsNodeStatus, diagnostics.captureNodeConditions,
		captureWMCOStatus, diagnostics.captureNodeEvents, captureWICDStatus,
		captureMachineStatus, diagnostics.captureServiceStatus)
	if ctx.Err() == nil {
		e2e.Logf("===== END DEBUG =====")
	}
}

// runDiagnostics runs supplied diagnostic steps in order using their shared context.
// Cancellation between steps, including after the last, reports completed work once; in-flight calls must observe context.
func runDiagnostics(ctx context.Context, oc *exutil.CLI, steps ...func(context.Context, *exutil.CLI)) {
	for completed := 0; ; completed++ {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			e2e.Logf("Diagnostic timeout reached after %d completed steps, stopping debug capture", completed)
			return
		case context.Canceled:
			e2e.Logf("Diagnostic capture canceled after %d completed steps, stopping debug capture", completed)
			return
		}
		if completed == len(steps) {
			return
		}
		steps[completed](ctx, oc)
	}
}

type windowsNodeDiagnostics struct {
	nodeNames []string
}

var windowsDiagnosticOutput = func(oc *exutil.CLI, args ...string) (string, error) {
	return oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
}

var windowsDiagnosticHostProcess = runHostProcessPSWithContext

// captureWindowsNodeStatus reports Windows node readiness columns through the diagnostic output binding.
// Query failures are logged; this legacy output call does not receive the supplied context.
func captureWindowsNodeStatus(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== Windows Node Status ===")
	nodeOutput, err := windowsDiagnosticOutput(oc,
		"nodes", "-l", windowsNodeLabel,
		"-o=custom-columns=NAME:.metadata.name,STATUS:.status.conditions[?(@.type==\"Ready\")].status,REASON:.status.conditions[?(@.type==\"Ready\")].reason,MESSAGE:.status.conditions[?(@.type==\"Ready\")].message")
	if err != nil {
		e2e.Logf("ERROR: Failed to get node status: %v", err)
	} else {
		e2e.Logf("Node status:\n%s", nodeOutput)
	}
}

// captureNodeConditions stores discovered Windows node names and logs their condition types.
// Per-node captures check cancellation, and the saved name list is reused by later diagnostic steps.
func (diagnostics *windowsNodeDiagnostics) captureNodeConditions(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== Windows Node Conditions ===")
	nodeNames, err := windowsDiagnosticOutput(oc,
		"nodes", "-l", windowsNodeLabel,
		"-o=jsonpath={.items[*].metadata.name}")
	diagnostics.nodeNames = strings.Fields(nodeNames)
	if err == nil {
		for _, nodeName := range diagnostics.nodeNames {
			if ctx.Err() != nil {
				return
			}
			e2e.Logf("--- Node: %s ---", nodeName)
			conditionsOutput, err := windowsDiagnosticOutput(oc,
				"node", nodeName,
				"-o=jsonpath={.status.conditions[*].type}")
			if err == nil {
				e2e.Logf("Condition types: %s", conditionsOutput)
			}
		}
	}
}

// captureWMCOStatus reports WMCO pod names and phases in the operator namespace.
// Query errors are logged without aborting the diagnostic sequence; the output binding is not context-aware.
func captureWMCOStatus(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== WMCO Operator Pod Status ===")
	wmcoPods, err := windowsDiagnosticOutput(oc,
		"pods", "-n", wmcoNamespace,
		"-l", "app=windows-machine-config-operator",
		"-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase")
	if err != nil {
		e2e.Logf("ERROR: Failed to get WMCO pod status: %v", err)
	} else {
		e2e.Logf("WMCO pods:\n%s", wmcoPods)
	}
}

// captureNodeEvents uses the saved node list to log event type/reason summaries.
// It checks cancellation before each node and ignores query failures without requesting free-form event messages.
func (diagnostics *windowsNodeDiagnostics) captureNodeEvents(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== Events for Windows Nodes (summary) ===")
	for _, nodeName := range diagnostics.nodeNames {
		if ctx.Err() != nil {
			return
		}
		events, err := windowsDiagnosticOutput(oc,
			"events", "--all-namespaces",
			"--field-selector", fmt.Sprintf("involvedObject.name=%s", nodeName),
			"--sort-by=.lastTimestamp",
			"-o=custom-columns=TYPE:.type,REASON:.reason")
		if err == nil {
			e2e.Logf("Events for %s:\n%s", nodeName, events)
		}
	}
}

// captureWICDStatus reports WICD pod placement, phase, and readiness columns.
// Pod selection stays scoped to the operator namespace, and query failures are logged rather than returned.
func captureWICDStatus(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== WICD Pod Status ===")
	wicdPods, err := windowsDiagnosticOutput(oc,
		"pods", "-n", wmcoNamespace,
		"-l", "app=windows-instance-config-daemon",
		"-o=custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,READY:.status.conditions[?(@.type==\"Ready\")].status")
	if err != nil {
		e2e.Logf("ERROR: Failed to get WICD pods: %v", err)
	} else {
		e2e.Logf("WICD pods:\n%s", wicdPods)
	}
}

// captureMachineStatus reports machine phase, node reference, and provider-state columns.
// Missing or failed machine queries remain informational instead of aborting diagnostic sequencing.
func captureMachineStatus(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== Machine Status ===")
	machineOutput, err := windowsDiagnosticOutput(oc,
		"machines", "-n", "openshift-machine-api",
		"-o=custom-columns=NAME:.metadata.name,PHASE:.status.phase,NODE:.status.nodeRef.name,PROVIDERSTATE:.status.providerStatus.instanceState")
	if err != nil {
		e2e.Logf("INFO: No machines or failed to query: %v", err)
	} else {
		e2e.Logf("Machines:\n%s", machineOutput)
	}
}

// captureServiceStatus queries service state for previously discovered NotReady nodes.
// It forwards the diagnostic context into HostProcess work and skips Ready nodes or failed readiness reads.
func (diagnostics *windowsNodeDiagnostics) captureServiceStatus(ctx context.Context, oc *exutil.CLI) {
	e2e.Logf("=== Windows Node Service Status ===")
	captureWindowsServiceStatus(ctx, diagnostics.nodeNames,
		func(nodeName string) (bool, error) {
			nodeReady, err := windowsDiagnosticOutput(oc,
				"node", nodeName,
				"-o=jsonpath={.status.conditions[?(@.type==\"Ready\")].status}")
			return strings.TrimSpace(nodeReady) == "True", err
		},
		func(ctx context.Context, nodeName string) (string, error) {
			return windowsDiagnosticHostProcess(ctx, oc, nodeName, windowsDebugImage,
				"Get-Service kubelet,windows-instance-config-daemon,containerd | Format-Table -AutoSize | Out-String -Width 200")
		})
}

// captureWindowsServiceStatus visits NotReady nodes to log service-status output.
// Cancellation is checked around readiness reads and before later nodes; individual query failures do not stop the loop.
func captureWindowsServiceStatus(ctx context.Context, nodeNames []string,
	isNodeReady func(string) (bool, error), getServiceStatus func(context.Context, string) (string, error)) {
	for _, nodeName := range nodeNames {
		if ctx.Err() != nil {
			return
		}

		nodeReady, err := isNodeReady(nodeName)
		if ctx.Err() != nil {
			return
		}
		if err != nil || nodeReady {
			continue
		}

		e2e.Logf("--- Service status from NotReady node: %s ---", nodeName)
		serviceStatus, err := getServiceStatus(ctx, nodeName)
		if err != nil {
			e2e.Logf("ERROR: Failed to get service status from %s: %v", nodeName, err)
		} else {
			e2e.Logf("Windows service status on %s:\n%s", nodeName, serviceStatus)
		}
	}
}

// pollWindowsNodesReady polls until the expected number of Windows nodes report Ready status.
func pollWindowsNodesReady(oc *exutil.CLI, expectedCount int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return pollWindowsNodesReadyWithContext(ctx, oc, expectedCount)
}

// pollWindowsNodesReadyWithContext captures diagnostics after five minutes of waiting.
// Diagnostics are limited to two minutes and the caller's remaining deadline.
func pollWindowsNodesReadyWithContext(ctx context.Context, oc *exutil.CLI, expectedCount int) error {
	debugCaptured := false
	debugThreshold := 5 * time.Minute
	diagnosticTimeout := 2 * time.Minute
	startTime := time.Now()

	return wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		output, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"nodes", "-l", windowsNodeLabel,
			"-o=jsonpath={.items[*].status.conditions[?(@.type==\"Ready\")].status}"))
		if err != nil {
			e2e.Logf("Error querying Windows nodes: %v", err)
			if !isTransientTLSRecoveryError(err) {
				return false, fmt.Errorf("querying Windows nodes: %w", err)
			}
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
			diagnosticCtx, cancel := context.WithTimeout(ctx, diagnosticTimeout)
			debugWindowsNodesNotReady(diagnosticCtx, oc)
			cancel()
			debugCaptured = true
		}

		return readyCount >= expectedCount, nil
	})
}

func waitWindowsNodesReady(oc *exutil.CLI, expectedCount int, timeout time.Duration) {
	pollErr := pollWindowsNodesReady(oc, expectedCount, timeout)
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf("timed out waiting for %d Windows nodes to be Ready after %v", expectedCount, timeout))
}

// nodeReadinessQuery emits one "name=ReadyConditionStatus" pair per node.
const nodeReadinessQuery = `-o=jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status} {end}`

// nodeReadiness parses the pairs emitted by nodeReadinessQuery into a node name to Ready map.
func nodeReadiness(output string) map[string]bool {
	readiness := map[string]bool{}
	for _, pair := range strings.Fields(output) {
		name, status, found := strings.Cut(pair, "=")
		if !found || name == "" {
			continue
		}
		readiness[name] = status == "True"
	}
	return readiness
}

// readyMachineSetNodes returns the names in machineNodeNames that report Ready in readiness.
// Only nodes registered by the Machines of the MachineSet are considered: waitWindowsNodesReady
// counts every Windows node in the cluster, so a node belonging to another MachineSet or left over
// from another test satisfies it even when no Machine of the set has joined.
func readyMachineSetNodes(machineNodeNames []string, readiness map[string]bool) []string {
	ready := []string{}
	seen := make(map[string]struct{}, len(machineNodeNames))
	for _, name := range machineNodeNames {
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		if readiness[name] {
			ready = append(ready, name)
		}
	}
	return ready
}

// machineSetNodeNames returns the node names registered by the Machines owned by machineSetName.
// Machines that are being deleted are ignored so that a node from an earlier scale-up cannot
// satisfy a later one.
func machineSetNodeNames(oc *exutil.CLI, machineSetName string) ([]string, error) {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machines.machine.openshift.io", "-n", mcoNamespace,
		"-l", "machine.openshift.io/cluster-api-machineset="+machineSetName,
		"-o=go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.status.nodeRef.name}} {{end}}{{end}}",
	).Output()
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, name := range strings.Fields(out) {
		// A Machine whose node has not registered yet renders an empty value in the template.
		if name != "" && name != "<no value>" {
			names = append(names, name)
		}
	}
	return names, nil
}

// waitForMachineSetNodesReady waits until expectedCount nodes registered by the Machines of
// machineSetName are Ready.
func waitForMachineSetNodesReady(oc *exutil.CLI, machineSetName string, expectedCount int, timeout time.Duration) {
	pollErr := wait.Poll(15*time.Second, timeout, func() (bool, error) {
		nodeNames, err := machineSetNodeNames(oc, machineSetName)
		if err != nil {
			e2e.Logf("Error listing the machines of %s: %v", machineSetName, err)
			return false, nil
		}
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", nodeReadinessQuery).Output()
		if err != nil {
			e2e.Logf("Error reading node readiness: %v", err)
			return false, nil
		}
		ready := readyMachineSetNodes(nodeNames, nodeReadiness(out))
		e2e.Logf("MachineSet %s nodes Ready: %d/%d (machine nodes: %v)", machineSetName, len(ready), expectedCount, nodeNames)
		return len(ready) >= expectedCount, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf(
		"MachineSet %s did not have %d Ready machine nodes within %v (%s)",
		machineSetName, expectedCount, timeout, machinesetTimeoutDiagnostics(oc, machineSetName)))
}

// machineSetSnapshot is a point-in-time view of a MachineSet and of the nodes registered by its
// Machines. Disruptive specs leave WMCO remediating Machines for a while after they return, and a
// spec that caches node names then fails when a node is replaced under it.
type machineSetSnapshot struct {
	Replicas int
	Machines []string // Machines owned by the MachineSet that are not being deleted, sorted
	Deleting int      // Machines owned by the MachineSet that carry a deletion timestamp
	Ready    int      // registered nodes of those Machines that report Ready
}

// settledAfter reports whether the MachineSet has converged and stopped changing: it owns exactly
// replicas Machines, none of them is being deleted, every Machine has a Ready node, the Machine
// list matches the previous observation, and that list has been unchanged for at least settle.
func (s machineSetSnapshot) settledAfter(prev machineSetSnapshot, unchangedFor, settle time.Duration) bool {
	return s.Deleting == 0 &&
		len(s.Machines) == s.Replicas &&
		s.Ready >= s.Replicas &&
		slices.Equal(s.Machines, prev.Machines) &&
		unchangedFor >= settle
}

// observeWindowsMachineSet reads the current state of machineSetName, including the Machines that
// are already being deleted, so that a replacement in flight is not mistaken for a converged set.
func observeWindowsMachineSet(oc *exutil.CLI, machineSetName string) (machineSetSnapshot, error) {
	replicas, err := getMachineSetReplicas(oc, machineSetName)
	if err != nil {
		return machineSetSnapshot{}, err
	}
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machines.machine.openshift.io", "-n", mcoNamespace,
		"-l", "machine.openshift.io/cluster-api-machineset="+machineSetName,
		"-o=go-template={{range .items}}{{.metadata.name}}|{{if .metadata.deletionTimestamp}}deleting{{else}}active{{end}}|{{.status.nodeRef.name}} {{end}}",
	).Output()
	if err != nil {
		return machineSetSnapshot{}, err
	}
	snapshot := machineSetSnapshot{Replicas: replicas}
	nodeNames := []string{}
	for _, field := range strings.Fields(out) {
		parts := strings.SplitN(field, "|", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[1] == "deleting" {
			snapshot.Deleting++
			continue
		}
		snapshot.Machines = append(snapshot.Machines, parts[0])
		if parts[2] != "" && parts[2] != "<no value>" {
			nodeNames = append(nodeNames, parts[2])
		}
	}
	sort.Strings(snapshot.Machines)

	readinessOut, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", nodeReadinessQuery).Output()
	if err != nil {
		return machineSetSnapshot{}, err
	}
	snapshot.Ready = len(readyMachineSetNodes(nodeNames, nodeReadiness(readinessOut)))
	return snapshot, nil
}

// waitForWindowsMachineSetSettled waits until the Windows MachineSet has converged and its
// Machines have been unchanged for settle. Call it at the end of specs that delete or recreate
// Machines, and at the start of specs that cache node names: WMCO remediates Machines by deleting
// them and the MachineSet then replaces them, so a node can disappear minutes after the spec that
// caused the churn returned.
func waitForWindowsMachineSetSettled(ctx context.Context, oc *exutil.CLI, machineSetName string, timeout, settle time.Duration) {
	var (
		prev       machineSetSnapshot
		havePrev   bool
		lastChange = time.Now()
	)
	pollErr := wait.PollUntilContextTimeout(ctx, 15*time.Second, timeout, true, func(context.Context) (bool, error) {
		snapshot, err := observeWindowsMachineSet(oc, machineSetName)
		if err != nil {
			e2e.Logf("Error observing MachineSet %s: %v", machineSetName, err)
			return false, nil
		}
		if !havePrev || !slices.Equal(prev.Machines, snapshot.Machines) {
			lastChange = time.Now()
			havePrev = true
		}
		unchangedFor := time.Since(lastChange)
		settled := snapshot.settledAfter(prev, unchangedFor, settle)
		e2e.Logf("MachineSet %s settling: replicas=%d machines=%v deleting=%d ready=%d unchangedFor=%v",
			machineSetName, snapshot.Replicas, snapshot.Machines, snapshot.Deleting, snapshot.Ready,
			unchangedFor.Round(time.Second))
		prev = snapshot
		return settled, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr, fmt.Sprintf(
		"MachineSet %s did not settle within %v (%s)",
		machineSetName, timeout, machinesetTimeoutDiagnostics(oc, machineSetName)))
}

// windowsNodeExists reports whether the named node is still registered in the cluster.
func windowsNodeExists(oc *exutil.CLI, nodeName string) bool {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"node", nodeName, "-o=jsonpath={.metadata.name}").Output()
	return err == nil && strings.TrimSpace(out) == nodeName
}

// chooseDrainTarget returns preferred when its node still exists and otherwise the first node
// registered by the Machines of machineSetName that does. Disruptive specs run while WMCO may still
// replace Machines, so a node captured at the start of a spec can be gone by the time it is used;
// restricting the fallback to the MachineSet also keeps unmanaged Windows nodes out of the test.
func chooseDrainTarget(oc *exutil.CLI, machineSetName, preferred string) string {
	if windowsNodeExists(oc, preferred) {
		return preferred
	}
	nodes, err := machineSetNodeNames(oc, machineSetName)
	if err != nil {
		e2e.Logf("Could not list the machines of %s: %v", machineSetName, err)
		return preferred
	}
	for _, host := range nodes {
		if host != preferred && windowsNodeExists(oc, host) {
			e2e.Logf("Node %s no longer exists; using %s instead", preferred, host)
			return host
		}
	}
	return preferred
}

// waitForMachineSetNode waits until machineSetName has at least one registered node and returns it.
// Used when the replacement for a deleted node is still being provisioned.
// firstExistingNode returns the first candidate for which exists reports true. Machine node
// references can be stale: a Machine that is not being deleted keeps the name of a Node that has
// already been removed, so a candidate list alone does not prove a usable node.
func firstExistingNode(candidates []string, exists func(string) bool) (string, bool) {
	for _, name := range candidates {
		if name != "" && exists(name) {
			return name, true
		}
	}
	return "", false
}

func waitForMachineSetNode(oc *exutil.CLI, machineSetName string, timeout time.Duration) (string, bool) {
	var node string
	err := wait.PollUntilContextTimeout(context.Background(), 10*time.Second, timeout, true, func(context.Context) (bool, error) {
		nodes, err := machineSetNodeNames(oc, machineSetName)
		if err != nil || len(nodes) == 0 {
			return false, nil
		}
		found, ok := firstExistingNode(nodes, func(name string) bool {
			return windowsNodeExists(oc, name)
		})
		if !ok {
			return false, nil
		}
		node = found
		return true, nil
	})
	return node, err == nil
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

// getRandomDNSLabel returns a random lowercase hexadecimal string suitable for
// use in a Kubernetes resource name.
func getRandomDNSLabel(length int) string {
	o.Expect(length).To(o.BeNumerically(">", 0), "getRandomDNSLabel requires a positive length")
	buff := make([]byte, (length+1)/2)
	_, err := rand.Read(buff)
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to generate random bytes")
	return fmt.Sprintf("%x", buff)[:length]
}

// createProject creates the test namespace and sets privileged SCC. Tests use fixed namespace
// names, so a namespace that already exists belongs to an earlier attempt on the same cluster.
// Reusing it silently makes reruns fail in confusing ways (stale workloads, deleted StorageClasses)
// and a Terminating namespace cannot be written to at all, so both cases are reported explicitly
// instead of being ignored. The test does not delete a namespace it does not own.
func createProject(oc *exutil.CLI, namespace string) {
	phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"namespace", namespace, "-o=jsonpath={.status.phase}").Output()
	if err == nil {
		// Each test owns a fixed namespace named after its test case, so a namespace that is still
		// there belongs to an earlier attempt of this same test. Reusing it would make the run depend
		// on state left behind, so it is removed and recreated instead of failing and asking for a
		// manual cleanup.
		e2e.Logf("Namespace %s already exists (phase %s); deleting and recreating it", namespace, strings.TrimSpace(phase))
		if _, deleteErr := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"namespace", namespace, "--wait=false").Output(); deleteErr != nil {
			e2e.Failf("failed to delete the leftover namespace %s: %v", namespace, deleteErr)
		}
		o.Expect(waitForNamespaceDeleted(oc, namespace, 5*time.Minute)).To(o.BeTrue(),
			"namespace %s did not disappear and cannot be recreated by this test; check its finalizers", namespace)
	}
	oc.CreateSpecifiedNamespaceAsAdmin(namespace)
	err = compat_otp.SetNamespacePrivileged(oc, namespace)
	o.Expect(err).NotTo(o.HaveOccurred())
}

// deleteProject deletes the given namespace.
func deleteProject(oc *exutil.CLI, namespace string) {
	oc.DeleteSpecifiedNamespaceAsAdmin(namespace)
}

// windowsWebServerConfig configures the Windows web server manifest generated by
// generateWindowsWebServerYAML. Zero values keep the plain web server workload.
type windowsWebServerConfig struct {
	Name             string
	Namespace        string
	Image            string
	Replicas         int
	IncludeService   bool
	ResourceLimits   string
	RuntimeClassName string
	// NodeSelector adds extra node selector entries, one "key: value" per line.
	NodeSelector string
	// PVCName mounts the named PersistentVolumeClaim at C:/html/ and serves the web server
	// content from it, so every replica sharing the claim serves the same page.
	PVCName string
	// ImagePullSecret references an existing image pull secret in the namespace.
	ImagePullSecret string
	// MultilineCommand renders the PowerShell command as a block scalar instead of a single
	// long line, which keeps the manifest readable and safely quoted.
	MultilineCommand bool
}

// generateWindowsWebServerYAML returns a YAML manifest for a Windows web server Deployment
// (and optionally a LoadBalancer Service). Used to deploy Windows workloads for connectivity,
// scaling and storage tests.
func generateWindowsWebServerYAML(cfg windowsWebServerConfig) string {
	// A web server workload is single replica unless a test explicitly scales it.
	if cfg.Replicas == 0 {
		cfg.Replicas = 1
	}
	var sb strings.Builder
	if cfg.IncludeService {
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
`, cfg.Name, cfg.Name, cfg.Name))
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
`, cfg.Name, cfg.Name, cfg.Name, cfg.Replicas, cfg.Name, cfg.Name))
	if cfg.RuntimeClassName != "" {
		sb.WriteString(fmt.Sprintf("      runtimeClassName: %s\n", cfg.RuntimeClassName))
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
`)
	if cfg.PVCName != "" {
		sb.WriteString(fmt.Sprintf(`      volumes:
      - name: test-volume
        persistentVolumeClaim:
          claimName: %s
`, cfg.PVCName))
	}
	sb.WriteString(fmt.Sprintf(`      containers:
      - name: windowswebserver
        image: %s
        imagePullPolicy: IfNotPresent
`, cfg.Image))
	if cfg.PVCName != "" {
		sb.WriteString(`        volumeMounts:
        - mountPath: C:/html/
          name: test-volume
`)
	}
	sb.WriteString(`        securityContext:
          runAsNonRoot: false
          windowsOptions:
            runAsUserName: "ContainerAdministrator"
`)
	if cfg.ResourceLimits != "" {
		sb.WriteString(fmt.Sprintf(`        resources:
          limits:
            cpu: %s
            memory: 1Gi
          requests:
            cpu: %s
            memory: 512Mi
`, cfg.ResourceLimits, cfg.ResourceLimits))
	}
	if cfg.MultilineCommand {
		sb.WriteString(`        command:
        - pwsh.exe
        - -command
        - |
`)
		if cfg.PVCName != "" {
			sb.WriteString(`          if (-not (Test-Path C:/html/index.html)) { echo '<html><body><H1>Windows Container Web Server</H1></body></html>' > C:/html/index.html; };
          $listener = New-Object System.Net.HttpListener;
          $listener.Prefixes.Add('http://*:80/');
          $listener.Start();
          Write-Host('Listening at http://*:80/');
          while ($listener.IsListening) {
            $context = $listener.GetContext();
            $response = $context.Response;
            $content=Get-Content C:/html/index.html -Raw;
            $buffer = [System.Text.Encoding]::UTF8.GetBytes($content);
            $response.ContentLength64 = $buffer.Length;
            $response.OutputStream.Write($buffer, 0, $buffer.Length);
            $response.Close();
          };
`)
		} else {
			sb.WriteString(`          $listener = New-Object System.Net.HttpListener;
          $listener.Prefixes.Add('http://*:80/');
          $listener.Start();
          Write-Host('Listening at http://*:80/');
          while ($listener.IsListening) {
            $context = $listener.GetContext();
            $response = $context.Response;
            $content='<html><body><H1>Windows Container Web Server</H1></body></html>';
            $buffer = [System.Text.Encoding]::UTF8.GetBytes($content);
            $response.ContentLength64 = $buffer.Length;
            $response.OutputStream.Write($buffer, 0, $buffer.Length);
            $response.Close();
          };
`)
		}
	} else if cfg.PVCName != "" {
		sb.WriteString(`        command:
        - pwsh.exe
        - -command
        - if (-not (Test-Path C:/html/index.html)) { echo "<html><body><H1>Windows Container Web Server</H1></body></html>" > C:/html/index.html; };$listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start();Write-Host('Listening at http://*:80/'); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $content=Get-Content C:/html/index.html -Raw; $buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; $response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); };
`)
	} else {
		sb.WriteString(`        command:
        - pwsh.exe
        - -command
        - $listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start();Write-Host('Listening at http://*:80/'); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $content='<html><body><H1>Windows Container Web Server</H1></body></html>'; $buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; $response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); };
`)
	}
	if cfg.ImagePullSecret != "" {
		sb.WriteString(fmt.Sprintf(`      imagePullSecrets:
      - name: %s
`, cfg.ImagePullSecret))
	}
	sb.WriteString(`      nodeSelector:
        kubernetes.io/os: windows
`)
	if cfg.NodeSelector != "" {
		for _, selector := range strings.Split(strings.TrimSpace(cfg.NodeSelector), "\n") {
			sb.WriteString("        " + strings.TrimSpace(selector) + "\n")
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitForDeploymentReadyWithContext(ctx, oc, deploymentName, namespace, timeout)
}

func waitForDeploymentReadyWithContext(ctx context.Context, oc *exutil.CLI, deploymentName, namespace string,
	timeout time.Duration) error {
	var lastPodStatus string
	imagePullBackOffCount := 0

	err := wait.PollUntilContextCancel(ctx, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		output, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"deployment", deploymentName,
			"-n", namespace,
			"-o", "jsonpath={.status.replicas},{.status.readyReplicas}"))
		if err != nil {
			if !isTransientTLSRecoveryError(err) {
				return false, fmt.Errorf("querying deployment readiness: %w", err)
			}
			return false, nil
		}

		parts := strings.Split(strings.TrimSpace(output), ",")
		if len(parts) != 2 {
			return false, fmt.Errorf("unexpected deployment readiness response %q", output)
		}
		if parts[0] == "" || parts[1] == "" {
			return false, nil
		}

		replicas, err := strconv.Atoi(parts[0])
		if err != nil {
			return false, fmt.Errorf("parsing deployment replicas %q: %w", parts[0], err)
		}
		readyReplicas, err := strconv.Atoi(parts[1])
		if err != nil {
			return false, fmt.Errorf("parsing deployment ready replicas %q: %w", parts[1], err)
		}

		if replicas > 0 && replicas == readyReplicas {
			return true, nil
		}

		podStatus, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args("pods",
			"-n", namespace,
			"-l", "app="+deploymentName,
			"-o", "jsonpath={.items[*].status.containerStatuses[*].state}"))
		if err != nil && !isTransientTLSRecoveryError(err) {
			return false, fmt.Errorf("querying deployment pod status: %w", err)
		}

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

		podList, _ := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args("pods",
			"-n", namespace,
			"-l", "app="+deploymentName,
			"-o", "wide"))
		e2e.Logf("Pod list:\n%s", podList)

		events, _ := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args("events",
			"-n", namespace,
			"--field-selector", "involvedObject.kind=Pod",
			"--sort-by", ".lastTimestamp"))
		e2e.Logf("Pod events:\n%s", events)

		deployStatus, _ := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("describe").Args(
			"deployment", deploymentName, "-n", namespace))
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

// getWorkloadsHostIP returns the node IPs of all pods belonging to the given Deployment.
func getWorkloadsHostIP(oc *exutil.CLI, deploymentName string, namespace string) ([]string, error) {
	hostIPs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pod", "--selector", "app="+deploymentName, "--sort-by=.status.hostIP",
		"-o=jsonpath={.items[*].status.hostIP}", "-n", namespace).Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(hostIPs), nil
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

// fetchMachineAddresses returns the addresses of the given type reported by the machines of
// machineSetName, ignoring machines that are being deleted.
func fetchMachineAddresses(oc *exutil.CLI, addressType, machineSetName string) []string {
	var raw string
	pollErr := wait.Poll(5*time.Second, 10*time.Minute, func() (bool, error) {
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"machines.machine.openshift.io", "-n", mcoNamespace,
			"-o=go-template={{range .items}}{{if and (eq (index .metadata.labels \"machine.openshift.io/cluster-api-machineset\") \""+machineSetName+"\") (not .metadata.deletionTimestamp)}}{{range .status.addresses}}{{if eq .type \""+addressType+"\"}}{{.address}} {{end}}{{end}}{{end}}{{end}}",
		).Output()
		if err != nil || strings.TrimSpace(out) == "" {
			e2e.Logf("No %s address reported yet for MachineSet %s, retrying", addressType, machineSetName)
			return false, nil
		}
		raw = out
		return true, nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr,
		fmt.Sprintf("MachineSet %s did not report a %s address within 10 minutes", machineSetName, addressType))

	addresses := []string{}
	for _, address := range strings.Fields(raw) {
		// Windows machines also report IPv6 addresses; only DNS names are kept as-is.
		if addressType == "InternalDNS" || !strings.Contains(address, ":") {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

// generateAWSWindowsMachineSetYAML returns a Windows MachineSet that provisions one machine from
// the given AMI. It mirrors the AWS Windows MachineSet created by hack/machineset.sh so the
// secondary OS variant joins the cluster the same way as the primary one.
func generateAWSWindowsMachineSetYAML(name, infrastructureID, zone, region, subnetID, imageID, instanceType string) string {
	if strings.TrimSpace(instanceType) == "" {
		instanceType = "m5a.large"
	}
	return fmt.Sprintf(`apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: %[2]s
  name: %[1]s
  namespace: %[7]s
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: %[2]s
      machine.openshift.io/cluster-api-machineset: %[1]s
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: %[2]s
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: %[1]s
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
          type: "winc"
      providerSpec:
        value:
          ami:
            id: %[6]s
          apiVersion: awsproviderconfig.openshift.io/v1beta1
          blockDevices:
          - ebs:
              iops: 0
              volumeSize: 120
              volumeType: gp2
          credentialsSecret:
            name: aws-cloud-credentials
          deviceIndex: 0
          iamInstanceProfile:
            id: %[2]s-worker-profile
          instanceType: %[8]s
          kind: AWSMachineProviderConfig
          placement:
            availabilityZone: %[3]s
            region: %[4]s
          securityGroups:
          - filters:
            - name: tag:Name
              values:
              - %[2]s-node
          - filters:
            - name: tag:Name
              values:
              - %[2]s-lb
          subnet:
            filters:
            - name: tag:Name
              values:
              - %[2]s-subnet-private-%[3]s
            id: %[5]s
          tags:
          - name: kubernetes.io/cluster/%[2]s
            value: owned
          userDataSecret:
            name: windows-user-data
            namespace: %[7]s
`, name, infrastructureID, zone, region, subnetID, imageID, mcoNamespace, instanceType)
}

// generateAzureWindowsMachineSetYAML returns a Windows MachineSet that provisions one machine from
// the given Azure image SKU.
func generateAzureWindowsMachineSetYAML(name, infrastructureID, location, sku string) string {
	return fmt.Sprintf(`apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
  labels:
    machine.openshift.io/cluster-api-cluster: %[2]s
  name: %[1]s
  namespace: %[5]s
spec:
  replicas: 1
  selector:
    matchLabels:
      machine.openshift.io/cluster-api-cluster: %[2]s
      machine.openshift.io/cluster-api-machineset: %[1]s
  template:
    metadata:
      labels:
        machine.openshift.io/cluster-api-cluster: %[2]s
        machine.openshift.io/cluster-api-machine-role: worker
        machine.openshift.io/cluster-api-machine-type: worker
        machine.openshift.io/cluster-api-machineset: %[1]s
        machine.openshift.io/os-id: Windows
    spec:
      metadata:
        labels:
          node-role.kubernetes.io/worker: ""
          type: "winc"
      providerSpec:
        value:
          apiVersion: azureproviderconfig.openshift.io/v1beta1
          credentialsSecret:
            name: azure-cloud-credentials
            namespace: %[5]s
          image:
            offer: WindowsServer
            publisher: MicrosoftWindowsServer
            resourceID: ""
            sku: %[4]s
            version: latest
          kind: AzureMachineProviderSpec
          location: %[3]s
          managedIdentity: %[2]s-identity
          networkResourceGroup: %[2]s-rg
          osDisk:
            diskSizeGB: 128
            managedDisk:
              storageAccountType: Premium_LRS
            osType: Windows
          publicIP: false
          resourceGroup: %[2]s-rg
          subnet: %[2]s-worker-subnet
          userDataSecret:
            name: windows-user-data
            namespace: %[5]s
          vmSize: Standard_D2s_v3
          vnet: %[2]s-vnet
          zone: ""
`, name, infrastructureID, location, sku, mcoNamespace)
}

// awsVersionForOtherBuild returns the supported Windows Server version that differs from the
// node's build. Only 2022 and 2025 are supported.
func awsVersionForOtherBuild(currentBuild string) (string, bool) {
	switch strings.TrimSpace(currentBuild) {
	case "10.0.20348":
		return "2025", true
	case "10.0.26100":
		return "2022", true
	}
	return "", false
}

// awsCredentialsProvider mirrors the credential source of the aws-windows-ami-discover step: the
// e2e pod carries the cluster profile file, and the cluster's machine credentials are the fallback
// when the profile is not mounted.
func awsCredentialsProvider(oc *exutil.CLI) (credentials.Provider, bool) {
	if path := os.Getenv("AWS_SHARED_CREDENTIALS_FILE"); path != "" {
		return &credentials.SharedCredentialsProvider{Filename: path, Profile: "default"}, true
	}
	if dir := os.Getenv("CLUSTER_PROFILE_DIR"); dir != "" {
		path := filepath.Join(dir, ".awscred")
		if _, err := os.Stat(path); err == nil {
			return &credentials.SharedCredentialsProvider{Filename: path, Profile: "default"}, true
		}
	}
	accessKey, secretKey, ok := awsCredentialsFromSecret(oc)
	if !ok {
		return nil, false
	}
	return &credentials.StaticProvider{Value: credentials.Value{
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}}, true
}

// awsCredentialsFromSecret reads aws_access_key_id and aws_secret_access_key from the cluster's
// machine credentials secret.
func awsCredentialsFromSecret(oc *exutil.CLI) (string, string, bool) {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"secret", "aws-cloud-credentials", "-n", mcoNamespace, "-o", "json").Output()
	if err != nil {
		e2e.Logf("Could not read aws-cloud-credentials: %v", err)
		return "", "", false
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		e2e.Logf("Could not parse aws-cloud-credentials: %v", err)
		return "", "", false
	}
	decode := func(key string) string {
		encoded, ok := secret.Data[key]
		if !ok {
			return ""
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return ""
		}
		return string(decoded)
	}
	accessKey := decode("aws_access_key_id")
	secretKey := decode("aws_secret_access_key")
	if accessKey == "" || secretKey == "" {
		return "", "", false
	}
	return accessKey, secretKey, true
}

// awsPrimaryMachineSetInstanceType returns the instance type used by the cluster's Windows
// MachineSet. The secondary OS variant has to boot on the same instance type that is known to work
// in this account and region; an empty result makes the generator fall back to m5a.large.
func awsPrimaryMachineSetInstanceType(oc *exutil.CLI) string {
	for _, selector := range []string{"-l=" + machineLabel, ""} {
		args := []string{"machinesets.machine.openshift.io", "-n", mcoNamespace}
		if selector != "" {
			args = append(args, selector)
		}
		args = append(args, "-o=jsonpath={.items[0].spec.template.spec.providerSpec.value.instanceType}")
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	return ""
}

// awsPrimaryMachineSetAMI returns the AMI id of the cluster's Windows MachineSet.
func awsPrimaryMachineSetAMI(oc *exutil.CLI) string {
	for _, selector := range []string{"-l=" + machineLabel, ""} {
		args := []string{"machinesets.machine.openshift.io", "-n", mcoNamespace}
		if selector != "" {
			args = append(args, selector)
		}
		args = append(args, "-o=jsonpath={.items[0].spec.template.spec.providerSpec.value.ami.id}")
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(args...).Output()
		if err == nil && strings.TrimSpace(out) != "" {
			return strings.TrimSpace(out)
		}
	}
	return ""
}

// awsDeriveSecondaryImageFilter builds the name filter for the other supported Windows Server
// version from the primary AMI's name, keeping the same variant and date scheme. hardcoding
// "-English-Core-Base-" can pick an image whose variant differs from the one the cluster actually
// booted, and such an image may not run the WMCO user data.
//
// Windows_Server-2022-English-Core-Base-2025.01.15 + 2025
//
//	-> Windows_Server-2025-English-Core-Base-*
func awsDeriveSecondaryImageFilter(primaryName, targetVersion string) (string, bool) {
	name := strings.TrimSpace(primaryName)
	if targetVersion == "" || !strings.HasPrefix(name, "Windows_Server-") {
		return "", false
	}
	rest := strings.TrimPrefix(name, "Windows_Server-")
	if len(rest) < 5 || !strings.HasPrefix(rest[:4], "20") || rest[4] != '-' {
		return "", false
	}
	currentVersion := rest[:4]
	if currentVersion == targetVersion {
		return "", false
	}
	rest = rest[5:]
	// Drop the trailing release date (YYYY.MM.DD) and wildcard it.
	if idx := strings.LastIndex(rest, "-"); idx != -1 {
		rest = rest[:idx+1]
	}
	return "Windows_Server-" + targetVersion + "-" + rest + "*", true
}

// awsImageName returns the name of an AMI.
func awsImageName(client *ec2.EC2, imageID string) (string, bool) {
	out, err := client.DescribeImages(&ec2.DescribeImagesInput{ImageIds: []*string{aws.String(imageID)}})
	if err != nil || len(out.Images) == 0 || out.Images[0].Name == nil {
		return "", false
	}
	return *out.Images[0].Name, true
}

// awsSecondaryWindowsImage resolves the latest Amazon AMI of the other supported Windows Server
// version in the cluster's region, the same way aws-windows-ami-discover does for BYOH hosts.
func awsSecondaryWindowsImage(oc *exutil.CLI, platform string) (string, bool) {
	if platform != "aws" {
		return "", false
	}
	region, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.aws.region}").Output()
	if err != nil || strings.TrimSpace(region) == "" {
		e2e.Logf("Could not determine the AWS region: %v", err)
		return "", false
	}
	region = strings.TrimSpace(region)

	targetVersion := ""
	for _, node := range getWindowsHostNames(oc) {
		build, err := getWindowsBuildID(oc, node)
		if err != nil {
			continue
		}
		if version, ok := awsVersionForOtherBuild(build); ok {
			targetVersion = version
			break
		}
	}
	if targetVersion == "" {
		return "", false
	}

	provider, ok := awsCredentialsProvider(oc)
	if !ok {
		e2e.Logf("No AWS credentials available to resolve a secondary Windows AMI")
		return "", false
	}
	sess, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewCredentials(provider),
		Region:      aws.String(region),
	})
	if err != nil {
		e2e.Logf("Could not create an AWS session: %v", err)
		return "", false
	}

	client := ec2.New(sess)
	nameFilter := fmt.Sprintf("Windows_Server-%s-English-Core-Base-????.??.??", targetVersion)
	if primaryAMI := awsPrimaryMachineSetAMI(oc); primaryAMI != "" {
		if primaryName, ok := awsImageName(client, primaryAMI); ok {
			if derived, ok := awsDeriveSecondaryImageFilter(primaryName, targetVersion); ok {
				e2e.Logf("Deriving the secondary AMI filter from the primary AMI %s: %s", primaryName, derived)
				nameFilter = derived
			}
		}
	}
	out, err := client.DescribeImages(&ec2.DescribeImagesInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("name"),
			Values: []*string{aws.String(nameFilter)},
		}},
		Owners: []*string{aws.String("amazon")},
	})
	if err != nil {
		e2e.Logf("Could not describe Windows %s AMIs in %s: %v", targetVersion, region, err)
		return "", false
	}
	if len(out.Images) == 0 {
		e2e.Logf("No Amazon AMI matched %s in %s", nameFilter, region)
		return "", false
	}
	latest := out.Images[0]
	for _, image := range out.Images[1:] {
		if image.CreationDate != nil && latest.CreationDate != nil && *image.CreationDate > *latest.CreationDate {
			latest = image
		}
	}
	if latest.ImageId == nil || *latest.ImageId == "" {
		return "", false
	}
	e2e.Logf("Resolved secondary Windows AMI %s (%s) for %s", *latest.ImageId, targetVersion, region)
	return *latest.ImageId, true
}

// createWindowsMachineSetWithImage creates a Windows MachineSet that provisions one machine from
// imageID and returns its name. Only AWS and Azure offer a second Windows OS variant, where the
// image is an AMI ID and an Azure image SKU respectively. The caller deletes the MachineSet.
func createWindowsMachineSetWithImage(oc *exutil.CLI, platform, name, imageID string) string {
	infrastructureID, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"infrastructure", "cluster", "-o=jsonpath={.status.infrastructureName}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	infrastructureID = strings.TrimSpace(infrastructureID)

	var manifest, machineSetName string
	switch platform {
	case "aws":
		region, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.aws.region}").Output()
		if err != nil || strings.TrimSpace(region) == "" {
			region, err = oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"machines.machine.openshift.io", "-n", mcoNamespace, "-l", machineLabel,
				"-o=jsonpath={.items[0].spec.providerSpec.value.placement.region}").Output()
			o.Expect(err).NotTo(o.HaveOccurred(), "Could not fetch the AWS region from the existing MachineSet")
		}
		zone := getAvailabilityZone(oc)
		subnetID, subnetErr := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"machinesets.machine.openshift.io", "-n", mcoNamespace,
			"-o=jsonpath={.items[0].spec.template.spec.providerSpec.value.subnet.id}").Output()
		// An empty subnet ID is expected when the MachineSet selects its subnet by tag; the generated
		// manifest keeps that filter. A failed read is different and must not turn into a manifest
		// that silently provisions into the default subnet.
		o.Expect(subnetErr).NotTo(o.HaveOccurred(), "Could not fetch the AWS subnet from the existing MachineSet")
		machineSetName = getWindowsMachineSetName(oc, name, platform, zone)
		// Reuse the primary MachineSet's instance type: the secondary AMI has to boot on the
		// same instance type that is known to work in this account and region.
		instanceType := awsPrimaryMachineSetInstanceType(oc)
		manifest = generateAWSWindowsMachineSetYAML(machineSetName, infrastructureID, zone,
			strings.TrimSpace(region), strings.TrimSpace(subnetID), imageID, instanceType)
	case "azure":
		location, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"nodes", "-o=jsonpath={.items[0].metadata.labels.topology\\.kubernetes\\.io/region}").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Could not determine the Azure region from the cluster nodes")
		o.Expect(strings.TrimSpace(location)).NotTo(o.BeEmpty(), "Azure region label is empty on the cluster nodes")
		machineSetName = name
		manifest = generateAzureWindowsMachineSetYAML(machineSetName, infrastructureID,
			strings.TrimSpace(location), imageID)
	default:
		e2e.Failf("Platform %s does not support multiple Windows OS variants", platform)
	}

	o.Expect(createResourceFromString(oc, mcoNamespace, manifest)).NotTo(o.HaveOccurred(),
		"Failed to create Windows MachineSet %s", machineSetName)
	return machineSetName
}

// azureSecondaryWindowsImage returns the Azure image SKU of a Windows Server version other than
// the one already running in the cluster, so a multi-OS test needs no CI-provided image input.
// AWS AMIs are resolved by name against the account, so AWS keeps supplying the image through
// winc-test-config.
func azureSecondaryWindowsImage(oc *exutil.CLI, platform string) (string, bool) {
	if platform != "azure" {
		return "", false
	}
	for _, node := range getWindowsHostNames(oc) {
		build, err := getWindowsBuildID(oc, node)
		if err != nil {
			continue
		}
		if sku, ok := azureSKUForOtherBuild(build); ok {
			return sku, true
		}
	}
	return "", false
}

// azureSKUForOtherBuild returns the Azure image SKU of the supported Windows Server version whose
// build differs from currentBuild.
func azureSKUForOtherBuild(currentBuild string) (string, bool) {
	switch strings.TrimSpace(currentBuild) {
	case "10.0.20348":
		return "2025-datacenter-smalldisk", true
	case "10.0.26100":
		return "2022-datacenter-smalldisk", true
	}
	return "", false
}

// getMachineSetReplicas returns the desired replica count of the given MachineSet. Tests that scale
// a MachineSet must capture this before scaling so cleanup restores the cluster's original size
// instead of assuming a fixed count.
func getMachineSetReplicas(oc *exutil.CLI, machineSetName string) (int, error) {
	replicas, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machinesets.machine.openshift.io", machineSetName, "-n", mcoNamespace,
		"-o=jsonpath={.spec.replicas}").Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get replicas of MachineSet %s: %w", machineSetName, err)
	}

	count, err := strconv.Atoi(strings.TrimSpace(replicas))
	if err != nil {
		return 0, fmt.Errorf("unexpected replica count %q for MachineSet %s: %w", replicas, machineSetName, err)
	}
	return count, nil
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

// cloneMachineSetName returns the name for a disposable clone of the Windows MachineSet.
//
// The Machine name is "<machineset name>-<five random characters>", and Azure derives the VM's
// Windows computer name from the Machine name, rejecting anything longer than 15 characters.
// Cloning e2e-wm as "e2e-wm-clone" therefore produces e2e-wm-clone-xxxxx (18 characters) and the
// VM is never created: the Machine fails with InvalidParameter and no node ever joins.
// "e2e-c<suffix>" keeps the Machine name at 12 characters on every platform.
func cloneMachineSetName(suffix string) string {
	return "e2e-c" + suffix
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

// restoreAPIServerTLS restores the original TLS configuration on apiserver/cluster.
func restoreAPIServerTLS(ctx context.Context, oc *exutil.CLI, origAdherence, origTLSProfile string) error {
	var restoreErr error
	if origAdherence == "" {
		_, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"apiserver/cluster", "--type=merge", "-p", `{"spec":{"tlsAdherence":null}}`))
		if err != nil {
			restoreErr = fmt.Errorf("could not remove tlsAdherence: %w", err)
		}
	} else {
		_, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"apiserver/cluster", "--type=merge",
			"-p", fmt.Sprintf(`{"spec":{"tlsAdherence":"%s"}}`, origAdherence)))
		if err != nil {
			restoreErr = fmt.Errorf("could not restore tlsAdherence: %w", err)
		}
	}
	if origTLSProfile == "" {
		// The API server may default this field and reject removing it. An
		// explicit Intermediate profile is equivalent to the default and is
		// safely accepted as the restored state.
		_, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"apiserver/cluster", "--type=merge",
			"-p", `{"spec":{"tlsSecurityProfile":{"type":"Intermediate","intermediate":{}}}}`))
		if err != nil {
			if restoreErr != nil {
				return joinTLSRestorationErrors(restoreErr, fmt.Errorf("could not restore default tlsSecurityProfile: %w", err))
			}
			return fmt.Errorf("could not restore default tlsSecurityProfile: %w", err)
		}
	} else {
		_, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"apiserver/cluster", "--type=merge",
			"-p", fmt.Sprintf(`{"spec":{"tlsSecurityProfile":%s}}`, origTLSProfile)))
		if err != nil {
			if restoreErr != nil {
				return joinTLSRestorationErrors(restoreErr, fmt.Errorf("could not restore tlsSecurityProfile: %w", err))
			}
			return fmt.Errorf("could not restore tlsSecurityProfile: %w", err)
		}
	}
	return restoreErr
}

type clusterOperatorList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

// clusterOperatorsSettled checks that the named operators are available and
// neither progressing nor degraded.
func clusterOperatorsSettled(operatorJSON string, operatorNames []string) (bool, string, error) {
	var operators clusterOperatorList
	if err := json.Unmarshal([]byte(operatorJSON), &operators); err != nil {
		return false, "", fmt.Errorf("parsing cluster operator status: %w", err)
	}

	operatorConditions := make(map[string]map[string]string, len(operators.Items))
	for _, operator := range operators.Items {
		conditions := make(map[string]string, len(operator.Status.Conditions))
		for _, condition := range operator.Status.Conditions {
			conditions[condition.Type] = condition.Status
		}
		operatorConditions[operator.Metadata.Name] = conditions
	}

	expectedConditions := map[string]string{
		"Available":   "True",
		"Progressing": "False",
		"Degraded":    "False",
	}
	for _, operatorName := range operatorNames {
		conditions, found := operatorConditions[operatorName]
		if !found {
			return false, fmt.Sprintf("cluster operator %s was not returned", operatorName), nil
		}
		for conditionType, expectedStatus := range expectedConditions {
			actualStatus, found := conditions[conditionType]
			if !found {
				return false, fmt.Sprintf("cluster operator %s has no %s condition", operatorName, conditionType), nil
			}
			if actualStatus != expectedStatus {
				return false, fmt.Sprintf("cluster operator %s has %s=%s, expected %s",
					operatorName, conditionType, actualStatus, expectedStatus), nil
			}
		}
	}

	return true, "", nil
}

type tlsRecoveryCheck func() (bool, string, error)
type tlsRecoveryContextCheck func(context.Context) (bool, string, error)

func isTransientTLSRecoveryError(err error) bool {
	message := strings.ToLower(err.Error())
	if cliAPIRefusalPattern.MatchString(message) {
		return true
	}
	for _, transientMessage := range []string{
		"connection refused",
		"connection reset by peer",
		"context deadline exceeded",
		"gateway timeout",
		"i/o timeout",
		"server is currently unable to handle the request",
		"service unavailable",
		"temporarily unavailable",
		"tls handshake timeout",
		"timeout awaiting response headers",
		"client.timeout exceeded while awaiting headers",
	} {
		if strings.Contains(message, transientMessage) {
			return true
		}
	}
	return false
}

func pollForTLSRecovery(interval, timeout, minimumStablePeriod time.Duration, check tlsRecoveryCheck) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return pollForTLSRecoveryWithContext(ctx, interval, timeout, minimumStablePeriod,
		func(context.Context) (bool, string, error) {
			return check()
		})
}

// waitForOpenShiftAPIAndOperators verifies recovery after changing the API
// server TLS profile. This mitigates test cleanup races; it does not identify
// the cause of any API outage observed during the rollout.
func waitForOpenShiftAPIAndOperators(oc *exutil.CLI, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitForOpenShiftAPIAndOperatorsWithContext(ctx, oc, timeout)
}

func waitForOpenShiftAPIAndOperatorsWithContext(ctx context.Context, oc *exutil.CLI, timeout time.Duration) error {
	return pollForTLSRecoveryWithContext(ctx, tlsRecoveryPollInterval, timeout, tlsRecoveryMinimumStablePeriod,
		func(ctx context.Context) (bool, string, error) {
			userJSON, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"--raw=/apis/user.openshift.io/v1/users/~", "--request-timeout=10s"))
			if err != nil {
				e2e.Logf("Waiting for the OpenShift user API to recover: %v", err)
				return false, "", fmt.Errorf("querying the OpenShift user API: %w", err)
			}
			var user struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal([]byte(userJSON), &user); err != nil {
				return false, "", fmt.Errorf("parsing the OpenShift user API response: %w", err)
			}
			if user.Metadata.Name == "" {
				return false, "", fmt.Errorf("OpenShift user API response has no metadata.name")
			}

			// The suite's BeforeEach reads this exact resource and field, so a
			// successful sample must prove that path is healthy as well.
			infrastructureJSON, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"infrastructure", "cluster", "-o=json", "--request-timeout=10s"))
			if err != nil {
				e2e.Logf("Waiting for the infrastructure API read used by BeforeEach: %v", err)
				return false, "", fmt.Errorf("querying infrastructure/cluster: %w", err)
			}
			var infrastructure struct {
				Status struct {
					PlatformStatus struct {
						Type string `json:"type"`
					} `json:"platformStatus"`
				} `json:"status"`
			}
			if err := json.Unmarshal([]byte(infrastructureJSON), &infrastructure); err != nil {
				return false, "", fmt.Errorf("parsing infrastructure/cluster: %w", err)
			}
			if infrastructure.Status.PlatformStatus.Type == "" {
				return false, "", fmt.Errorf("infrastructure/cluster has no status.platformStatus.type")
			}

			args := append([]string{"clusteroperator"}, tlsRecoveryClusterOperators...)
			args = append(args, "-o=json", "--request-timeout=10s")
			operatorJSON, err := cliOutputWithContext(ctx,
				oc.AsAdmin().WithoutNamespace().Run("get").Args(args...))
			if err != nil {
				e2e.Logf("Waiting for control-plane cluster operators: %v", err)
				return false, "", fmt.Errorf("querying control-plane cluster operators: %w", err)
			}
			settled, state, err := clusterOperatorsSettled(operatorJSON, tlsRecoveryClusterOperators)
			if err != nil {
				return false, "", err
			}
			if !settled {
				e2e.Logf("Waiting for control-plane cluster operators: %s", state)
			}
			return settled, state, nil
		})
}

func getAPIServerTLSConfiguration(oc *exutil.CLI) (string, string, error) {
	return getAPIServerTLSConfigurationWithContext(context.Background(), oc)
}

func getAPIServerTLSConfigurationWithContext(ctx context.Context, oc *exutil.CLI) (string, string, error) {
	profile, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"apiserver/cluster", "-o=jsonpath={.spec.tlsSecurityProfile}"))
	if err != nil {
		return "", "", fmt.Errorf("failed to get apiserver TLS profile: %w", err)
	}
	adherence, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"apiserver/cluster", "-o=jsonpath={.spec.tlsAdherence}"))
	if err != nil {
		return "", "", fmt.Errorf("failed to get apiserver TLS adherence: %w", err)
	}
	return profile, adherence, nil
}

func tlsProfileType(profile string) string {
	if profile == "" {
		return "Intermediate"
	}
	return gjson.Parse(profile).Get("type").String()
}

// ensureTLSProfileDiffersFromTarget changes a target profile to Intermediate
// first when necessary, ensuring the next target patch triggers the watcher.
func ensureTLSProfileDiffersFromTarget(oc *exutil.CLI, targetType string) error {
	profile, _, err := getAPIServerTLSConfiguration(oc)
	if err != nil {
		return err
	}
	if tlsProfileType(profile) != targetType {
		return nil
	}

	baseline := getWMCORestartState(oc)
	if baseline == "" {
		return fmt.Errorf("could not capture WMCO restart state before setting alternate Intermediate profile")
	}
	if err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("apiserver/cluster", "--type=merge",
		"-p", `{"spec":{"tlsSecurityProfile":{"type":"Intermediate","intermediate":{}}}}`).Execute(); err != nil {
		return fmt.Errorf("failed to set alternate Intermediate TLS profile: %w", err)
	}
	restarted, err := checkWMCORestarted(oc, baseline)
	if err != nil {
		return err
	}
	if !restarted {
		return fmt.Errorf("WMCO did not restart after setting alternate Intermediate TLS profile")
	}
	return waitForDeploymentReady(oc, wmcoDeploymentName, wmcoNamespace, 5*time.Minute)
}

type tlsRecoveryWaitOperations struct {
	waitForRestart      func(context.Context, time.Duration) (bool, error)
	waitForDeployment   func(context.Context, time.Duration) error
	waitForWindowsNodes func(context.Context, time.Duration) error
	waitForStability    func(context.Context, time.Duration) error
}

func tlsRecoveryPhaseTimeout(deadline time.Time, maximum time.Duration) (time.Duration, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, fmt.Errorf("TLS restoration exceeded its %v shared deadline", tlsRestorationTimeout)
	}
	if remaining < maximum {
		return remaining, nil
	}
	return maximum, nil
}

func tlsRecoveryPhaseContext(parent context.Context, deadline time.Time,
	maximum time.Duration) (context.Context, context.CancelFunc, time.Duration, error) {
	timeout, err := tlsRecoveryPhaseTimeout(deadline, maximum)
	if err != nil {
		return nil, nil, 0, err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, timeout, nil
}

func waitForTLSRecoveryAfterRestore(ctx context.Context, deadline time.Time, windowsNodeCount int,
	operations tlsRecoveryWaitOperations) error {
	phaseDeadline := deadline.Add(-tlsRecoveryAPITimeout)
	var phaseErr error
	if ctx.Err() == nil && time.Until(phaseDeadline) > 0 {
		restartCtx, cancelRestart, restartTimeout, err := tlsRecoveryPhaseContext(ctx, phaseDeadline,
			tlsRecoveryRestartTimeout)
		if err == nil {
			var restarted bool
			restarted, err = operations.waitForRestart(restartCtx, restartTimeout)
			if err == nil && !restarted {
				err = fmt.Errorf("WMCO did not restart while restoring TLS configuration")
			}
		}
		if cancelRestart != nil {
			cancelRestart()
		}
		if err != nil {
			phaseErr = fmt.Errorf("restart recovery: %w", err)
			e2e.Logf("TLS restoration restart recovery failed: %v", phaseErr)
		}
		if phaseErr == nil {
			deploymentCtx, cancelDeployment, deploymentTimeout, err := tlsRecoveryPhaseContext(ctx, phaseDeadline,
				tlsRecoveryDeploymentTimeout)
			if err == nil {
				err = operations.waitForDeployment(deploymentCtx, deploymentTimeout)
				cancelDeployment()
			}
			if err != nil {
				phaseErr = fmt.Errorf("deployment recovery: %w", err)
				e2e.Logf("TLS restoration deployment recovery failed: %v", phaseErr)
			}
		}
		if phaseErr == nil && windowsNodeCount > 0 {
			nodesCtx, cancelNodes, nodesTimeout, err := tlsRecoveryPhaseContext(ctx, phaseDeadline,
				tlsRecoveryWindowsNodesTimeout)
			if err == nil {
				err = operations.waitForWindowsNodes(nodesCtx, nodesTimeout)
				cancelNodes()
			}
			if err != nil {
				phaseErr = fmt.Errorf("Windows node recovery: %w", err)
				e2e.Logf("TLS restoration Windows node recovery failed: %v", phaseErr)
			}
		}
	} else {
		phaseErr = fmt.Errorf("TLS restoration has no time for restart recovery (deadline %s): %v", phaseDeadline, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return joinTLSRestorationErrors(phaseErr, err)
	}
	stabilityCtx, cancelStability, stabilityTimeout, err := tlsRecoveryPhaseContext(ctx, deadline, tlsRecoveryControlPlaneTimeout)
	if err != nil {
		return joinTLSRestorationErrors(phaseErr, err)
	}
	defer cancelStability()
	stabilityErr := operations.waitForStability(stabilityCtx, stabilityTimeout)
	if stabilityErr != nil {
		e2e.Logf("TLS restoration final control-plane recovery failed: %v", stabilityErr)
	}
	return joinTLSRestorationErrors(phaseErr, stabilityErr)
}

func restoreAPIServerTLSAndWait(oc *exutil.CLI, origAdherence, origTLSProfile string, windowsNodeCount int) error {
	deadline := time.Now().Add(tlsRestorationTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	baseline, changed, err := restoreTLSConfigurationWithContext(ctx, deadline, origAdherence, origTLSProfile,
		func(ctx context.Context) (string, string, error) {
			return getAPIServerTLSConfigurationWithContext(ctx, oc)
		},
		func(ctx context.Context) (string, error) { return getWMCORestartStateWithContext(ctx, oc) },
		func(ctx context.Context) error { return restoreAPIServerTLS(ctx, oc, origAdherence, origTLSProfile) })
	if !changed && err == nil {
		return nil
	}
	recoveryErr := waitForTLSRecoveryAfterRestore(ctx, deadline, windowsNodeCount, tlsRecoveryWaitOperations{
		waitForRestart: func(ctx context.Context, timeout time.Duration) (bool, error) {
			if baseline == "" {
				return false, fmt.Errorf("WMCO restart baseline unavailable after TLS restoration attempt")
			}
			return checkWMCORestartedWithinContext(ctx, oc, baseline, timeout)
		},
		waitForDeployment: func(ctx context.Context, timeout time.Duration) error {
			return waitForDeploymentReadyWithContext(ctx, oc, wmcoDeploymentName, wmcoNamespace, timeout)
		},
		waitForWindowsNodes: func(ctx context.Context, _ time.Duration) error {
			return pollWindowsNodesReadyWithContext(ctx, oc, windowsNodeCount)
		},
		waitForStability: func(ctx context.Context, timeout time.Duration) error {
			return waitForOpenShiftAPIAndOperatorsWithContext(ctx, oc, timeout)
		},
	})
	if recoveryErr != nil {
		e2e.Logf("TLS restoration recovery phase failed: %v", recoveryErr)
	}
	return joinTLSRestorationErrors(err, recoveryErr)
}

func restoreTLSConfigurationWithContext(ctx context.Context, deadline time.Time, origAdherence, origTLSProfile string,
	read func(context.Context) (string, string, error), baselineRead func(context.Context) (string, error),
	restore func(context.Context) error) (string, bool, error) {
	readDeadline := deadline.Add(-tlsRecoveryRestartTimeout - tlsRecoveryDeploymentTimeout -
		tlsRecoveryWindowsNodesTimeout - tlsRecoveryAPITimeout)
	if time.Until(readDeadline) > time.Minute {
		readDeadline = time.Now().Add(time.Minute)
	}
	var baseline string
	readCtx, cancelRead := context.WithDeadline(ctx, readDeadline)
	defer cancelRead()
	changed := true
	var readErr error
	for readCtx.Err() == nil {
		profile, adherence, err := read(readCtx)
		if err == nil {
			changed = profile != origTLSProfile || adherence != origAdherence
			readErr = nil
			break
		}
		readErr = fmt.Errorf("reading TLS configuration before restoration: %w", err)
		e2e.Logf("TLS restoration configuration read failed: %v", readErr)
		if !isTransientTLSRecoveryError(err) {
			break
		}
		select {
		case <-readCtx.Done():
		case <-time.After(time.Second):
		}
	}
	if !changed && readErr == nil {
		return "", false, nil
	}
	var baselineErr error
	for readCtx.Err() == nil {
		state, err := baselineRead(readCtx)
		if err == nil && state != "" {
			baseline = state
			baselineErr = nil
			break
		}
		if err != nil {
			baselineErr = fmt.Errorf("reading WMCO restart baseline: %w", err)
		}
		if err != nil && !isTransientTLSRecoveryError(err) {
			e2e.Logf("TLS restoration restart baseline failed: %v", err)
			break
		}
		select {
		case <-readCtx.Done():
		case <-time.After(time.Second):
		}
	}
	var restoreErr error
	restored := false
	patchCtx, cancelPatch := context.WithDeadline(ctx, deadline.Add(-tlsRecoveryAPITimeout))
	defer cancelPatch()
	for patchCtx.Err() == nil {
		restoreErr = restore(patchCtx)
		if restoreErr == nil {
			restored = true
			break
		}
		if !isRetryableTLSRestorationError(restoreErr) {
			break
		}
		e2e.Logf("TLS restoration patch temporarily unavailable: %v", restoreErr)
		select {
		case <-patchCtx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	if !restored && restoreErr == nil {
		restoreErr = fmt.Errorf("TLS restoration patch deadline expired before it could start: %v", patchCtx.Err())
	}
	if restoreErr != nil || ctx.Err() != nil {
		return baseline, true, joinTLSRestorationErrors(readErr, baselineErr, restoreErr, ctx.Err())
	}
	if baseline == "" {
		return "", true, joinTLSRestorationErrors(readErr, baselineErr,
			fmt.Errorf("could not capture WMCO restart state before restoring TLS configuration"))
	}
	return baseline, true, readErr
}

func isRetryableTLSRestorationError(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, fieldErr := range joined.Unwrap() {
			if !isTransientTLSRecoveryError(fieldErr) {
				return false
			}
		}
		return true
	}
	return isTransientTLSRecoveryError(err)
}

// createTLSCheckerPod creates a temporary Linux pod for running openssl commands
// and waits for it to reach Running state. Uses the cluster-local tools image
// from the OpenShift payload to support disconnected environments.
func createTLSCheckerPod(oc *exutil.CLI) string {
	// Pod names must comply with RFC 1123. Use a lowercase hexadecimal suffix
	// so the generated name contains only valid characters.
	podName := "tls-checker-" + getRandomDNSLabel(5)

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
func runTLSCheck(ctx context.Context, oc *exutil.CLI, checkerPod, host, port, tlsFlag string) (string, error) {
	tlsArg := ""
	if tlsFlag != "" {
		tlsArg = " " + tlsFlag
	}
	cmd := fmt.Sprintf("echo | openssl s_client -connect %s%s 2>&1 || true", net.JoinHostPort(host, port), tlsArg)
	return cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("exec").Args(
		"-n", wmcoNamespace, "pod/"+checkerPod, "--", "bash", "-c", cmd))
}

func readyWMCOTLSEndpoint(ctx context.Context, oc *exutil.CLI, checkerPod string) (string, error) {
	readyCtx, cancel := context.WithTimeout(ctx, tlsEndpointReadyTimeout)
	defer cancel()
	return waitForWMCOTLSEndpoint(readyCtx, 5*time.Second, func(ctx context.Context) (string, error) {
		output, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"pod", "-n", wmcoNamespace, "--selector", "name="+wmcoDeploymentName, "-o=json"))
		if err != nil {
			return "", err
		}
		return readyWMCOPodIP(output)
	}, func(ctx context.Context, address string) (string, error) {
		return runTLSCheck(ctx, oc, checkerPod, address, "9182", "-tls1_2")
	})
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

// generateTestPrivateKey generates an RSA private key and writes it to a temporary PEM file.
// Returns the file path. Caller is responsible for cleanup.
func generateTestPrivateKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", fmt.Errorf("generate RSA private key: %w", err)
	}

	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	tmpFile, err := os.CreateTemp("", "winc-39640-key-*.pem")
	if err != nil {
		return "", fmt.Errorf("create temporary private key file: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
		}
	}()

	if err := tmpFile.Chmod(0600); err != nil {
		return "", fmt.Errorf("set private key file permissions: %w", err)
	}
	if n, err := tmpFile.Write(privateKeyPEM); err != nil {
		return "", fmt.Errorf("write private key file: %w", err)
	} else if n != len(privateKeyPEM) {
		return "", fmt.Errorf("write private key file: wrote %d of %d bytes", n, len(privateKeyPEM))
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close private key file: %w", err)
	}

	cleanup = false
	return tmpFile.Name(), nil
}

func safePollErrorReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	default:
		return "API request failed"
	}
}

// publicKeyHashFromPrivateKey returns the value WMCO places in the public key hash node annotation.
func publicKeyHashFromPrivateKey(privateKeyPath string) (string, error) {
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	authorizedKey := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(authorizedKey))), nil
}

// getNodeNamesFromMachineSet is an error-aware form of Origin's MachineSet helper for use in a poll.
func getNodeNamesFromMachineSet(ctx context.Context, oc *exutil.CLI, machineSetName string) ([]string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	machines, err := oc.AdminDynamicClient().Resource(machinesGVR).Namespace(mcoNamespace).List(requestCtx,
		metav1.ListOptions{LabelSelector: "machine.openshift.io/cluster-api-machineset=" + machineSetName})
	if err != nil {
		return nil, fmt.Errorf("list MachineSet machines: %w", err)
	}

	nodeNames := make([]string, 0, len(machines.Items))
	for _, machine := range machines.Items {
		nodeName, found, err := unstructured.NestedString(machine.Object, "status", "nodeRef", "name")
		if err != nil {
			return nil, fmt.Errorf("read Machine node reference: %w", err)
		}
		if found && nodeName != "" {
			nodeNames = append(nodeNames, nodeName)
		}
	}
	return nodeNames, nil
}

// targetNodesReadyWithKeyHash checks whether the given nodes are Ready and use the replacement key.
func targetNodesReadyWithKeyHash(ctx context.Context, nodeClient corev1client.NodeInterface, nodeNames []string,
	expectedNodeCount int, expectedHash string) (bool, error) {
	targetNodes := make(map[string]struct{}, len(nodeNames))
	for _, nodeName := range nodeNames {
		if nodeName != "" {
			targetNodes[nodeName] = struct{}{}
		}
	}
	if expectedNodeCount <= 0 || len(targetNodes) != expectedNodeCount || expectedHash == "" {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	for nodeName := range targetNodes {
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		node, err := nodeClient.Get(requestCtx, nodeName, metav1.GetOptions{})
		cancel()
		if err != nil {
			return false, fmt.Errorf("get target node: %w", err)
		}
		if !nodeReadyWithKeyHash(node, expectedHash) {
			return false, nil
		}
	}
	return true, nil
}

func nodeReadyWithKeyHash(node *corev1.Node, expectedHash string) bool {
	if node == nil || expectedHash == "" || node.Annotations[pubKeyHashAnno] != expectedHash {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
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
		e2e.Failf("machineset %s did not reach %d ready replicas within %d minutes (%s)",
			machineSetName, replicas, timeout, machinesetTimeoutDiagnostics(oc, machineSetName))
	}
}

// machinesetTimeoutDiagnostics explains why a MachineSet may not have converged: the machine
// phases it owns and any kubelet bootstrap CSR still waiting for approval. On a cluster whose
// machine approver is not approving new machines, the CSR line is the actionable cause.
func machinesetTimeoutDiagnostics(oc *exutil.CLI, machineSetName string) string {
	machines, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"machines.machine.openshift.io", "-l",
		"machine.openshift.io/cluster-api-machineset="+machineSetName, "-n", mcoNamespace,
		"-o=jsonpath={range .items[*]}{.metadata.name}={.status.phase} {end}").Output()
	if err != nil {
		machines = "<unavailable: " + safePollErrorReason(err) + ">"
	}
	machines = strings.TrimSpace(machines)
	if machines == "" {
		machines = "none"
	}

	created, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("machineset", machineSetName, "-n", mcoNamespace,
		"-o=jsonpath={.metadata.creationTimestamp}").Output()
	since, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil || parseErr != nil {
		since = time.Time{}
	}

	csrs, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csr", "-o=json").Output()
	pendingCSRs := "unknown"
	if err == nil {
		pending := pendingBootstrapCSRs(csrs, since)
		pendingCSRs = "none"
		if len(pending) > 0 {
			pendingCSRs = strings.Join(pending, " ")
		}
	}

	return fmt.Sprintf("machines: %s; unapproved node-bootstrapper CSRs: %s", machines, pendingCSRs)
}

// pendingBootstrapCSRs returns the name and creation time of every kubelet bootstrap CSR that has
// not been approved or denied yet, formatted for a failure message.
func pendingBootstrapCSRs(csrJSON string, since time.Time) []string {
	pending := []string{}
	for _, csr := range gjson.Get(csrJSON, "items").Array() {
		if !strings.Contains(csr.Get("spec.username").String(), "node-bootstrapper") {
			continue
		}
		// Ignore retries left behind by earlier test attempts on the same cluster.
		if !since.IsZero() {
			created, err := time.Parse(time.RFC3339, csr.Get("metadata.creationTimestamp").String())
			if err != nil || created.Before(since) {
				continue
			}
		}
		decided := false
		for _, condition := range gjson.Get(csr.Raw, "status.conditions").Array() {
			conditionType := condition.Get("type").String()
			if condition.Get("status").String() == "True" &&
				(conditionType == "Approved" || conditionType == "Denied") {
				decided = true
				break
			}
		}
		if decided {
			continue
		}
		pending = append(pending, csr.Get("metadata.name").String()+"("+csr.Get("metadata.creationTimestamp").String()+")")
	}
	return pending
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

	// wincTestCM carries optional CI inputs that are not part of a plain WMCO cluster,
	// such as the images used by the multi-OS and disconnected tests. Tests that need
	// these inputs skip when the ConfigMap or key is missing.
	wincTestCM = "winc-test-config"
	// marketplaceNamespace is the only namespace OLM serves to Subscriptions in other namespaces.
	marketplaceNamespace = "openshift-marketplace"
	// machineApproverNamespace and machineApproverDeployment locate the component that approves
	// bootstrap CSRs for machine-backed Windows nodes.
	machineApproverNamespace  = "openshift-cluster-machine-approver"
	machineApproverDeployment = "machine-approver"
	// reinstallCatalogSource is the CatalogSource the namespace reinstall test creates locally
	// when the CI catalog is namespace scoped.
	reinstallCatalogSource            = "wmco-catalog"
	primaryDisconnectedImageKey       = "primary_windows_container_disconnected_image"
	secondaryWindowsImageKey          = "secondary_windows_image"
	secondaryWindowsContainerImageKey = "secondary_windows_container_image"
)

type ConfigMapPayload struct {
	Data struct {
		CaBundleCrt string `json:"ca-bundle.crt"`
	} `json:"data"`
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

func getWindowsNodeNames(oc *exutil.CLI) []string {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[*].metadata.name}").Output()
	o.Expect(err).NotTo(o.HaveOccurred())
	if strings.TrimSpace(output) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(output), " ")
}

func readyWMCOPodName(podJSON string) (string, error) {
	name, _, err := selectReadyWMCOPod(podJSON, false)
	return name, err
}

func readyWMCOPodIP(podJSON string) (string, error) {
	_, address, err := selectReadyWMCOPod(podJSON, true)
	return address, err
}

func selectReadyWMCOPod(podJSON string, requireIP bool) (string, string, error) {
	var podList struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				PodIP      string `json:"podIP"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					Name  string `json:"name"`
					Ready bool   `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(podJSON), &podList); err != nil {
		return "", "", fmt.Errorf("parsing WMCO pod list: %w", err)
	}
	if podList.Items == nil {
		return "", "", fmt.Errorf("parsing WMCO pod list: response has no items array")
	}

	var newestPodName string
	var newestPodIP string
	var newestCreationTime string
	for _, pod := range podList.Items {
		if pod.Metadata.DeletionTimestamp != "" || pod.Status.Phase != "Running" {
			continue
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		managerReady := false
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == "manager" && status.Ready {
				managerReady = true
				break
			}
		}
		if !ready || !managerReady || (requireIP && net.ParseIP(pod.Status.PodIP) == nil) {
			continue
		}
		podName := pod.Metadata.Name
		creationTime := pod.Metadata.CreationTimestamp
		if podName != "" && (newestPodName == "" || creationTime > newestCreationTime) {
			newestPodName = podName
			newestPodIP = pod.Status.PodIP
			newestCreationTime = creationTime
		}
	}
	if newestPodName == "" {
		return "", "", errNoReadyWMCOPod
	}
	return newestPodName, newestPodIP, nil
}

func fetchReadyWMCOManagerLogs(oc *exutil.CLI) (string, error) {
	podJSON, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pods", "--selector", "name="+wmcoDeploymentName,
		"--field-selector=status.phase=Running", "-o=json", "-n", wmcoNamespace,
		"--request-timeout=10s").Output()
	if err != nil {
		return "", fmt.Errorf("listing WMCO pods: %w", err)
	}
	podName, err := readyWMCOPodName(podJSON)
	if err != nil {
		return "", err
	}
	logs, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args(
		podName, "-c", "manager", "-n", wmcoNamespace, "--request-timeout=10s").Output()
	if err != nil {
		if isExpectedPodLogNotFound(err, podName) {
			return "", fmt.Errorf("%w: %v", errWMCOPodDisappeared, err)
		}
		return "", fmt.Errorf("fetching manager logs from ready WMCO pod %s: %w", podName, err)
	}
	return logs, nil
}

func isExpectedPodLogNotFound(err error, expectedPodName string) bool {
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "not found") && !strings.Contains(message, "notfound") {
		return false
	}
	match := podLogResourcePattern.FindStringSubmatch(message)
	if len(match) != 3 {
		return false
	}
	podName := match[1]
	if podName == "" {
		podName = match[2]
	}
	return podName == strings.ToLower(expectedPodName)
}

type wmcoLogFetch func() (string, error)

func waitForWMCOManagerTLSLogs(oc *exutil.CLI, expectedTLSVersion string, timeout time.Duration) error {
	_, err := pollForWMCOManagerTLSLogs(5*time.Second, timeout, expectedTLSVersion, func() (string, error) {
		return fetchReadyWMCOManagerLogs(oc)
	})
	return err
}

// getWMCORestartState returns state that changes when either the WMCO pod is
// recreated or its manager container is restarted in place.
func getWMCORestartState(oc *exutil.CLI) string {
	state, _ := getWMCORestartStateWithContext(context.Background(), oc)
	return state
}

func getWMCORestartStateWithContext(ctx context.Context, oc *exutil.CLI) (string, error) {
	podJSON, err := cliOutputWithContext(ctx, oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"pod", "--selector", "name="+wmcoDeploymentName,
		"--field-selector=status.phase=Running", "-o=json", "-n", wmcoNamespace))
	if err != nil {
		return "", fmt.Errorf("querying WMCO restart state: %w", err)
	}
	if podJSON == "" {
		return "", nil
	}
	if !gjson.Valid(podJSON) {
		return "", fmt.Errorf("parsing WMCO restart state: invalid pod JSON")
	}

	pod := gjson.Parse(podJSON).Get("items.0")
	if !pod.Exists() {
		return "", nil
	}
	manager := pod.Get(`status.containerStatuses.#(name=="manager")`)
	if !manager.Exists() {
		return "", nil
	}

	return fmt.Sprintf("podUID=%s podStartTime=%s managerRestartCount=%s managerContainerID=%s",
		pod.Get("metadata.uid").String(),
		pod.Get("status.startTime").String(),
		manager.Get("restartCount").String(),
		manager.Get("containerID").String()), nil
}

// checkWMCORestarted polls until the WMCO pod is recreated or its manager
// container is restarted in place.
func checkWMCORestarted(oc *exutil.CLI, initialRestartState string) (bool, error) {
	return checkWMCORestartedWithin(oc, initialRestartState, tlsRecoveryRestartTimeout)
}

func checkWMCORestartedWithin(oc *exutil.CLI, initialRestartState string, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return checkWMCORestartedWithinContext(ctx, oc, initialRestartState, timeout)
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

// getConfigMapDataIfPresent returns the value of a ConfigMap key and reports whether it is
// present and non-empty. Optional CI inputs must not fail a test that cannot run without them.
func getConfigMapDataIfPresent(oc *exutil.CLI, cm string, dataKey string, namespace string) (string, bool) {
	value, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"configmap", cm, "-n", namespace, "-o=jsonpath={.data."+dataKey+"}").Output()
	if err != nil {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// waitForNamespaceDeleted waits for a namespace to disappear. Cleanup that must follow workload
// removal, such as uninstalling a CSI node driver, uses this so it does not race the pods.
func waitForNamespaceDeleted(oc *exutil.CLI, namespace string, timeout time.Duration) bool {
	pollErr := wait.Poll(5*time.Second, timeout, func() (bool, error) {
		_, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("namespace", namespace).Output()
		if err != nil && strings.Contains(err.Error(), "NotFound") {
			return true, nil
		}
		return false, nil
	})
	if pollErr != nil {
		e2e.Logf("Warning: namespace %s still exists after %v", namespace, timeout)
		return false
	}
	return true
}

// machineApproverAvailable reports whether the cluster machine approver has a ready replica. The
// approver is scaled down on an idle cluster, and a Windows Machine created while it is down never
// gets its node-bootstrapper CSR approved.
func machineApproverAvailable(oc *exutil.CLI) bool {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("deployment", machineApproverDeployment,
		"-n", machineApproverNamespace, "-o=jsonpath={.status.availableReplicas}").Output()
	if err != nil {
		e2e.Logf("Could not read the machine approver deployment: %v", err)
		return false
	}
	available, convErr := strconv.Atoi(strings.TrimSpace(out))
	return convErr == nil && available > 0
}

// isDisconnectedCluster reports whether the cluster pulls Windows images from a mirror, which is
// how the disconnected CI job provides them. Mirror sets also exist in some connected jobs, so this
// checks that the Windows image registry is redirected instead of only that a mirror set exists.
func isDisconnectedCluster(oc *exutil.CLI) bool {
	mirrorSets, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"imagetagmirrorsets.config.openshift.io,imagedigestmirrorsets.config.openshift.io", "-o=json").Output()
	if err != nil {
		e2e.Logf("Could not read the cluster mirror sets, assuming a connected cluster: %v", err)
		return false
	}
	return windowsImagesMirrored(mirrorSets)
}

// windowsImagesMirrored reports whether a mirror set redirects mcr.microsoft.com, the registry that
// serves the Windows container images, to the cluster's mirror registry.
func windowsImagesMirrored(mirrorSetsJSON string) bool {
	for _, mirrorSet := range gjson.Get(mirrorSetsJSON, "items").Array() {
		for _, path := range []string{"spec.imageTagMirrors.#.source", "spec.imageDigestMirrors.#.source"} {
			for _, mirrorSource := range gjson.Get(mirrorSet.Raw, path).Array() {
				src := mirrorSource.String()
				// A source names a repository, or a whole registry when it carries no path. Both
				// forms mirror every Windows image served by mcr.microsoft.com.
				if src == "mcr.microsoft.com" || strings.HasPrefix(src, "mcr.microsoft.com/") {
					return true
				}
			}
		}
	}
	return false
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

// getNumNodesWithAnnotation returns the number of Windows nodes whose
// windowsmachineconfig.openshift.io/version annotation matches annotationValue.
func getNumNodesWithAnnotation(oc *exutil.CLI, annotationValue string) (int, error) {
	output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"nodes", "-l", windowsNodeLabel,
		"-o=jsonpath={.items[*].metadata.annotations.windowsmachineconfig\\.openshift\\.io\\/version}").Output()
	if err != nil {
		return 0, fmt.Errorf("failed to get node annotations: %w", err)
	}
	count := 0
	for _, v := range strings.Fields(output) {
		if v == annotationValue {
			count++
		}
	}
	return count, nil
}

// generateClusterAutoscalerYAML returns a YAML manifest for a ClusterAutoscaler.
func generateClusterAutoscalerYAML(maxNodesTotal int) string {
	return fmt.Sprintf(`apiVersion: autoscaling.openshift.io/v1
kind: ClusterAutoscaler
metadata:
  name: default
spec:
  podPriorityThreshold: -10
  resourceLimits:
    maxNodesTotal: %d
  scaleDown:
    enabled: true
    delayAfterAdd: 10s
    delayAfterDelete: 10s
    delayAfterFailure: 10s
    unneededTime: 10s`, maxNodesTotal)
}

// generateMachineAutoscalerYAML returns a YAML manifest for a MachineAutoscaler targeting a MachineSet.
func generateMachineAutoscalerYAML(machineSetName string, minReplicas, maxReplicas int) string {
	return fmt.Sprintf(`apiVersion: autoscaling.openshift.io/v1beta1
kind: MachineAutoscaler
metadata:
  name: %s
  namespace: %s
spec:
  minReplicas: %d
  maxReplicas: %d
  scaleTargetRef:
    apiVersion: machine.openshift.io/v1beta1
    kind: MachineSet
    name: %s`, machineSetName, mcoNamespace, minReplicas, maxReplicas, machineSetName)
}
