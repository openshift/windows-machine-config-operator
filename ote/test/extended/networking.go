package extended

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

const (
	defaultNamespace  = "winc-test"
	windowsWorkloads  = "win-webserver"
	linuxWorkloads    = "linux-webserver"
	linuxTagsImage    = "quay.io/openshifttest/hello-openshift:multiarch-winc"
	wincTestCM        = "winc-test-config"
	windowsServiceDNS = "win-webserver.winc-test.svc.cluster.local"
	linuxServiceDNS   = "linux-webserver.winc-test.svc.cluster.local:8080"
)

// getPlatform returns the cluster's IaaS platform type in lowercase.
func getPlatform(oc *cli.CLI) (string, error) {
	p, err := oc.AsAdmin().Output("get", "infrastructure", "cluster",
		"-o=jsonpath={.status.platformStatus.type}")
	if err != nil {
		return "", fmt.Errorf("failed to get platform: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(p)), nil
}

// isPlatformNone returns true if the cluster runs on platform "none".
func isPlatformNone(oc *cli.CLI) (bool, error) {
	p, err := getPlatform(oc)
	if err != nil {
		return false, err
	}
	return p == "none", nil
}

// getConfigMapValue returns a value from a ConfigMap data field.
func getConfigMapValue(oc *cli.CLI, cm, key, namespace string) (string, error) {
	v, err := oc.AsAdmin().Output("get", "configmap", cm,
		"-n", namespace,
		fmt.Sprintf("-o=jsonpath={.data.%s}", key))
	if err != nil {
		return "", fmt.Errorf("failed to get configmap %s key %s: %w", cm, key, err)
	}
	return strings.TrimSpace(v), nil
}

// createNamespace creates a namespace (no-op if it already exists).
func createNamespace(oc *cli.CLI, namespace string) error {
	if _, err := oc.AsAdmin().Output("get", "namespace", namespace); err == nil {
		return nil
	}
	if _, err := oc.AsAdmin().Output("create", "namespace", namespace); err != nil {
		return fmt.Errorf("failed to create namespace %s: %w", namespace, err)
	}
	_, _ = oc.AsAdmin().Output("label", "namespace", namespace,
		"pod-security.kubernetes.io/enforce=privileged",
		"pod-security.kubernetes.io/warn=privileged",
		"--overwrite")
	return nil
}

// deleteNamespace deletes a namespace (best-effort cleanup).
func deleteNamespace(oc *cli.CLI, namespace string) {
	_, _ = oc.AsAdmin().Output("delete", "namespace", namespace, "--ignore-not-found")
}

// applyYAML writes a YAML string to a temp file and runs oc apply.
func applyYAML(oc *cli.CLI, yaml string) error {
	f, err := os.CreateTemp("", "wmco-ote-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(yaml); err != nil {
		f.Close()
		return fmt.Errorf("failed to write manifest: %w", err)
	}
	f.Close()
	if _, err := oc.AsAdmin().Output("apply", "-f", f.Name()); err != nil {
		return fmt.Errorf("oc apply failed: %w", err)
	}
	return nil
}

// waitForDeploymentReady polls until a deployment has all pods ready.
func waitForDeploymentReady(oc *cli.CLI, deployment, namespace string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := oc.AsAdmin().Output("get", "deployment", deployment,
			"-n", namespace,
			"-o=jsonpath={.status.readyReplicas}/{.spec.replicas}")
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

// deployWindowsWorkload creates a Windows webserver deployment and ClusterIP service.
func deployWindowsWorkload(oc *cli.CLI, image, namespace string) error {
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
        ports:
        - containerPort: 80
---
apiVersion: v1
kind: Service
metadata:
  name: win-webserver
  namespace: %s
spec:
  selector:
    app: win-webserver
  ports:
  - port: 80
    targetPort: 80
`, namespace, image, namespace)
	if err := applyYAML(oc, manifest); err != nil {
		return err
	}
	return waitForDeploymentReady(oc, windowsWorkloads, namespace, 10*time.Minute)
}

// deployWindowsWorkloadWithLB creates a Windows webserver deployment and LoadBalancer service.
func deployWindowsWorkloadWithLB(oc *cli.CLI, image, namespace string) error {
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
        ports:
        - containerPort: 80
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
	if err := applyYAML(oc, manifest); err != nil {
		return err
	}
	return waitForDeploymentReady(oc, windowsWorkloads, namespace, 10*time.Minute)
}

// deployLinuxWorkload creates a Linux webserver deployment and ClusterIP service.
func deployLinuxWorkload(oc *cli.CLI, namespace string) error {
	manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: linux-webserver
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: linux-webserver
  template:
    metadata:
      labels:
        app: linux-webserver
    spec:
      containers:
      - name: linux-webserver
        image: %s
        ports:
        - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: linux-webserver
  namespace: %s
spec:
  selector:
    app: linux-webserver
  ports:
  - port: 8080
    targetPort: 8080
`, namespace, linuxTagsImage, namespace)
	if err := applyYAML(oc, manifest); err != nil {
		return err
	}
	return waitForDeploymentReady(oc, linuxWorkloads, namespace, 5*time.Minute)
}

// getPodNames returns pod names for a deployment, sorted by hostIP.
func getPodNames(oc *cli.CLI, deployment, namespace string) ([]string, error) {
	out, err := oc.AsAdmin().Output("get", "pods",
		"--selector", "app="+deployment,
		"--sort-by=.status.hostIP",
		"-o=jsonpath={.items[*].metadata.name}",
		"-n", namespace)
	if err != nil || out == "" {
		return nil, fmt.Errorf("no pods found for %s in %s: %w", deployment, namespace, err)
	}
	return strings.Fields(out), nil
}

// getPodIPs returns pod IP addresses for a deployment, sorted by hostIP.
func getPodIPs(oc *cli.CLI, deployment, namespace string) ([]string, error) {
	out, err := oc.AsAdmin().Output("get", "pods",
		"--selector", "app="+deployment,
		"--sort-by=.status.hostIP",
		"-o=jsonpath={.items[*].status.podIP}",
		"-n", namespace)
	if err != nil || out == "" {
		return nil, fmt.Errorf("no pod IPs for %s in %s: %w", deployment, namespace, err)
	}
	return strings.Fields(out), nil
}

// getPodHostIPs returns host IP addresses for pods in a deployment.
func getPodHostIPs(oc *cli.CLI, deployment, namespace string) ([]string, error) {
	out, err := oc.AsAdmin().Output("get", "pods",
		"--selector", "app="+deployment,
		"--sort-by=.status.hostIP",
		"-o=jsonpath={.items[*].status.hostIP}",
		"-n", namespace)
	if err != nil || out == "" {
		return nil, fmt.Errorf("no host IPs for %s in %s: %w", deployment, namespace, err)
	}
	return strings.Fields(out), nil
}

// getServiceClusterIP returns the ClusterIP of a service.
func getServiceClusterIP(oc *cli.CLI, service, namespace string) (string, error) {
	ip, err := oc.AsAdmin().Output("get", "service", service,
		"-o=jsonpath={.spec.clusterIP}", "-n", namespace)
	if err != nil || ip == "" {
		return "", fmt.Errorf("failed to get ClusterIP for %s: %w", service, err)
	}
	return ip, nil
}

// getLBEndpoint returns the LoadBalancer ingress IP or hostname.
func getLBEndpoint(oc *cli.CLI, platform, service, namespace string) (string, error) {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var jsonpath string
		if platform == "azure" || platform == "gcp" {
			jsonpath = "-o=jsonpath={.status.loadBalancer.ingress[0].ip}"
		} else {
			jsonpath = "-o=jsonpath={.status.loadBalancer.ingress[0].hostname}"
		}
		endpoint, err := oc.AsAdmin().Output("get", "service", service, jsonpath, "-n", namespace)
		if err == nil && endpoint != "" {
			return endpoint, nil
		}
		time.Sleep(10 * time.Second)
	}
	return "", fmt.Errorf("LB endpoint not available after 5 minutes")
}

// psInvokeWebRequest builds a PowerShell Invoke-WebRequest command.
func psInvokeWebRequest(url string) string {
	return fmt.Sprintf(
		"$r = Invoke-WebRequest -Uri %s -UseBasicParsing -ErrorAction SilentlyContinue; "+
			"if ($r.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($r.Content) } else { $r.Content }",
		url)
}

// scaleDeploymentNet scales a deployment and waits for it to be ready.
func scaleDeploymentNet(oc *cli.CLI, deployment, namespace string, replicas int) error {
	if _, err := oc.AsAdmin().Output("scale",
		fmt.Sprintf("--replicas=%d", replicas),
		"deployment", deployment, "-n", namespace); err != nil {
		return fmt.Errorf("failed to scale %s: %w", deployment, err)
	}
	return waitForDeploymentReady(oc, deployment, namespace, 10*time.Minute)
}

// CheckEastWestNetwork verifies Windows↔Linux pod communication via direct pod IPs.
// Requires pre-existing workloads in winc-test namespace (set up by Flexy/cluster setup).
// Corresponds to OCP-28632.
func CheckEastWestNetwork(_ context.Context, oc *cli.CLI) error {
	winPods, err := getPodNames(oc, windowsWorkloads, defaultNamespace)
	if err != nil {
		return fmt.Errorf("failed to get Windows pod names: %w", err)
	}
	winIPs, err := getPodIPs(oc, windowsWorkloads, defaultNamespace)
	if err != nil {
		return fmt.Errorf("failed to get Windows pod IPs: %w", err)
	}
	linuxPods, err := getPodNames(oc, linuxWorkloads, defaultNamespace)
	if err != nil {
		return fmt.Errorf("failed to get Linux pod names: %w", err)
	}
	linuxIPs, err := getPodIPs(oc, linuxWorkloads, defaultNamespace)
	if err != nil {
		return fmt.Errorf("failed to get Linux pod IPs: %w", err)
	}

	// Windows pod → Linux pod
	msg, err := oc.AsAdmin().Output("exec", "-n", defaultNamespace, winPods[0],
		"--", "pwsh.exe", "-Command", psInvokeWebRequest("http://"+linuxIPs[0]+":8080"))
	if err != nil {
		return fmt.Errorf("exec into Windows pod failed: %w", err)
	}
	if !strings.Contains(msg, "Linux Container Web Server") {
		return fmt.Errorf("Windows pod cannot reach Linux pod, got: %s", func() string {
			if len(msg) > 200 {
				return msg[:200]
			}
			return msg
		}())
	}

	// Linux pod → Windows pod
	msg, err = oc.AsAdmin().Output("exec", "-n", defaultNamespace, linuxPods[0],
		"--", "curl", winIPs[0])
	if err != nil {
		return fmt.Errorf("exec into Linux pod failed: %w", err)
	}
	if !strings.Contains(msg, "Windows Container Web Server") {
		return fmt.Errorf("Linux pod cannot reach Windows pod, got: %s", func() string {
			if len(msg) > 200 {
				return msg[:200]
			}
			return msg
		}())
	}
	return nil
}

// CheckExternalNetworking verifies a Windows workload is accessible via LoadBalancer.
// Skipped on vsphere, nutanix and platform=none. Corresponds to OCP-32273.
func CheckExternalNetworking(_ context.Context, oc *cli.CLI) error {
	platform, err := getPlatform(oc)
	if err != nil {
		return err
	}
	if platform == "vsphere" || platform == "nutanix" {
		return nil // platform does not support LoadBalancer
	}
	none, err := isPlatformNone(oc)
	if err != nil {
		return err
	}
	if none {
		return nil // platform none does not support LoadBalancer
	}

	namespace := "winc-32273"
	defer deleteNamespace(oc, namespace)
	if err := createNamespace(oc, namespace); err != nil {
		return err
	}

	image, err := getConfigMapValue(oc, wincTestCM, "primary_windows_container_image", defaultNamespace)
	if err != nil || image == "" {
		return fmt.Errorf("failed to get Windows container image: %w", err)
	}
	if err := deployWindowsWorkloadWithLB(oc, image, namespace); err != nil {
		return fmt.Errorf("failed to deploy Windows workload with LB: %w", err)
	}

	endpoint, err := getLBEndpoint(oc, platform, windowsWorkloads, namespace)
	if err != nil {
		return err
	}

	// Poll until LB responds (up to 5 minutes)
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		// Use oc exec from the Windows pod to curl the LB endpoint
		pods, err := getPodNames(oc, windowsWorkloads, namespace)
		if err == nil && len(pods) > 0 {
			msg, _, _ := oc.AsAdmin().Run("exec", "-n", namespace, pods[0],
				"--", "curl", "--max-time", "10", "http://"+endpoint)
			if strings.Contains(msg, "Windows Container Web Server") {
				return nil
			}
		}
		time.Sleep(20 * time.Second)
	}
	return fmt.Errorf("LoadBalancer at %s did not serve Windows workload after 5 minutes", endpoint)
}

// CheckCNIAndInternalNetworking verifies Windows↔Linux connectivity via pod IPs and DNS,
// including same-node and cross-node Windows pod connectivity.
// Corresponds to OCP-31276.
func CheckCNIAndInternalNetworking(_ context.Context, oc *cli.CLI) error {
	namespace := "winc-31276"
	defer deleteNamespace(oc, namespace)
	if err := createNamespace(oc, namespace); err != nil {
		return err
	}

	image, err := getConfigMapValue(oc, wincTestCM, "primary_windows_container_image", defaultNamespace)
	if err != nil || image == "" {
		return fmt.Errorf("failed to get Windows container image: %w", err)
	}

	if err := deployWindowsWorkload(oc, image, namespace); err != nil {
		return fmt.Errorf("failed to deploy Windows workload: %w", err)
	}
	if err := deployLinuxWorkload(oc, namespace); err != nil {
		return fmt.Errorf("failed to deploy Linux workload: %w", err)
	}

	// Scale to 5 Windows pods to test same-node and cross-node communication
	if err := scaleDeploymentNet(oc, windowsWorkloads, namespace, 5); err != nil {
		return fmt.Errorf("failed to scale Windows deployment to 5: %w", err)
	}

	winPods, err := getPodNames(oc, windowsWorkloads, namespace)
	if err != nil {
		return err
	}
	linuxPods, err := getPodNames(oc, linuxWorkloads, namespace)
	if err != nil {
		return err
	}
	winIPs, err := getPodIPs(oc, windowsWorkloads, namespace)
	if err != nil {
		return err
	}
	linuxIPs, err := getPodIPs(oc, linuxWorkloads, namespace)
	if err != nil {
		return err
	}
	hostIPs, err := getPodHostIPs(oc, windowsWorkloads, namespace)
	if err != nil {
		return err
	}

	// Linux → Windows pod via direct IP
	msg, err := oc.AsAdmin().Output("exec", "-n", namespace, linuxPods[0],
		"--", "curl", winIPs[0])
	if err != nil || !strings.Contains(msg, "Windows Container Web Server") {
		return fmt.Errorf("Linux pod cannot reach Windows pod via IP: %s", msg)
	}

	// Windows → Linux pod via direct IP
	msg, err = oc.AsAdmin().Output("exec", "-n", namespace, winPods[0],
		"--", "pwsh.exe", "-Command", psInvokeWebRequest("http://"+linuxIPs[0]+":8080"))
	if err != nil || !strings.Contains(msg, "Linux Container Web Server") {
		return fmt.Errorf("Windows pod cannot reach Linux pod via IP: %s", msg)
	}

	// Same-node Windows↔Windows pod communication
	for i := 0; i < len(hostIPs)-1 && i < len(winPods)-1; i++ {
		if hostIPs[i] == hostIPs[i+1] {
			msg, err = oc.AsAdmin().Output("exec", "-n", namespace, winPods[i],
				"--", "pwsh.exe", "-Command", psInvokeWebRequest("http://"+winIPs[i+1]))
			if err != nil || !strings.Contains(msg, "Windows Container Web Server") {
				return fmt.Errorf("same-node Windows pod communication failed: %s", msg)
			}
			break
		}
	}

	// Linux → Windows via DNS (use namespace-specific DNS name)
	winDNS := fmt.Sprintf("win-webserver.%s.svc.cluster.local", namespace)
	linuxDNS := fmt.Sprintf("linux-webserver.%s.svc.cluster.local:8080", namespace)
	msg, err = oc.AsAdmin().Output("exec", "-n", namespace, linuxPods[0],
		"--", "curl", winDNS)
	if err != nil || !strings.Contains(msg, "Windows Container Web Server") {
		return fmt.Errorf("Linux pod cannot reach Windows pod via DNS: %s", msg)
	}

	// Windows → Linux via DNS
	msg, err = oc.AsAdmin().Output("exec", "-n", namespace, winPods[0],
		"--", "pwsh.exe", "-Command", psInvokeWebRequest("http://"+linuxDNS))
	if err != nil || !strings.Contains(msg, "Linux Container Web Server") {
		return fmt.Errorf("Windows pod cannot reach Linux pod via DNS: %s", msg)
	}

	// Cross-node Windows↔Windows communication
	if len(hostIPs) > 1 && hostIPs[0] != hostIPs[len(hostIPs)-1] {
		lastIdx := len(winPods) - 1
		msg, err = oc.AsAdmin().Output("exec", "-n", namespace, winPods[0],
			"--", "pwsh.exe", "-Command", psInvokeWebRequest("http://"+winIPs[lastIdx]))
		if err != nil || !strings.Contains(msg, "Windows Container Web Server") {
			return fmt.Errorf("cross-node Windows pod communication failed: %s", msg)
		}
	}

	return nil
}
