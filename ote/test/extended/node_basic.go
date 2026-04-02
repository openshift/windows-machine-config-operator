package extended

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

const (
	windowsNodeLabel = "kubernetes.io/os=windows"
	linuxNodeLabel   = "kubernetes.io/os=linux"
	mcoNamespace     = "openshift-machine-api"
)

// getWindowsInternalIPs returns the internal IP addresses of all Windows worker nodes.
func getWindowsInternalIPs(oc *cli.CLI) ([]string, error) {
	ips, err := oc.AsAdmin().Output("get", "nodes",
		"-l", windowsNodeLabel,
		`-o=jsonpath={.items[*].status.addresses[?(@.type=="InternalIP")].address}`)
	if err != nil {
		return nil, fmt.Errorf("failed to get Windows node IPs: %w", err)
	}
	if ips == "" {
		return []string{}, nil
	}
	return strings.Split(ips, " "), nil
}

// getWindowsHostNames returns the hostnames of all Windows worker nodes.
func getWindowsHostNames(oc *cli.CLI) ([]string, error) {
	names, err := oc.AsAdmin().Output("get", "nodes",
		"-l", windowsNodeLabel,
		`-o=jsonpath={.items[*].status.addresses[?(@.type=="Hostname")].address}`)
	if err != nil {
		return nil, fmt.Errorf("failed to get Windows node hostnames: %w", err)
	}
	if names == "" {
		return []string{}, nil
	}
	return strings.Split(names, " "), nil
}

// getKubeletVersionWithRetry fetches the kubelet version for nodes matching the given label.
func getKubeletVersionWithRetry(oc *cli.CLI, label string) (string, error) {
	for i := 0; i < 5; i++ {
		version, err := oc.AsAdmin().Output("get", "nodes",
			"-l="+label,
			"-o=jsonpath={.items[0].status.nodeInfo.kubeletVersion}")
		if err == nil && version != "" {
			return version, nil
		}
		time.Sleep(5 * time.Second)
	}
	return "", fmt.Errorf("failed to get kubelet version for label %s after retries", label)
}

// getWMCOVersionFromLogs returns the WMCO version string from operator logs.
func getWMCOVersionFromLogs(oc *cli.CLI) (string, error) {
	log, err := oc.AsAdmin().Output("logs",
		"deployment.apps/windows-machine-config-operator",
		"-n", wmcoNamespace)
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

// matchKubeletVersion returns true if the two kubelet versions are compatible
// based on the current WMCO version.
func matchKubeletVersion(oc *cli.CLI, version1, version2 string) (bool, error) {
	v1Parts := strings.Split(strings.Split(strings.TrimPrefix(version1, "v"), "+")[0], ".")
	v2Parts := strings.Split(strings.Split(strings.TrimPrefix(version2, "v"), "+")[0], ".")
	if len(v1Parts) < 3 || len(v2Parts) < 3 {
		return false, nil
	}
	wmcoVersion, err := getWMCOVersionFromLogs(oc)
	if err != nil {
		return false, err
	}
	if strings.HasSuffix(strings.Split(wmcoVersion, "-")[0], ".0.0") {
		return v1Parts[0] == v2Parts[0] && v1Parts[1] == v2Parts[1] && v1Parts[2] == v2Parts[2], nil
	}
	return v1Parts[0] == v2Parts[0] && v1Parts[1] == v2Parts[1], nil
}

// checkVersionAnnotationReady returns true if the version annotation is set on a Windows node.
func checkVersionAnnotationReady(oc *cli.CLI, nodeName string) (bool, error) {
	msg, err := oc.AsAdmin().Output("get", "nodes", nodeName,
		`-o=jsonpath={.metadata.annotations.windowsmachineconfig\.openshift\.io/version}`)
	if err != nil {
		return false, err
	}
	return msg != "", nil
}

// getSSHPublicKey reads the SSH public key from the path in KUBE_SSH_KEY_PATH env var
// (appending .pub), or falls back to ~/.ssh/id_rsa.pub.
func getSSHPublicKey() (string, error) {
	if keyPath := os.Getenv("KUBE_SSH_KEY_PATH"); keyPath != "" {
		data, err := os.ReadFile(keyPath + ".pub")
		if err == nil {
			return string(data), nil
		}
	}
	defaultPath := filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub")
	data, err := os.ReadFile(defaultPath)
	if err != nil {
		return "", fmt.Errorf("SSH public key not found: set KUBE_SSH_KEY_PATH or ensure ~/.ssh/id_rsa.pub exists: %w", err)
	}
	return string(data), nil
}

// pollUntil polls fn until it returns true or times out.
func pollUntil(interval, timeout time.Duration, fn func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(interval)
	}
	return fmt.Errorf("timed out after %v", timeout)
}

// CheckWmcoHostNetwork verifies that the WMCO pod can reach Windows nodes via SSH,
// confirming WMCO runs with HostNetwork access. Corresponds to OCP-32554.
func CheckWmcoHostNetwork(_ context.Context, oc *cli.CLI) error {
	ips, err := getWindowsInternalIPs(oc)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no Windows nodes found")
	}
	curlDest := ips[0] + ":22"
	// curl --http0.9 to an SSH port returns the SSH banner but exits non-zero.
	// Ignore the error and check stdout for the banner.
	msg, _, _ := oc.AsAdmin().Run("exec",
		"-n", wmcoNamespace,
		"deployment.apps/windows-machine-config-operator",
		"--", "curl", "--http0.9", curlDest)
	if !strings.Contains(msg, "SSH-2.0-OpenSSH") {
		return fmt.Errorf("WMCO pod cannot reach Windows node SSH, got: %s", msg)
	}
	return nil
}

// CheckGenerateUserDataSecret verifies that WMCO correctly generates and maintains
// the windows-user-data secret. Corresponds to OCP-32615.
func CheckGenerateUserDataSecret(_ context.Context, oc *cli.CLI) error {
	pubKeyContent, err := getSSHPublicKey()
	if err != nil {
		return err
	}
	// Extract the key content (middle field of "TYPE KEY COMMENT")
	keyFields := strings.Fields(pubKeyContent)
	if len(keyFields) < 2 {
		return fmt.Errorf("unexpected public key format")
	}
	keyContent := keyFields[1]

	// Check that the secret contains the public key.
	encoded, err := oc.AsAdmin().Output("get", "secret", "windows-user-data",
		"-n", mcoNamespace, "-o=jsonpath={.data.userData}")
	if err != nil {
		return fmt.Errorf("failed to get windows-user-data secret: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("failed to decode userData: %w", err)
	}
	if !strings.Contains(string(decoded), keyContent) {
		return fmt.Errorf("public key not found in windows-user-data secret")
	}

	// Delete the secret and verify WMCO recreates it.
	if _, err := oc.AsAdmin().Output("delete", "secret", "windows-user-data",
		"-n", mcoNamespace); err != nil {
		return fmt.Errorf("failed to delete windows-user-data: %w", err)
	}
	if err := pollUntil(10*time.Second, 60*time.Second, func() (bool, error) {
		msg, err := oc.AsAdmin().Output("get", "secret", "-n", mcoNamespace)
		if err != nil {
			return false, nil
		}
		if !strings.Contains(msg, "windows-user-data") {
			return false, nil
		}
		enc, err := oc.AsAdmin().Output("get", "secret", "windows-user-data",
			"-o=jsonpath={.data.userData}", "-n", mcoNamespace)
		if err != nil {
			return false, nil
		}
		dec, _ := base64.StdEncoding.DecodeString(enc)
		return strings.Contains(string(dec), keyContent), nil
	}); err != nil {
		return fmt.Errorf("windows-user-data not recreated with correct key: %w", err)
	}

	// Corrupt the secret and verify WMCO restores it.
	if _, err := oc.AsAdmin().Output("patch", "secret", "windows-user-data",
		"-p", `{"data":{"userData":"aW52YWxpZAo="}}`, "-n", mcoNamespace); err != nil {
		return fmt.Errorf("failed to patch windows-user-data: %w", err)
	}
	if err := pollUntil(5*time.Second, 60*time.Second, func() (bool, error) {
		enc, err := oc.AsAdmin().Output("get", "secret", "windows-user-data",
			"-o=jsonpath={.data.userData}", "-n", mcoNamespace)
		if err != nil {
			return false, nil
		}
		dec, _ := base64.StdEncoding.DecodeString(enc)
		return strings.Contains(string(dec), keyContent), nil
	}); err != nil {
		return fmt.Errorf("windows-user-data not restored after corruption: %w", err)
	}
	return nil
}

// CheckWindowsNodeBasic verifies basic Windows node properties:
// kubelet version parity with Linux, worker label, version annotation,
// and required payload binaries in the WMCO pod. Corresponds to OCP-33612.
// Note: SSH-based Windows service checks are out of scope for OTE standalone mode.
func CheckWindowsNodeBasic(_ context.Context, oc *cli.CLI) error {
	// Check kubelet version matches between Windows and Linux nodes.
	linuxVer, err := getKubeletVersionWithRetry(oc, linuxNodeLabel)
	if err != nil {
		return fmt.Errorf("failed to get Linux kubelet version: %w", err)
	}
	windowsVer, err := getKubeletVersionWithRetry(oc, windowsNodeLabel)
	if err != nil {
		return fmt.Errorf("failed to get Windows kubelet version: %w", err)
	}
	match, err := matchKubeletVersion(oc, linuxVer, windowsVer)
	if err != nil {
		return fmt.Errorf("kubelet version check failed: %w", err)
	}
	if !match {
		return fmt.Errorf("kubelet version mismatch: Linux=%s Windows=%s", linuxVer, windowsVer)
	}

	// Check worker label on Windows nodes.
	nodes, err := oc.AsAdmin().Output("get", "nodes", "--no-headers", "-l=kubernetes.io/os=windows")
	if err != nil {
		return fmt.Errorf("failed to get Windows nodes: %w", err)
	}
	for _, node := range strings.Split(nodes, "\n") {
		if node == "" {
			continue
		}
		if !strings.Contains(node, "worker") {
			return fmt.Errorf("worker label not found on Windows node: %s", node)
		}
	}

	// Check version annotation on each Windows node.
	hostnames, err := getWindowsHostNames(oc)
	if err != nil {
		return err
	}
	for _, host := range hostnames {
		ready, err := checkVersionAnnotationReady(oc, host)
		if err != nil {
			return fmt.Errorf("error checking version annotation on %s: %w", host, err)
		}
		if !ready {
			return fmt.Errorf("version annotation not set on Windows node %s", host)
		}
	}

	// Check required payload binaries in the WMCO operator pod.
	type folderCheck struct {
		folder   string
		expected string
	}
	checks := []folderCheck{
		{"/payload", "azure-cloud-node-manager.exe.tar.gz cni containerd csi-proxy ecr-credential-provider.exe.tar.gz generated hybrid-overlay-node.exe.tar.gz kube-node powershell sha256sum windows-exporter windows-instance-config-daemon.exe.tar.gz"},
		{"/payload/containerd", "containerd-shim-runhcs-v1.exe.tar.gz containerd.exe.tar.gz containerd_conf.toml.tar.gz"},
		{"/payload/cni", "host-local.exe.tar.gz win-bridge.exe.tar.gz win-overlay.exe.tar.gz"},
		{"/payload/kube-node", "kube-log-runner.exe.tar.gz kube-proxy.exe.tar.gz kubelet.exe.tar.gz"},
		{"/payload/powershell", "gcp-get-hostname.ps1.tar.gz hns.psm1.tar.gz windows-defender-exclusion.ps1.tar.gz"},
		{"/payload/generated", "network-conf.ps1.tar.gz"},
	}
	for _, c := range checks {
		msg, err := oc.AsAdmin().Output("exec",
			"-n", wmcoNamespace,
			"deployment.apps/windows-machine-config-operator",
			"--", "ls", c.folder)
		if err != nil {
			return fmt.Errorf("failed to list %s: %w", c.folder, err)
		}
		actual := strings.ReplaceAll(msg, "\n", " ")
		if actual != c.expected {
			return fmt.Errorf("payload mismatch in %s:\n  expected: %s\n  actual:   %s",
				c.folder, c.expected, actual)
		}
	}

	return nil
}
