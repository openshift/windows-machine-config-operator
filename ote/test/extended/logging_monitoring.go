package extended

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

const windowsLabelOS = "kubernetes.io/os=windows"

// windowsHostnames returns the hostnames of all Windows worker nodes.
func windowsHostnames(oc *cli.CLI) ([]string, error) {
	names, err := oc.AsAdmin().Output("get", "nodes",
		"-l", windowsLabelOS,
		`-o=jsonpath={.items[*].status.addresses[?(@.type=="Hostname")].address}`)
	if err != nil {
		return nil, fmt.Errorf("failed to get Windows node hostnames: %w", err)
	}
	if names == "" {
		return []string{}, nil
	}
	return strings.Split(names, " "), nil
}

// CheckWindowsNodeLogs verifies that cluster-admin can retrieve Windows service logs
// via oc adm node-logs. Corresponds to OCP-33779.
func CheckWindowsNodeLogs(_ context.Context, oc *cli.CLI) error {
	hosts, err := windowsHostnames(oc)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no Windows nodes found")
	}

	for _, host := range hosts {
		msg, err := oc.AsAdmin().Output("adm", "node-logs", host, "--path=kubelet/kubelet.log")
		if err != nil {
			return fmt.Errorf("failed to retrieve kubelet log on %s: %w", host, err)
		}
		if !strings.Contains(msg, "Running kubelet as a Windows service!") {
			return fmt.Errorf("unexpected kubelet log on %s: %s", host, msg[:minInt(len(msg), 200)])
		}
	}

	for _, host := range hosts {
		msg, err := oc.AsAdmin().Output("adm", "node-logs", host, "--path=kube-proxy/kube-proxy.log")
		if err != nil {
			return fmt.Errorf("failed to retrieve kube-proxy log on %s: %w", host, err)
		}
		if !strings.Contains(msg, "Running kube-proxy as a Windows service!") {
			return fmt.Errorf("unexpected kube-proxy log on %s: %s", host, msg[:minInt(len(msg), 200)])
		}
	}

	msg, err := oc.AsAdmin().Output("adm", "node-logs", "-l", windowsLabelOS, "--path=hybrid-overlay/hybrid-overlay.log")
	if err != nil {
		return fmt.Errorf("failed to retrieve hybrid-overlay logs: %w", err)
	}
	for _, host := range hosts {
		if !strings.Contains(msg, host) {
			return fmt.Errorf("hybrid-overlay log missing entry for node %s", host)
		}
	}

	msg, err = oc.AsAdmin().Output("adm", "node-logs", "-l", windowsLabelOS, "--path=containerd/containerd.log")
	if err != nil {
		return fmt.Errorf("failed to retrieve containerd logs: %w", err)
	}
	if !strings.Contains(msg, "starting containerd") {
		return fmt.Errorf("unexpected containerd log: %s", msg[:minInt(len(msg), 200)])
	}

	msg, err = oc.AsAdmin().Output("adm", "node-logs", "-l", windowsLabelOS,
		"--path=wicd/windows-instance-config-daemon.exe.INFO")
	if err != nil {
		return fmt.Errorf("failed to retrieve wicd logs: %w", err)
	}
	for _, host := range hosts {
		if !strings.Contains(msg, host+" Log file created at:") {
			return fmt.Errorf("wicd log missing entry for node %s", host)
		}
	}

	msg, err = oc.AsAdmin().Output("adm", "node-logs", "-l", windowsLabelOS, "--path=csi-proxy/csi-proxy.log")
	if err != nil {
		return fmt.Errorf("failed to retrieve csi-proxy logs: %w", err)
	}
	for _, host := range hosts {
		if !strings.Contains(msg, host+" Log file created at:") {
			return fmt.Errorf("csi-proxy log missing entry for node %s", host)
		}
	}

	return nil
}

// CheckMustGather verifies that oc adm must-gather collects Windows node logs.
// Corresponds to OCP-33783.
func CheckMustGather(_ context.Context, oc *cli.CLI) error {
	destDir, err := os.MkdirTemp("", "must-gather-33783-*")
	if err != nil {
		return fmt.Errorf("failed to create temp must-gather dir: %w", err)
	}
	defer os.RemoveAll(destDir)

	msg, err := oc.AsAdmin().Output("adm", "must-gather", "--dest-dir="+destDir)
	if err != nil {
		return fmt.Errorf("must-gather failed: %w", err)
	}
	required := []string{
		"host_service_logs/windows/",
		"host_service_logs/windows/log_files/",
		"host_service_logs/windows/log_files/hybrid-overlay/hybrid-overlay.log",
		"host_service_logs/windows/log_files/kube-proxy/kube-proxy.log",
		"host_service_logs/windows/log_files/kubelet/kubelet.log",
		"host_service_logs/windows/log_files/containerd/containerd.log",
		"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.ERROR",
		"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.INFO",
		"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.WARNING",
		"host_service_logs/windows/log_files/csi-proxy/csi-proxy.log",
	}
	for _, v := range required {
		if !strings.Contains(msg, v) {
			return fmt.Errorf("must-gather output missing: %s", v)
		}
	}
	return nil
}

// CheckWindowsNodeFilesystemGraphs verifies that Windows nodes report positive
// ephemeral storage allocatable values. Corresponds to OCP-73595.
func CheckWindowsNodeFilesystemGraphs(_ context.Context, oc *cli.CLI) error {
	hosts, err := windowsHostnames(oc)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("no Windows nodes found")
	}
	for _, host := range hosts {
		storage, err := oc.AsAdmin().Output("get", "node", host,
			"-o=jsonpath={.status.allocatable['ephemeral-storage']}")
		if err != nil {
			return fmt.Errorf("failed to get ephemeral storage for node %s: %w", host, err)
		}
		val, err := strconv.ParseInt(strings.TrimSuffix(storage, "Ki"), 10, 64)
		if err != nil {
			return fmt.Errorf("failed to parse storage value %q for node %s: %w", storage, host, err)
		}
		if val <= 0 {
			return fmt.Errorf("expected positive storage value for node %s, got %d", host, val)
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
