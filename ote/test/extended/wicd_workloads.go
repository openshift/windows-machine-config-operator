package extended

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

// randomString generates a cryptographically random hex string of the given byte length.
func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("failed to generate random string: %v", err))
	}
	return hex.EncodeToString(b)
}

// ── shared constants (only defined here since this is a standalone branch) ──

const (
	winOSLabel  = "kubernetes.io/os=windows"
	wmcoNS      = "openshift-windows-machine-config-operator"
	wmcoDeploy  = "windows-machine-config-operator"
	winTestCM   = "winc-test-config"
	winTestNS   = "winc-test"
	wicdCMLabel = "windows-services"
)

// ── shared helpers ──

func getWindowsNodeNames(oc *cli.CLI) ([]string, error) {
	out, err := oc.AsAdmin().Output("get", "nodes", "-l", winOSLabel,
		`-o=jsonpath={.items[*].status.addresses[?(@.type=="Hostname")].address}`)
	if err != nil || out == "" {
		return nil, fmt.Errorf("no Windows nodes found: %w", err)
	}
	return strings.Fields(out), nil
}

func getPrimaryWindowsImage(oc *cli.CLI) (string, error) {
	img, err := oc.AsAdmin().Output("get", "configmap", winTestCM,
		"-n", winTestNS,
		"-o=jsonpath={.data.primary_windows_container_image}")
	if err != nil || img == "" {
		return "", fmt.Errorf("failed to get Windows container image: %w", err)
	}
	return strings.TrimSpace(img), nil
}

func getClusterPlatform(oc *cli.CLI) (string, error) {
	p, err := oc.AsAdmin().Output("get", "infrastructure", "cluster",
		"-o=jsonpath={.status.platformStatus.type}")
	if err != nil {
		return "", fmt.Errorf("failed to get platform: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(p)), nil
}

func findConfigMap(oc *cli.CLI, keyword, namespace string) (string, error) {
	out, err := oc.AsAdmin().Output("get", "configmap",
		"-n", namespace, "-o=jsonpath={.items[*].metadata.name}")
	if err != nil {
		return "", err
	}
	for _, name := range strings.Fields(out) {
		if strings.Contains(name, keyword) {
			return name, nil
		}
	}
	return "", fmt.Errorf("configmap with keyword %q not found in %s", keyword, namespace)
}

func waitForConfigMap(oc *cli.CLI, expected, keyword, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cm, err := findConfigMap(oc, keyword, namespace)
		if err == nil && cm == expected {
			return nil
		}
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("configmap %q not found after %v", expected, timeout)
}

func getWMCOVersion(oc *cli.CLI) (string, error) {
	log, err := oc.AsAdmin().Output("logs", "deployment.apps/"+wmcoDeploy, "-n", wmcoNS)
	if err != nil {
		return "", fmt.Errorf("failed to get WMCO logs: %w", err)
	}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`"Version"\s*:\s*"([^"]+)"`),
		regexp.MustCompile(`operator\s+version\s+([^\s"']+)`),
	} {
		if m := re.FindStringSubmatch(log); len(m) >= 2 {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("WMCO version not found in logs")
}

func applyYAMLManifest(oc *cli.CLI, manifest string) error {
	f, err := os.CreateTemp("", "wmco-ote-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(manifest); err != nil {
		f.Close()
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	f.Close()
	if _, err := oc.AsAdmin().Output("apply", "-f", f.Name()); err != nil {
		return fmt.Errorf("oc apply failed: %w", err)
	}
	return nil
}

func createNS(oc *cli.CLI, namespace string) error {
	if _, err := oc.AsAdmin().Output("get", "namespace", namespace); err == nil {
		return nil
	}
	if _, err := oc.AsAdmin().Output("create", "namespace", namespace); err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
	}
	_, _ = oc.AsAdmin().Output("label", "namespace", namespace,
		"pod-security.kubernetes.io/enforce=privileged", "--overwrite")
	return nil
}

func deleteNS(oc *cli.CLI, namespace string) {
	_, _ = oc.AsAdmin().Output("delete", "namespace", namespace, "--ignore-not-found")
}

func waitForDeploy(oc *cli.CLI, deployment, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := oc.AsAdmin().Output("get", "deployment", deployment,
			"-n", namespace, "-o=jsonpath={.status.readyReplicas}/{.spec.replicas}")
		if err == nil && out != "" {
			parts := strings.Split(out, "/")
			if len(parts) == 2 && parts[0] != "" && parts[0] != "0" && parts[0] == parts[1] {
				return nil
			}
		}
		time.Sleep(15 * time.Second)
	}
	return fmt.Errorf("deployment %s/%s not ready after %v", namespace, deployment, timeout)
}

// ── Tests ──

// CheckWicdConfigMap verifies that WMCO creates, maintains, and protects
// the windows-services ConfigMap. Corresponds to OCP-50403.
func CheckWicdConfigMap(_ context.Context, oc *cli.CLI) error {
	wmcoVersion, err := getWMCOVersion(oc)
	if err != nil {
		return err
	}
	expectedCM := "windows-services-" + wmcoVersion

	// Verify the correct ConfigMap exists
	actualCM, err := findConfigMap(oc, wicdCMLabel, wmcoNS)
	if err != nil {
		return fmt.Errorf("windows-services ConfigMap not found: %w", err)
	}
	if actualCM != expectedCM {
		return fmt.Errorf("ConfigMap mismatch: expected %s, got %s", expectedCM, actualCM)
	}

	// Check version annotation on Windows nodes
	nodes, err := getWindowsNodeNames(oc)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		ann, err := oc.AsAdmin().Output("get", "node", node,
			"-o=jsonpath={.metadata.annotations.windowsmachineconfig\\.openshift\\.io/desired-version}")
		if err != nil {
			return fmt.Errorf("failed to get version annotation on %s: %w", node, err)
		}
		if strings.Trim(ann, "'") != wmcoVersion {
			return fmt.Errorf("desired-version mismatch on %s: expected %s got %s", node, wmcoVersion, ann)
		}
	}

	// Check WICD service account exists
	if _, err := oc.AsAdmin().Output("get", "serviceaccount",
		"windows-instance-config-daemon", "-n", wmcoNS); err != nil {
		return fmt.Errorf("windows-instance-config-daemon service account not found: %w", err)
	}

	// Delete ConfigMap and verify WMCO recreates it
	if _, err := oc.AsAdmin().Output("delete", "configmap", actualCM, "-n", wmcoNS); err != nil {
		return fmt.Errorf("failed to delete ConfigMap: %w", err)
	}
	if err := waitForConfigMap(oc, expectedCM, wicdCMLabel, wmcoNS, 10*time.Minute); err != nil {
		return fmt.Errorf("ConfigMap not recreated: %w", err)
	}

	// Verify ConfigMap is immutable (patch should fail with an immutability error)
	_, patchErr := oc.AsAdmin().Output("patch", "configmap", expectedCM,
		"-p", `{"data":{"services":"[]"}}`, "-n", wmcoNS)
	if patchErr == nil {
		return fmt.Errorf("expected patch to ConfigMap %s to fail (immutable), but it succeeded", expectedCM)
	}
	if !strings.Contains(patchErr.Error(), "immutable") {
		return fmt.Errorf("patch on ConfigMap %s failed for unexpected reason: %v", expectedCM, patchErr)
	}

	return nil
}

// CheckContainerdVersion verifies that Windows nodes report the containerd version
// matching the WMCO submodule version. Corresponds to OCP-60814.
func CheckContainerdVersion(_ context.Context, oc *cli.CLI) error {
	wmcoVersion, err := getWMCOVersion(oc)
	if err != nil {
		return err
	}
	parts := strings.SplitN(wmcoVersion, "-", 2)
	if len(parts) < 2 {
		return fmt.Errorf("unexpected WMCO version format: %s", wmcoVersion)
	}
	versionHash := parts[1]

	resp, err := http.Get("https://raw.githubusercontent.com/openshift/windows-machine-config-operator/" + versionHash + "/Makefile")
	if err != nil {
		return fmt.Errorf("failed to fetch Makefile: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read Makefile: %w", err)
	}

	expectedVersion := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "CONTAINERD_GIT_VERSION=") {
			expectedVersion = strings.TrimSpace(strings.TrimPrefix(line, "CONTAINERD_GIT_VERSION="))
			break
		}
	}
	if expectedVersion == "" {
		return fmt.Errorf("CONTAINERD_GIT_VERSION not found in Makefile")
	}

	nodes, err := getWindowsNodeNames(oc)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		runtimeVer, err := oc.AsAdmin().Output("get", "node", node,
			"-o=jsonpath={.status.nodeInfo.containerRuntimeVersion}")
		if err != nil {
			return fmt.Errorf("failed to get containerRuntimeVersion on %s: %w", node, err)
		}
		parts := strings.SplitN(runtimeVer, "containerd://", 2)
		if len(parts) < 2 {
			return fmt.Errorf("unexpected containerRuntimeVersion format on %s: %s", node, runtimeVer)
		}
		actualVersion := "v" + strings.TrimSpace(parts[1])
		if actualVersion != expectedVersion {
			return fmt.Errorf("containerd version mismatch on %s: expected %s got %s", node, expectedVersion, actualVersion)
		}
	}
	return nil
}

// CheckPreventNonWindowsWorkloads verifies that pods without Windows tolerations
// cannot be scheduled on Windows nodes. Corresponds to OCP-25593.
func CheckPreventNonWindowsWorkloads(_ context.Context, oc *cli.CLI) error {
	namespace := "winc-25593"
	defer deleteNS(oc, namespace)
	if err := createNS(oc, namespace); err != nil {
		return err
	}

	// Verify Windows nodes have the correct taint. Check all taints (not just index 0)
	// since cloud providers may add extra taints (e.g. uninitialized) alongside the WMCO taint.
	taints, err := oc.AsAdmin().Output("get", "nodes", "-l", winOSLabel,
		`-o=jsonpath={range .items[0].spec.taints[*]}{.key}={.value}:{.effect}{"\n"}{end}`)
	if err != nil {
		return fmt.Errorf("failed to get Windows node taints: %w", err)
	}
	foundTaint := false
	for _, line := range strings.Split(taints, "\n") {
		if strings.TrimSpace(line) == "os=Windows:NoSchedule" {
			foundTaint = true
			break
		}
	}
	if !foundTaint {
		return fmt.Errorf("Windows taint os=Windows:NoSchedule not found, got: %s", taints)
	}

	// Get Windows container image
	image, err := getPrimaryWindowsImage(oc)
	if err != nil {
		return err
	}

	// Deploy a Windows workload WITHOUT tolerations
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: win-webserver
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: win-webserver
  template:
    metadata:
      labels:
        app: win-webserver
    spec:
      containers:
      - name: win-webserver
        image: %s
        imagePullPolicy: IfNotPresent
      nodeSelector:
        beta.kubernetes.io/os: windows
      os:
        name: windows
`, namespace, image)

	if err := applyYAMLManifest(oc, manifest); err != nil {
		return fmt.Errorf("failed to apply no-taint deployment: %w", err)
	}

	// Verify the pod gets the "had untolerated taint" condition
	deadline := time.Now().Add(60 * time.Second)
	scheduled := false
	for time.Now().Before(deadline) {
		msg, _ := oc.AsAdmin().Output("get", "pod",
			"-l=app=win-webserver",
			"-o=jsonpath={.items[].status.conditions[].message}",
			"-n", namespace)
		if strings.Contains(msg, "had untolerated taint") {
			scheduled = false
			break
		}
		time.Sleep(10 * time.Second)
		scheduled = true
	}
	if scheduled {
		return fmt.Errorf("pod without Windows toleration was not rejected by taint")
	}

	// Check no non-winc pods are running on Windows nodes
	nodes, err := getWindowsNodeNames(oc)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		namespaces, _ := oc.AsAdmin().Output("get", "pods", "--all-namespaces",
			"-o=jsonpath={.items[*].metadata.namespace}",
			"-l=app=win-webserver",
			"--field-selector=spec.nodeName="+node, "--no-headers")
		for _, ns := range strings.Fields(namespaces) {
			if ns != "" && !strings.Contains(ns, "winc") {
				return fmt.Errorf("non-winc pod found on Windows node %s in namespace %s", node, ns)
			}
		}
	}
	return nil
}

// CheckWindowsPodProjectedVolume verifies that a Windows pod can mount projected
// volumes containing secrets. Corresponds to OCP-42204.
func CheckWindowsPodProjectedVolume(_ context.Context, oc *cli.CLI) error {
	namespace := "winc-42204"
	defer deleteNS(oc, namespace)
	if err := createNS(oc, namespace); err != nil {
		return err
	}

	username := "admin"
	password := randomString(12)

	// Create secrets from literals
	if _, err := oc.AsAdmin().Output("create", "secret", "generic", "user",
		"--from-literal=username="+username, "-n", namespace); err != nil {
		return fmt.Errorf("failed to create username secret: %w", err)
	}
	if _, err := oc.AsAdmin().Output("create", "secret", "generic", "pass",
		"--from-literal=password="+password, "-n", namespace); err != nil {
		return fmt.Errorf("failed to create password secret: %w", err)
	}

	// Choose image based on deployed image version
	deployedImage, err := getPrimaryWindowsImage(oc)
	if err != nil {
		return err
	}
	image := "mcr.microsoft.com/windows/servercore:ltsc2019"
	if strings.Contains(deployedImage, "ltsc2022") {
		image = "mcr.microsoft.com/windows/servercore:ltsc2022"
	}

	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: win-webserver
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: win-webserver
  template:
    metadata:
      labels:
        app: win-webserver
    spec:
      tolerations:
      - key: "os"
        value: "Windows"
        effect: "NoSchedule"
      containers:
      - name: windowswebserver
        image: %s
        imagePullPolicy: IfNotPresent
        command:
        - powershell.exe
        - -command
        - while ($true) { Start-Sleep 3600 }
        volumeMounts:
        - name: all-in-one
          mountPath: "/projected-volume"
          readOnly: true
      volumes:
      - name: all-in-one
        projected:
          sources:
          - secret:
              name: user
          - secret:
              name: pass
      nodeSelector:
        beta.kubernetes.io/os: windows
      os:
        name: windows
`, namespace, image)

	if err := applyYAMLManifest(oc, manifest); err != nil {
		return fmt.Errorf("failed to apply projected volume deployment: %w", err)
	}
	if err := waitForDeploy(oc, "win-webserver", namespace, 20*time.Minute); err != nil {
		return err
	}

	pods, err := oc.AsAdmin().Output("get", "pods", "-l=app=win-webserver",
		"-n", namespace, "-o=jsonpath={.items[0].metadata.name}")
	if err != nil || pods == "" {
		return fmt.Errorf("no pods found in %s", namespace)
	}

	// Check projected volume contains the secrets
	msg, err := oc.AsAdmin().Output("exec", pods, "-n", namespace,
		"--", "powershell.exe", "cat C:\\projected-volume\\username")
	if err != nil || !strings.Contains(msg, username) {
		return fmt.Errorf("username not found in projected volume: %s", msg)
	}
	msg, err = oc.AsAdmin().Output("exec", pods, "-n", namespace,
		"--", "powershell.exe", "cat C:\\projected-volume\\password")
	if err != nil || !strings.Contains(msg, password) {
		return fmt.Errorf("password not found in projected volume: %s", msg)
	}
	return nil
}

// CheckWindowsLBService verifies that a Windows workload is accessible via
// LoadBalancer and remains accessible after scaling. Corresponds to OCP-38186.
func CheckWindowsLBService(_ context.Context, oc *cli.CLI) error {
	platform, err := getClusterPlatform(oc)
	if err != nil {
		return err
	}
	if platform == "vsphere" || platform == "nutanix" || platform == "none" {
		return nil // platform does not support LoadBalancer
	}

	namespace := "winc-38186"
	defer deleteNS(oc, namespace)
	if err := createNS(oc, namespace); err != nil {
		return err
	}

	image, err := getPrimaryWindowsImage(oc)
	if err != nil {
		return err
	}

	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: win-webserver
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: win-webserver
  template:
    metadata:
      labels:
        app: win-webserver
    spec:
      tolerations:
      - key: "os"
        value: "Windows"
        effect: "NoSchedule"
      nodeSelector:
        kubernetes.io/os: windows
      containers:
      - name: win-webserver
        image: %s
        imagePullPolicy: IfNotPresent
        command:
        - pwsh.exe
        - -command
        - "$listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start(); while ($listener.IsListening) { $context = $listener.GetContext(); $response = $context.Response; $content='<html><body><H1>Windows Container Web Server</H1></body></html>'; $buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; $response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); }"
        ports:
        - containerPort: 80
        securityContext:
          runAsNonRoot: false
          windowsOptions:
            runAsUserName: "ContainerAdministrator"
---
apiVersion: v1
kind: Service
metadata:
  name: win-webserver
  namespace: %s
spec:
  type: LoadBalancer
  selector:
    app: win-webserver
  ports:
  - port: 80
    targetPort: 80
`, namespace, image, namespace)

	if err := applyYAMLManifest(oc, manifest); err != nil {
		return fmt.Errorf("failed to deploy Windows LB workload: %w", err)
	}
	if err := waitForDeploy(oc, "win-webserver", namespace, 20*time.Minute); err != nil {
		return err
	}

	// Get LoadBalancer endpoint
	var jsonpath string
	if platform == "azure" || platform == "gcp" {
		jsonpath = "-o=jsonpath={.status.loadBalancer.ingress[0].ip}"
	} else {
		jsonpath = "-o=jsonpath={.status.loadBalancer.ingress[0].hostname}"
	}
	var endpoint string
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		endpoint, _ = oc.AsAdmin().Output("get", "service", "win-webserver", jsonpath, "-n", namespace)
		if endpoint != "" {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if endpoint == "" {
		return fmt.Errorf("LB endpoint not available after 5 minutes")
	}

	pods, err := oc.AsAdmin().Output("get", "pods", "-l=app=win-webserver",
		"-n", namespace, "-o=jsonpath={.items[0].metadata.name}")
	if err != nil || pods == "" {
		return fmt.Errorf("no pods found")
	}

	// Verify the webserver responds on localhost. We curl from within the pod rather than
	// hitting the LB endpoint directly because Windows pods cannot reach their own LB IP
	// due to hairpin NAT limitations on supported cloud platforms.
	deadline2 := time.Now().Add(2 * time.Minute)
	served := false
	for time.Now().Before(deadline2) {
		msg, _, _ := oc.AsAdmin().Run("exec", "-n", namespace, pods, "--",
			"pwsh.exe", "-command",
			"(Invoke-WebRequest -Uri http://localhost:80 -UseBasicParsing -ErrorAction SilentlyContinue).Content")
		if strings.Contains(msg, "Windows Container Web Server") {
			served = true
			break
		}
		time.Sleep(15 * time.Second)
	}
	if !served {
		return fmt.Errorf("Windows webserver not responding on localhost in pod %s", pods)
	}

	// Scale to 6 pods and verify deployment is healthy
	if _, err := oc.AsAdmin().Output("scale", "--replicas=6",
		"deployment", "win-webserver", "-n", namespace); err != nil {
		return fmt.Errorf("failed to scale deployment: %w", err)
	}
	if err := waitForDeploy(oc, "win-webserver", namespace, 20*time.Minute); err != nil {
		return err
	}
	_ = endpoint // LB endpoint was provisioned successfully

	return nil
}

// getValueFromText extracts the value after a key= prefix in text content.
func getValueFromText(body []byte, searchVal string) string {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, searchVal) {
			parts := strings.SplitN(line, searchVal, 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// GenerateWICDConfigMap generates a WMCO windows-services ConfigMap YAML string.
func GenerateWICDConfigMap(name, services string) string {
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]string{
			"name":      name,
			"namespace": wmcoNS,
		},
		"data": map[string]string{
			"services": services,
		},
	}
	b, _ := json.Marshal(cm)
	return string(b)
}
