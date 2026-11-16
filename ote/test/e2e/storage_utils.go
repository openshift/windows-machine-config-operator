package winc

import (
	"fmt"
	"strings"
	"time"

	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// csiDriverNamespace is the namespace the cluster CSI operator installs node drivers into.
const csiDriverNamespace = "openshift-cluster-csi-drivers"

// generatePVCYAML returns a PersistentVolumeClaim manifest.
func generatePVCYAML(name, namespace, storageClass, accessMode, size string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  storageClassName: %s
  accessModes:
  - %s
  resources:
    requests:
      storage: %s
`, name, namespace, storageClass, accessMode, size)
}

// generateStorageClassYAML returns a StorageClass backed by the platform's CSI provisioner.
func generateStorageClassYAML(platform, name string) (string, error) {
	var provisioner string
	switch platform {
	case "azure":
		provisioner = "file.csi.azure.com"
	case "vsphere":
		provisioner = "csi.vsphere.vmware.com"
	default:
		return "", fmt.Errorf("no Windows CSI storage provisioner for platform %s", platform)
	}
	return fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: %s
provisioner: %s
parameters:
  csi.storage.k8s.io/fstype: "ntfs"
`, name, provisioner), nil
}

// windowsCSIDriverName returns the Windows CSI node DaemonSet name for the platform.
func windowsCSIDriverName(platform string) (string, error) {
	switch platform {
	case "azure":
		return "azure-file-csi-driver-node-windows", nil
	case "vsphere":
		return "vmware-vsphere-csi-driver-node-windows", nil
	}
	return "", fmt.Errorf("no Windows CSI node driver for platform %s", platform)
}

// windowsCSIDriverManifest returns the Windows CSI node driver DaemonSet for the platform. The
// manifest is generated from hack/manifests/csi by ote/hack/gen-csi-manifests.sh.
func windowsCSIDriverManifest(platform string) (string, error) {
	manifest, ok := csiDriverManifests[platform]
	if !ok {
		return "", fmt.Errorf("no Windows CSI node driver for platform %s", platform)
	}
	return manifest, nil
}

// installWindowsCSIDriver installs the Windows CSI node driver for the platform and waits until
// it runs on every Windows node.
func installWindowsCSIDriver(oc *exutil.CLI, platform string) error {
	driverName, err := windowsCSIDriverName(platform)
	if err != nil {
		return err
	}
	manifest, err := windowsCSIDriverManifest(platform)
	if err != nil {
		return err
	}
	if err := createResourceFromString(oc, csiDriverNamespace, manifest); err != nil {
		return fmt.Errorf("failed to apply the Windows CSI driver manifest: %w", err)
	}

	// The DaemonSet selects Windows nodes only, so it must become ready on each of them.
	expectedReplicas := len(getWindowsHostNames(oc))
	const timeout = 5 * time.Minute
	pollErr := wait.Poll(10*time.Second, timeout, func() (bool, error) {
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"daemonset", driverName, "-n", csiDriverNamespace,
			"-o=jsonpath={.status.numberReady}").Output()
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(out) == fmt.Sprintf("%d", expectedReplicas), nil
	})
	compat_otp.AssertWaitPollNoErr(pollErr,
		fmt.Sprintf("Windows CSI driver %s is not ready on all %d Windows nodes after %v", driverName, expectedReplicas, timeout))
	return nil
}

// uninstallWindowsCSIDriver removes the Windows CSI node driver installed by
// installWindowsCSIDriver.
func uninstallWindowsCSIDriver(oc *exutil.CLI, platform string) error {
	driverName, err := windowsCSIDriverName(platform)
	if err != nil {
		return err
	}
	if err := deleteResourceAndWait(oc, "daemonset", driverName, csiDriverNamespace); err != nil {
		return fmt.Errorf("failed to delete the Windows CSI driver DaemonSet %s: %w", driverName, err)
	}
	return nil
}

// waitForWebContent polls url from podName until the response contains expected content.
func waitForWebContent(oc *exutil.CLI, namespace, podName, url, expected, desc string) error {
	return wait.Poll(15*time.Second, 3*time.Minute, func() (bool, error) {
		out, err := oc.AsAdmin().WithoutNamespace().Run("exec").Args(
			"-n", namespace, podName, "--", "pwsh.exe", "-Command", buildInvokeWebRequestCommand(url)).Output()
		if err != nil || !strings.Contains(out, expected) {
			e2e.Logf("%s, retrying...", desc)
			return false, nil
		}
		return true, nil
	})
}
