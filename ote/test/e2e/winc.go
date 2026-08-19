package winc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/tidwall/gjson"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[OTP][sig-windows] Windows_Containers", func() {
	defer g.GinkgoRecover()

	oc := compat_otp.NewCLIWithoutNamespace("default")

	g.BeforeEach(func() {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		iaasPlatform = strings.ToLower(output)
	})

	// --- Migrated tests go below this line (added via Mode 2) ---

	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Critical-33612-Windows node basic check", func() {
		g.By("Check Windows worker nodes run the same kubelet version as other Linux worker nodes")
		linuxKubeletVersion, err := getKubeletVersionWithRetry(oc, linuxNodeLabel)
		o.Expect(err).NotTo(o.HaveOccurred())
		windowsKubeletVersion, err := getKubeletVersionWithRetry(oc, windowsNodeLabel)
		o.Expect(err).NotTo(o.HaveOccurred())

		if !matchKubeletVersion(oc, linuxKubeletVersion, windowsKubeletVersion) {
			e2e.Failf("failed to check Windows %s and Linux %s kubelet version should be the same", windowsKubeletVersion, linuxKubeletVersion)
		}

		g.By("Check worker label is applied to Windows nodes")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "--no-headers", "-l=kubernetes.io/os=windows").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, node := range strings.Split(msg, "\n") {
			if node == "" {
				continue
			}
			if !strings.Contains(node, "worker") {
				e2e.Failf("Failed to check worker label is applied to Windows node %s", node)
			}
		}

		g.By("Check version annotation is applied to Windows nodes")
		// Note: Case 33536 also is covered
		windowsHostName := getWindowsHostNames(oc)
		for _, host := range windowsHostName {
			retcode, err := checkVersionAnnotationReady(oc, host)
			o.Expect(err).NotTo(o.HaveOccurred())
			if !retcode {
				e2e.Failf("Failed to check version annotation is applied to Windows node %s", host)
			}
		}

		g.By("Check dockerfile prepare required binaries in operator image")
		checkFolders := []struct {
			folder   string
			expected string
		}{
			{
				folder:   "/payload",
				expected: "azure-cloud-node-manager.exe.tar.gz cni containerd csi-proxy ecr-credential-provider.exe.tar.gz generated hybrid-overlay-node.exe.tar.gz kube-node powershell sha256sum windows-exporter windows-instance-config-daemon.exe.tar.gz",
			},
			{
				folder:   "/payload/containerd",
				expected: "containerd-shim-runhcs-v1.exe.tar.gz containerd.exe.tar.gz containerd_conf.toml.tar.gz",
			},
			{
				folder:   "/payload/cni",
				expected: "host-local.exe.tar.gz win-bridge.exe.tar.gz win-overlay.exe.tar.gz",
			},
			{
				folder:   "/payload/kube-node",
				expected: "kube-log-runner.exe.tar.gz kube-proxy.exe.tar.gz kubelet.exe.tar.gz",
			},
			{
				folder:   "/payload/powershell",
				expected: "gcp-get-hostname.ps1.tar.gz hns.psm1.tar.gz windows-defender-exclusion.ps1.tar.gz",
			},
			{
				folder:   "/payload/generated",
				expected: "network-conf.ps1.tar.gz",
			},
		}
		for _, checkFolder := range checkFolders {
			g.By("Check required files in" + checkFolder.folder)
			command := []string{"exec", "-n", wmcoNamespace, wmcoDeployment, "--", "ls", checkFolder.folder}
			msg, err := oc.AsAdmin().WithoutNamespace().Run(command...).Args().Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			actual := strings.ReplaceAll(msg, "\n", " ")
			if actual != checkFolder.expected {
				e2e.Failf("Failed to check required files in %s, expected: %s actual: %s", checkFolder.folder, checkFolder.expected, actual)
			}
		}

	})

	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Critical-32615-Generate userData secret [Serial]", func() {
		g.By("Derive public key from cloud-private-key cluster secret")
		publicKeyContent := derivePublicKeyFromSecret(oc)

		g.By("Check secret windows-user-data generated and contain correct public key")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", "windows-user-data", "-n", mcoNamespace, "-o=jsonpath={.data.userData}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		decodedUserData, err := base64.StdEncoding.DecodeString(msg)
		o.Expect(err).NotTo(o.HaveOccurred())
		if !strings.Contains(string(decodedUserData), publicKeyContent) {
			e2e.Failf("Public key not found in windows-user-data secret (decoded length: %d bytes)", len(decodedUserData))
		}

		g.By("Verify windows-user-data secret also exists in CAPI namespace with identical content (OCPBUGS-38401)")
		_, capiNsErr := oc.AsAdmin().WithoutNamespace().Run("get").Args("namespace", capiNamespace).Output()
		if capiNsErr != nil {
			e2e.Logf("Namespace %s does not exist, skipping CAPI secret verification", capiNamespace)
		} else {
			capiUserData, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", "windows-user-data", "-n", capiNamespace, "-o=jsonpath={.data.userData}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			if capiUserData == "" {
				e2e.Failf("windows-user-data secret not found in namespace %s", capiNamespace)
			}
			if msg != capiUserData {
				e2e.Failf("windows-user-data secrets differ between namespaces %s and %s (base64 content mismatch)",
					mcoNamespace, capiNamespace)
			}
			e2e.Logf("Successfully verified windows-user-data secret exists and is identical in both %s and %s namespaces", mcoNamespace, capiNamespace)
		}

		g.By("Check delete secret windows-user-data")
		_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("secret", "windows-user-data", "-n", mcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		pollErr := wait.Poll(10*time.Second, 60*time.Second, func() (bool, error) {
			msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", "-n", mcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			if !strings.Contains(msg, "windows-user-data") {
				e2e.Logf("Secret windows-user-data does not exist yet and wait up to 1 minute ...")
				return false, nil
			}
			e2e.Logf("Secret windows-user-data exist now")
			msg, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", "windows-user-data", "-o=jsonpath={.data.userData}", "-n", mcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			decodedUserData, decErr := base64.StdEncoding.DecodeString(msg)
			o.Expect(decErr).NotTo(o.HaveOccurred())
			if !strings.Contains(string(decodedUserData), publicKeyContent) {
				e2e.Failf("Public key not found in recreated windows-user-data secret (decoded length: %d bytes)", len(decodedUserData))
			}
			return true, nil
		})
		if pollErr != nil {
			e2e.Failf("Secret windows-user-data does not exist after waiting up to 1 minutes ...")
		}
		g.By("Check update secret windows-user-data")
		_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("secret", "windows-user-data", "-p", `{"data":{"userData":"aW52YWxpZAo="}}`, "-n", mcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		pollErr = wait.Poll(5*time.Second, 60*time.Second, func() (bool, error) {
			msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("secret", "windows-user-data", "-o=jsonpath={.data.userData}", "-n", mcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			decodedUserData, decErr := base64.StdEncoding.DecodeString(msg)
			o.Expect(decErr).NotTo(o.HaveOccurred())
			if !strings.Contains(string(decodedUserData), publicKeyContent) {
				e2e.Logf("Secret windows-user-data is not updated yet and wait up to 1 minute ...")
				return false, nil
			}
			e2e.Logf("Secret windows-user-data is updated")
			return true, nil
		})
		if pollErr != nil {
			e2e.Failf("Secret windows-user-data is not updated after waiting up to 1 minutes ...")
		}
	})

	// author: sgao@redhat.com
	g.It("Author:sgao-Smokerun-Low-32554-wmco run in a pod with HostNetwork", func() {
		winInternalIPs := getWindowsInternalIPs(oc)
		if len(winInternalIPs) == 0 {
			e2e.Failf("No Windows nodes with InternalIP found")
		}
		curlDest := net.JoinHostPort(winInternalIPs[0], "22")
		msg, err := execInPod(oc, wmcoNamespace, wmcoDeployment, "curl", "--http0.9", curlDest)
		if err != nil {
			e2e.Logf("execInPod error (may be expected for SSH banner): %v", err)
		}
		if !strings.Contains(msg, "SSH-2.0-OpenSSH") {
			e2e.Failf("Failed to check WMCO run in a pod with HostNetwork: %s", msg)
		}
	})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-Medium-37362-[wmco] wmco using correct golang version", func() {
		g.By("Fetch the correct golang version")
		getCMD := "oc version -ojson | jq '.serverVersion.goVersion'"
		goVersion, err := exec.Command("bash", "-c", getCMD).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to get server go version")
		s := string(goVersion)
		tVersion := truncatedVersion(s)
		e2e.Logf("Golang version is: %s", s)
		e2e.Logf("Golang version truncated is: %s", tVersion)
		g.By("Compare fetched version with WMCO log version")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args(wmcoDeployment, "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		if !strings.Contains(msg, tVersion) {
			e2e.Failf("Golang version mismatch: expected WMCO logs to contain %s", tVersion)
		}
	})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-High-89616-Verify log rotation for kubelet and kube-proxy services [Slow][Disruptive]", func() {
		winNodeCount, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"nodes", "-l", windowsNodeLabel, "--no-headers").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		expectedNodes := len(strings.Split(strings.TrimSpace(winNodeCount), "\n"))

		g.By("Set log rotation env vars on WMCO deployment")
		err = oc.AsAdmin().WithoutNamespace().Run("set").Args(
			"env", wmcoDeployment, "-n", wmcoNamespace,
			"SERVICES_LOG_FILE_SIZE=1M",
			"SERVICES_LOG_FILE_AGE=168h",
			"SERVICES_LOG_FLUSH_INTERVAL=5s").Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		defer func() {
			g.By("Cleanup: remove log rotation env vars from WMCO deployment")
			cleanupErr := oc.AsAdmin().WithoutNamespace().Run("set").Args(
				"env", wmcoDeployment, "-n", wmcoNamespace,
				"SERVICES_LOG_FILE_SIZE-",
				"SERVICES_LOG_FILE_AGE-",
				"SERVICES_LOG_FLUSH_INTERVAL-").Execute()
			o.Expect(cleanupErr).NotTo(o.HaveOccurred(), "failed to restore WMCO deployment configuration")
			waitWindowsNodesReady(oc, expectedNodes, 15*time.Minute)
		}()

		g.By("Wait for WMCO to reconcile and Windows nodes to be reconfigured")
		err = waitForDeploymentReady(oc, wmcoDeploymentName, wmcoNamespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred(), "WMCO deployment did not become ready after env var update")
		waitWindowsNodesReady(oc, expectedNodes, 15*time.Minute)

		g.By("Wait for services ConfigMap to be updated with log rotation configuration")
		err = wait.Poll(5*time.Second, 3*time.Minute, func() (bool, error) {
			_, pollErr := getLatestServicesCMData(oc)
			return pollErr == nil, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "services ConfigMap was not updated after setting log rotation env vars")

		g.By("Get the services ConfigMap to verify log rotation configuration")
		servicesCMData, err := getLatestServicesCMData(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		for _, svcName := range []string{"kubelet", "kube-proxy"} {
			g.By(fmt.Sprintf("Verify %s service command includes kube-log-runner", svcName))
			svcCmd := getServiceCommand(servicesCMData, svcName)
			o.Expect(svcCmd).NotTo(o.BeEmpty(), "%s service not found in services ConfigMap", svcName)
			o.Expect(svcCmd).Should(o.ContainSubstring("kube-log-runner"),
				"%s should be wrapped with kube-log-runner", svcName)
			o.Expect(svcCmd).Should(o.ContainSubstring("-log-file="),
				"%s should have -log-file flag", svcName)
			e2e.Logf("%s service command: %s", svcName, svcCmd)
		}
	})

	// author: jfrancoa@redhat.com
	g.It("Smokerun-Author:jfrancoa-Medium-38188-Get Windows instance/core number and CPU arch", func() {
		winMetrics := []string{"cluster:node_instance_type_count:sum", "cluster:capacity_cpu_cores:sum"}

		mon, err := compat_otp.NewPrometheusMonitor(oc.AsAdmin())
		o.Expect(err).NotTo(o.HaveOccurred(),
			"Error creating new thanos monitor")

		for _, metricQuery := range winMetrics {
			g.By(fmt.Sprintf("Check that the metric %s is exposed to telemetry", metricQuery))

			expectedExposedMetric := fmt.Sprintf(`{__name__=\"%s\"}`, metricQuery)
			telemetryConfig, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", "-n", "openshift-monitoring", "telemetry-config", "-o=jsonpath={.data}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(telemetryConfig).To(o.ContainSubstring(expectedExposedMetric),
				"Metric %s, is not exposed to telemetry", metricQuery)

			g.By(fmt.Sprintf("Verify the metric %s displays the right value", metricQuery))

			queryResult, err := mon.SimpleQuery(metricQuery + "{label_node_openshift_io_os_id=\"Windows\"}")
			o.Expect(err).NotTo(o.HaveOccurred(),
				"Error querying metric: %s", metricQuery)
			metricValue := extractMetricValue(queryResult)

			valueFromCluster := getMetricsFromCluster(oc, metricQuery)

			e2e.Logf("Query %s value: %s", metricQuery, metricValue)
			o.Expect(metricValue).Should(o.Equal(valueFromCluster),
				"Prometheus metric %s does not match the value %s obtained from the cluster", metricValue, valueFromCluster)
		}
	})

	// author: sgao@redhat.com
	g.It("Author:sgao-Smokerun-Medium-33768-NodeWithoutOVNKubeNodePodRunning alert ignore Windows nodes", func() {
		g.By("Check NodeWithoutOVNKubeNodePodRunning alert ignore Windows nodes")
		prometheusPod, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-n", "openshift-monitoring", "-l=app.kubernetes.io/name=prometheus", "-o", "jsonpath={.items[0].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		getAlertCMD, err := execInPod(oc, "openshift-monitoring", prometheusPod, "curl", "localhost:9090/api/v1/rules")
		o.Expect(err).NotTo(o.HaveOccurred())
		if !strings.Contains(string(getAlertCMD), "kube_node_labels{label_kubernetes_io_os=\\\"windows\\\"}") {
			e2e.Failf("Failed to check NodeWithoutOVNKubeNodePodRunning alert ignore Windows nodes")
		}
	})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-Medium-60814-Check containerd version is properly reported", func() {
		wmcoVersion, err := getWMCOVersionFromLogs(oc)
		o.Expect(err).NotTo(o.HaveOccurred())

		if strings.HasSuffix(wmcoVersion, "-dirty") {
			g.Skip("WMCO PR build detected, commit hash not available on upstream GitHub")
		}

		parts := strings.Split(wmcoVersion, "-")
		o.Expect(len(parts)).Should(o.BeNumerically(">", 1), "unexpected WMCO version format")
		versionHash := parts[1]
		resp, err := http.Get("https://raw.githubusercontent.com/openshift/windows-machine-config-operator/" + versionHash + "/Makefile")
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to fetch Makefile from GitHub")
		defer resp.Body.Close()
		o.Expect(resp.StatusCode).To(o.Equal(http.StatusOK), "failed to fetch Makefile from GitHub (HTTP %d)", resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to read Makefile response body")

		submoduleContainerdVersion := getValueFromText(body, "CONTAINERD_GIT_VERSION=")
		o.Expect(submoduleContainerdVersion).NotTo(o.BeEmpty(), "CONTAINERD_GIT_VERSION not found in Makefile")
		for _, winhost := range getWindowsHostNames(oc) {
			if strings.Compare(submoduleContainerdVersion, getContainerdVersion(oc, winhost)) != 0 {
				e2e.Failf("Containerd version mismatch expected %s actual %s", submoduleContainerdVersion, getContainerdVersion(oc, winhost))
			}
		}
	})

	// author: weinliu@redhat.com
	g.It("Author:weinliu-Smokerun-High-77777-Verify metrics configuration and HTTPS endpoint [Serial]", func() {
		g.By("Verifying ServiceMonitor existence")
		serviceMonitorName := "windows-exporter"

		output, err := oc.AsAdmin().Run("get").Args("servicemonitor", serviceMonitorName, "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(output).To(o.ContainSubstring(serviceMonitorName), fmt.Sprintf("ServiceMonitor %v not found", serviceMonitorName))

		g.By("Verifying namespace selector configuration")
		output, err = oc.AsAdmin().Run("get").Args("servicemonitor", serviceMonitorName, "-n", wmcoNamespace, "-o", "yaml").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get ServiceMonitor YAML configuration")
		o.Expect(output).To(o.ContainSubstring("namespaceSelector:"), "Namespace selector not found in ServiceMonitor configuration")
		o.Expect(output).To(o.ContainSubstring("matchNames:"), "matchNames field not found in namespace selector configuration")
		o.Expect(output).To(o.ContainSubstring("- kube-system"), "kube-system namespace not found in matchNames list")

		g.By("Verifying windows-exporter service port configuration")
		svcOutput, err := oc.AsAdmin().Run("get").Args("svc", "windows-exporter", "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(svcOutput).To(o.ContainSubstring("9182/TCP"), "Service port 9182 not found")

		g.By("Verifying WMCO logs mention HTTPS metrics server")
		waitUntilWMCOStatusChanged(oc, "metrics server", "")

		g.By("Verifying HTTP is not allowed on metrics endpoint")
		winInternalIPs := getWindowsInternalIPs(oc)
		if len(winInternalIPs) < 2 {
			g.Skip("Need at least 2 Windows nodes to test cross-node HTTP rejection")
		}
		metricsURL := "http://" + net.JoinHostPort(winInternalIPs[1], "9182") + "/metrics"
		msg, err := execInPod(oc, wmcoNamespace, wmcoDeployment, "curl", "-k", metricsURL)
		if err != nil {
			e2e.Logf("execInPod error (expected for HTTP-to-HTTPS rejection): %v", err)
		}
		o.Expect(msg).To(o.ContainSubstring("Client sent an HTTP request to an HTTPS server"),
			"Expected HTTP request to be rejected with HTTPS requirement message")
	})

	// author: rrasouli@redhat.com
	g.It("Author:rrasouli-Smokerun-Medium-79251-Validate matching provider IDs between Windows nodes and machines", func() {
		if isNone(oc) {
			g.Skip("Platform none does not support Machine API")
		}
		e2e.Logf("Fetching Windows Machines and Nodes provider IDs...")

		windowsMachinesJSON, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args(compat_otp.MapiMachine, "-n", compat_otp.MachineAPINamespace, "-l", machineLabel, "-o=json").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to retrieve Windows Machines JSON")

		windowsNodesJSON, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("nodes", "-l", windowsNodeLabel, "-o=json").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to retrieve Windows Nodes JSON")

		machineProviderIDs, err := extractInstanceID(windowsMachinesJSON, "Windows Machine")
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to process Windows Machines provider IDs")

		nodeProviderIDs, err := extractInstanceID(windowsNodesJSON, "Windows Node")
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to process Windows Nodes provider IDs")

		e2e.Logf("Final Windows Machine provider IDs: %v", machineProviderIDs)
		e2e.Logf("Final Windows Node provider IDs: %v", nodeProviderIDs)

		validatedCount := 0
		for nodeName := range nodeProviderIDs {
			if isBYOH(oc, nodeName) {
				e2e.Logf("Skipping BYOH node %s - no Machine object expected", nodeName)
				continue
			}

			nodeProviderID, exists := nodeProviderIDs[nodeName]
			o.Expect(exists).To(o.BeTrue(), fmt.Sprintf("Node %s does not have a provider ID", nodeName))

			matchingMachineFound := false
			for machineName, machineProviderID := range machineProviderIDs {
				if machineProviderID == nodeProviderID {
					matchingMachineFound = true
					validatedCount++
					e2e.Logf("Machine %s is correctly associated with Node %s (Instance ID: %s)", machineName, nodeName, nodeProviderID)
					break
				}
			}
			o.Expect(matchingMachineFound).To(o.BeTrue(),
				fmt.Sprintf("No matching Machine found for Node %s with Provider ID %s", nodeName, nodeProviderID))
		}

		if validatedCount == 0 {
			g.Skip("All Windows nodes are BYOH - no MachineSet nodes to validate")
		}
	})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-Medium-42204-Create Windows pod with a Projected Volume", func() {
		namespace := "winc-42204"
		defer deleteProject(oc, namespace)
		createProject(oc, namespace)
		username, password := "admin", getRandomString(8)

		g.By("Create username and password secrets")
		_, err := oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "user",
			"--from-literal=username="+username, "-n", namespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic", "pass",
			"--from-literal=password="+password, "-n", namespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Create Windows Pod with Projected Volume")
		podManifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: win-projected-vol
spec:
  containers:
  - name: win-container
    image: %s
    command: ["pwsh", "-Command", "Start-Sleep -Seconds 3600"]
    volumeMounts:
    - name: projected-volume
      mountPath: "C:\\projected-volume"
      readOnly: true
  volumes:
  - name: projected-volume
    projected:
      sources:
      - secret:
          name: user
      - secret:
          name: pass
  nodeSelector:
    kubernetes.io/os: windows
  tolerations:
  - key: "os"
    value: "Windows"
    effect: "NoSchedule"
  os:
    name: windows`, windowsDebugImage)

		err = createResourceFromString(oc, namespace, podManifest)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Wait for pod to be Running")
		pollErr := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
			phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "win-projected-vol",
				"-n", namespace, "-o=jsonpath={.status.phase}").Output()
			if err != nil {
				return false, nil
			}
			return phase == "Running", nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "Pod win-projected-vol did not reach Running state")

		g.By("Verify projected volume contents and secret data")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("exec").Args("win-projected-vol",
			"-n", namespace, "--", "pwsh", "-Command",
			"Get-ChildItem C:\\projected-volume; Get-Content C:\\projected-volume\\username; Get-Content C:\\projected-volume\\password").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(msg).To(o.ContainSubstring(username), "Projected volume should contain username")
		o.Expect(strings.Contains(msg, password)).To(o.BeTrue(),
			"Projected volume should contain the password file content (value redacted)")
		e2e.Logf("Projected volume listing verified on pod win-projected-vol (contents redacted)")
	})

	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Critical-25593-Prevent scheduling non Windows workloads on Windows nodes", func() {
		namespace := "winc-25593"
		defer deleteProject(oc, namespace)
		createProject(oc, namespace)

		g.By("Check Windows node have a taint 'os=Windows:NoSchedule'")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", "-l", windowsNodeLabel,
			"-o=jsonpath={.items[0].spec.taints[0].key}={.items[0].spec.taints[0].value}:{.items[0].spec.taints[0].effect}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(msg).To(o.Equal("os=Windows:NoSchedule"), "Windows node should have taint os=Windows:NoSchedule")

		g.By("Check deployment without tolerations would not land on Windows nodes")
		deployManifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: win-no-toleration
  labels:
    app: win-no-toleration
spec:
  replicas: 1
  selector:
    matchLabels:
      app: win-no-toleration
  template:
    metadata:
      labels:
        app: win-no-toleration
    spec:
      containers:
      - name: win-container
        image: %s
        command: ["pwsh", "-Command", "Start-Sleep -Seconds 3600"]
      nodeSelector:
        kubernetes.io/os: windows
      os:
        name: windows`, windowsDebugImage)

		err = createResourceFromString(oc, namespace, deployManifest)
		o.Expect(err).NotTo(o.HaveOccurred())

		pollErr := wait.Poll(10*time.Second, 60*time.Second, func() (bool, error) {
			msg, _ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pod", "-l=app=win-no-toleration",
				"-o=jsonpath={.items[].status.conditions[].message}", "-n", namespace).Output()
			return strings.Contains(msg, "had untolerated taint"), nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "Deployment without tolerations should not land on Windows nodes")

		g.By("Check none of core/optional operators/operands would land on Windows nodes")
		for _, winHostName := range getWindowsHostNames(oc) {
			e2e.Logf("Check pods running on Windows node: %s", winHostName)
			msg, _ = oc.AsAdmin().WithoutNamespace().Run("get").Args("pods", "--all-namespaces",
				"-o=jsonpath={.items[*].metadata.namespace}", "-l=app=win-no-toleration",
				"--field-selector", "spec.nodeName="+winHostName, "--no-headers").Output()
			for _, ns := range strings.Split(msg, " ") {
				if ns != "" && !strings.Contains(ns, "winc") {
					e2e.Failf("Non-winc pods found running on Windows node %s in namespace %s", winHostName, ns)
				}
			}
		}
	})

	// author: weinliu@redhat.com
	g.It("Author:weinliu-Smokerun-Medium-73752-Monitor Network In, and Network Out graphs for Windows Pods managed by wmco", func() {
		mon, err := compat_otp.NewPrometheusMonitor(oc.AsAdmin())
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating Prometheus monitor")

		g.By("Getting WMCO pods")
		podList, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"pods", "-n", wmcoNamespace,
			"-l", "name=windows-machine-config-operator",
			"-o=jsonpath={.items[*].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		podNames := strings.Fields(podList)
		o.Expect(podNames).NotTo(o.BeEmpty(), "No WMCO pods found")

		networkMetrics := []string{
			"pod:network_receive_bytes_total:sum",
			"pod:network_transmit_bytes_total:sum",
		}

		for _, podName := range podNames {
			for _, metric := range networkMetrics {
				g.By(fmt.Sprintf("Verifying %s for pod %s", metric, podName))
				queryResult, err := mon.SimpleQuery(fmt.Sprintf("%s{pod=\"%s\"}", metric, podName))
				o.Expect(err).NotTo(o.HaveOccurred(), "Error querying %s for pod %s", metric, podName)
				metricValue := extractMetricValue(queryResult)
				e2e.Logf("Pod %s metric %s = %s", podName, metric, metricValue)
			}
		}
	})

	// author: weinliu@redhat.com
	g.It("Author:weinliu-Smokerun-Medium-70922-Monitor CPU, Memory, and Filesystem graphs for Windows Pods managed by wmco", func() {
		mon, err := compat_otp.NewPrometheusMonitor(oc.AsAdmin())
		o.Expect(err).NotTo(o.HaveOccurred(), "Error creating Prometheus monitor")

		g.By("Checking WMCO deployment is ready")
		readyReplicas, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"deployment", "windows-machine-config-operator", "-n", wmcoNamespace,
			"-o=jsonpath={.status.readyReplicas}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(readyReplicas).NotTo(o.BeEmpty(), "WMCO deployment has no ready replicas")

		g.By("Getting WMCO pods")
		podList, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"pods", "-n", wmcoNamespace,
			"-l", "name=windows-machine-config-operator",
			"-o=jsonpath={.items[*].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		podNames := strings.Fields(podList)
		o.Expect(podNames).NotTo(o.BeEmpty(), "No WMCO pods found")

		podMetrics := []string{
			"pod:container_cpu_usage:sum",
			"pod:container_memory_usage_bytes:sum",
			"pod:container_fs_usage_bytes:sum",
		}

		for _, podName := range podNames {
			for _, metric := range podMetrics {
				g.By(fmt.Sprintf("Verifying %s for pod %s", metric, podName))
				queryResult, err := mon.SimpleQuery(fmt.Sprintf("%s{pod=\"%s\"}", metric, podName))
				o.Expect(err).NotTo(o.HaveOccurred(), "Error querying %s for pod %s", metric, podName)
				metricValue := extractMetricValue(queryResult)
				e2e.Logf("Pod %s metric %s = %s", podName, metric, metricValue)
			}
		}

		g.By("Verifying CPU utilisation recording rule reports utilization not idle (OCPBUGS-85061)")
		var cpuMetricValue, cpuLastReason string
		pollErr := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
			queryResult, err := mon.SimpleQuery("instance:node_cpu_utilisation:rate1m{job=\"windows-exporter\"}")
			if err != nil {
				cpuLastReason = fmt.Sprintf("query error: %v", err)
				e2e.Logf("Error querying instance:node_cpu_utilisation:rate1m: %v", err)
				return false, nil
			}
			parsed := gjson.Parse(queryResult)
			if status := parsed.Get("status").String(); status != "success" {
				cpuLastReason = fmt.Sprintf("query status: %s", status)
				e2e.Logf("Query instance:node_cpu_utilisation:rate1m returned status %s, retrying...", status)
				return false, nil
			}
			cpuMetricValue = parsed.Get("data.result.0.value.1").String()
			if cpuMetricValue == "" {
				cpuLastReason = "empty result set"
				e2e.Logf("No result yet for instance:node_cpu_utilisation:rate1m, retrying...")
				return false, nil
			}
			return true, nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(),
			"Timed out waiting for instance:node_cpu_utilisation:rate1m (last reason: %s)", cpuLastReason)

		cpuUtil, convErr := strconv.ParseFloat(cpuMetricValue, 64)
		o.Expect(convErr).NotTo(o.HaveOccurred(), "Failed to parse CPU utilisation value: %s", cpuMetricValue)
		e2e.Logf("instance:node_cpu_utilisation:rate1m = %f (should be utilization, not idle)", cpuUtil)
		o.Expect(cpuUtil).To(o.BeNumerically("<", 0.9),
			"CPU utilisation recording rule value %f suggests it is recording idle rate instead of utilization (OCPBUGS-85061)", cpuUtil)
	})

	// author: rrasouli@redhat.com
	g.It("Author:rrasouli-Smokerun-Critical-84267-Verify hybrid-overlay-node client certificate rotation", func() {
		winInternalIPs := getWindowsInternalIPs(oc)
		o.Expect(len(winInternalIPs)).To(o.BeNumerically(">", 0), "Test requires at least one Windows node")

		for _, winhost := range winInternalIPs {
			nodeName := getNodeNameFromIP(oc, winhost)
			o.Expect(nodeName).NotTo(o.BeEmpty(), "Failed to get node name for IP %s", winhost)

			g.By(fmt.Sprintf("Verifying node %s is Ready before testing", nodeName))
			waitWindowsNodeReady(oc, nodeName, 5*time.Minute)

			g.By("Verifying hybrid-overlay-node binary and CA certificate on host filesystem")
			binaryCheck, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
				`$bin = Test-Path 'C:\host\k\hybrid-overlay-node.exe'; `+
					`$certs = Get-ChildItem 'C:\host\k' -Filter '*.crt' -ErrorAction SilentlyContinue; `+
					`Write-Output "binary=$bin"; `+
					`Write-Output "certs=$($certs.Count)"`)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to check hybrid-overlay binary")
			o.Expect(binaryCheck).To(o.ContainSubstring("binary=True"), "hybrid-overlay-node.exe should exist on host")
			o.Expect(binaryCheck).To(o.MatchRegexp(`certs=[1-9]`), "CA certificate file should exist in C:\\k\\")
			e2e.Logf("Hybrid-overlay binary and CA cert verified on %s", nodeName)

			g.By("Dumping hybrid-overlay log for analysis")
			logPath := `C:\host\var\log\hybrid-overlay\hybrid-overlay.log`
			logContent, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
				fmt.Sprintf("Get-Content -Raw -Path '%s' -ErrorAction SilentlyContinue", logPath))
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to read log file content")
			e2e.Logf("Successfully retrieved log content (%d characters)", len(logContent))

			if len(strings.TrimSpace(logContent)) > 0 {
				g.By("Verifying certificate rotation patterns in log")
				requiredPatterns := []struct {
					pattern     string
					description string
				}{
					{"Certificate rotation is enabled", "rotation is enabled"},
					{"Rotating certificates", "rotation process started"},
					{"Starting client certificate rotation controller", "rotation controller started"},
					{"Certificate found", "certificate was found"},
					{"is issued", "CSR was issued"},
					{"Waiting", "future rotation scheduled"},
				}
				for _, p := range requiredPatterns {
					g.By(fmt.Sprintf("Checking for: %s", p.description))
					o.Expect(strings.Contains(logContent, p.pattern)).To(o.BeTrue(),
						"Log should contain pattern '%s' indicating %s", p.pattern, p.description)
					e2e.Logf("Found pattern: %s", p.pattern)
				}

				g.By("Verifying absence of error patterns")
				for _, badPattern := range []string{"actively refused", "localhost:8443"} {
					o.Expect(strings.Contains(logContent, badPattern)).To(o.BeFalse(),
						"Log should not contain error pattern: %s", badPattern)
				}
			} else {
				e2e.Logf("Hybrid-overlay log is empty on %s, skipping log pattern checks", nodeName)
			}

			g.By(fmt.Sprintf("Verifying node %s remains stable", nodeName))
			waitWindowsNodeReady(oc, nodeName, 5*time.Minute)

			g.By(fmt.Sprintf("OCPBUGS-86246: Verifying cert cleanup on node %s via oc debug", nodeName))
			certDir := `C:\host\k\cni\config`
			certPattern := "ovnkube-client-*.pem"

			countCmd := fmt.Sprintf(
				`(Get-ChildItem -Path '%s' -Filter '%s' -ErrorAction SilentlyContinue | Measure-Object).Count`,
				certDir, certPattern)
			countBefore, debugErr := runDebugNodePS(oc, nodeName, windowsDebugImage, countCmd)
			o.Expect(debugErr).NotTo(o.HaveOccurred(), "Failed to count existing cert files")
			e2e.Logf("Existing ovnkube-client cert files before test: %s", strings.TrimSpace(countBefore))

			createCmd := fmt.Sprintf(
				`$dir = '%s'; $base = (Get-Date).AddDays(-10); `+
					`for ($i = 1; $i -le 10; $i++) { `+
					`$ts = $base.AddHours($i).ToString('yyyyMMddHHmmss'); `+
					`$f = Join-Path $dir "ovnkube-client-$ts.pem"; `+
					`Set-Content -Path $f -Value 'fake-cert'; `+
					`(Get-Item $f).CreationTime = $base.AddHours($i); `+
					`(Get-Item $f).LastWriteTime = $base.AddHours($i) }; `+
					`(Get-ChildItem -Path '%s' -Filter '%s' | Measure-Object).Count`,
				certDir, certDir, certPattern)
			countAfterCreate, debugErr := runDebugNodePS(oc, nodeName, windowsDebugImage, createCmd)
			o.Expect(debugErr).NotTo(o.HaveOccurred(), "Failed to create fake cert files")
			e2e.Logf("Total cert files after creating fakes: %s", strings.TrimSpace(countAfterCreate))

			e2e.Logf("Waiting 3 minutes for WICD to reconcile and clean up old certs...")
			time.Sleep(3 * time.Minute)

			countAfterCleanup, debugErr := runDebugNodePS(oc, nodeName, windowsDebugImage, countCmd)
			o.Expect(debugErr).NotTo(o.HaveOccurred(), "Failed to count cert files after cleanup")
			remaining := strings.TrimSpace(countAfterCleanup)
			e2e.Logf("Cert files remaining after WICD cleanup: %s", remaining)
			remainingCount, convErr := strconv.Atoi(remaining)
			o.Expect(convErr).NotTo(o.HaveOccurred(), "Failed to parse cert count")
			o.Expect(remainingCount).To(o.Equal(2),
				"WICD should keep only 2 most recent ovnkube-client cert files, found %d", remainingCount)

			e2e.Logf("Successfully verified certificate rotation and cert cleanup on %s", nodeName)
		}
	})

	// author: rrasouli@redhat.com
	g.It("Author:rrasouli-Smokerun-Medium-88278-wmco reads certificates from controllerConfig instead of MachineConfig", func() {
		winInternalIPs := getWindowsInternalIPs(oc)
		o.Expect(len(winInternalIPs)).To(o.BeNumerically(">", 0), "Test requires at least one Windows node")

		g.By("Ensure Windows nodes are Ready before proceeding")
		waitWindowsNodesReady(oc, len(winInternalIPs), 15*time.Minute)

		g.By("Retrieve kubelet CA certificate from controllerConfig")
		kubeletCAData, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"controllerconfig", "machine-config-controller",
			"-o=jsonpath={.spec.kubeAPIServerServingCAData}").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get kubelet CA from controllerConfig")
		o.Expect(kubeletCAData).NotTo(o.BeEmpty(), "controllerConfig.spec.kubeAPIServerServingCAData should not be empty")

		kubeletCADecoded, err := base64.StdEncoding.DecodeString(kubeletCAData)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to decode kubelet CA data")
		kubeletCAContent := string(kubeletCADecoded)
		o.Expect(kubeletCAContent).To(o.ContainSubstring("BEGIN CERTIFICATE"), "Kubelet CA should be PEM-formatted")
		e2e.Logf("Successfully retrieved kubelet CA from controllerConfig (%d bytes)", len(kubeletCAContent))

		g.By("Verify kubelet-ca.crt exists on Windows nodes and matches controllerConfig data")
		kubeletCertPath := `C:\host\k\kubelet-ca.crt`
		for _, winhost := range winInternalIPs {
			nodeName := getNodeNameFromIP(oc, winhost)
			g.By(fmt.Sprintf("Verifying kubelet-ca.crt on Windows node %s", nodeName))

			fileExistsCmd := fmt.Sprintf("Test-Path -Path '%s'", kubeletCertPath)
			fileExists, err := runDebugNodePS(oc, nodeName, windowsDebugImage, fileExistsCmd)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to check if kubelet-ca.crt exists on %s", nodeName)
			o.Expect(strings.TrimSpace(fileExists)).To(o.Equal("True"),
				"kubelet-ca.crt should exist at %s on node %s", kubeletCertPath, nodeName)

			certContent, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
				fmt.Sprintf("Get-Content -Raw -Path '%s'", kubeletCertPath))
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to read kubelet-ca.crt from %s", nodeName)
			o.Expect(certContent).NotTo(o.BeEmpty(), "Certificate content should not be empty on %s", nodeName)

			o.Expect(strings.TrimSpace(certContent)).To(o.ContainSubstring(strings.TrimSpace(kubeletCAContent)),
				"kubelet-ca.crt content on %s should match controllerConfig.spec.kubeAPIServerServingCAData", nodeName)
			e2e.Logf("Verified kubelet-ca.crt on %s matches controllerConfig data", nodeName)
		}

		g.By("Retrieve cloud provider CA data from controllerConfig (if present)")
		cloudCAData, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"controllerconfig", "machine-config-controller",
			"-o=jsonpath={.spec.cloudProviderCAData}").Output()
		if err == nil && cloudCAData != "" && len(cloudCAData) > 10 && !strings.Contains(cloudCAData, "null") {
			e2e.Logf("Cloud provider CA data found in controllerConfig (%d bytes)", len(cloudCAData))

			cloudCADecoded, err := base64.StdEncoding.DecodeString(cloudCAData)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to decode cloud provider CA data")
			cloudCAContent := string(cloudCADecoded)

			cloudCACertPath := `C:\host\k\ca-bundle.crt`
			for _, winhost := range winInternalIPs {
				nodeName := getNodeNameFromIP(oc, winhost)
				g.By(fmt.Sprintf("Verifying cloud provider CA on Windows node %s", nodeName))

				fileExists, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
					fmt.Sprintf("Test-Path -Path '%s'", cloudCACertPath))
				if err == nil && strings.TrimSpace(fileExists) == "True" {
					certContent, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
						fmt.Sprintf("Get-Content -Raw -Path '%s'", cloudCACertPath))
					o.Expect(err).NotTo(o.HaveOccurred(), "Failed to read cloud CA from %s", nodeName)
					o.Expect(strings.TrimSpace(certContent)).To(o.ContainSubstring(strings.TrimSpace(cloudCAContent)),
						"Cloud CA content on %s should match controllerConfig.spec.cloudProviderCAData", nodeName)
					e2e.Logf("Verified cloud provider CA on %s matches controllerConfig data", nodeName)
				} else {
					e2e.Logf("Cloud provider CA bundle not found on %s (may not be required for this platform)", nodeName)
				}
			}
		} else {
			e2e.Logf("Cloud provider CA data not configured in controllerConfig (platform may not require it)")
		}

		g.By("Retrieve additional trust bundle from controllerConfig (if present)")
		trustBundleData, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"controllerconfig", "machine-config-controller",
			"-o=jsonpath={.spec.additionalTrustBundle}").Output()
		var trustBundleContent string
		if err == nil && trustBundleData != "" && len(trustBundleData) > 10 {
			decoded, decErr := base64.StdEncoding.DecodeString(trustBundleData)
			if decErr == nil {
				trustBundleContent = string(decoded)
			} else {
				trustBundleContent = trustBundleData
			}
		}
		if trustBundleContent != "" && strings.Contains(trustBundleContent, "BEGIN CERTIFICATE") {
			e2e.Logf("Additional trust bundle found in controllerConfig (%d bytes)", len(trustBundleContent))

			for _, winhost := range winInternalIPs {
				nodeName := getNodeNameFromIP(oc, winhost)
				g.By(fmt.Sprintf("Verifying additional trust bundle on Windows node %s", nodeName))

				certCheckCmd := `(Get-ChildItem -Path Cert:\LocalMachine\Root | Measure-Object).Count`
				certCount, err := runDebugNodePS(oc, nodeName, windowsDebugImage, certCheckCmd)
				o.Expect(err).NotTo(o.HaveOccurred(), "Failed to query certificate store on %s", nodeName)
				e2e.Logf("Node %s has %s certificates in LocalMachine\\Root store", nodeName, strings.TrimSpace(certCount))

				count, err := strconv.Atoi(strings.TrimSpace(certCount))
				o.Expect(err).NotTo(o.HaveOccurred(), "Failed to parse certificate count")
				o.Expect(count).To(o.BeNumerically(">", 0), "Certificate store should contain certificates on %s", nodeName)
			}
		} else {
			e2e.Logf("Additional trust bundle not configured in controllerConfig")
		}

		g.By("Verify Windows nodes remain Ready and functional")
		waitWindowsNodesReady(oc, len(winInternalIPs), 5*time.Minute)
		e2e.Logf("All Windows nodes are Ready and using certificates from controllerConfig")
	})

	g.It("Author:rrasouli-Smokerun-Medium-79100-Horizontal Pod Autoscaling with Windows containers", func() {
		if !haveMetricsServer(oc) {
			g.Skip("metrics-server is required for HPA testing")
		}

		namespace := "winc-79100"
		deploymentName := "win-webserver"
		defer deleteProject(oc, namespace)

		g.By("Creating test namespace")
		createProject(oc, namespace)

		g.By("Creating Windows deployment")
		manifest := generateWindowsWebServerYAML(deploymentName, namespace, windowsDebugImage, 1, false, "200m", "")
		err := createResourceFromString(oc, namespace, manifest)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = waitForDeploymentReady(oc, deploymentName, namespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Creating memory-based HPA")
		memoryManifest := generateHPAYAML("hpa-resource-metrics-memory", namespace, deploymentName, 1, 5, 20, "memory", "40Mi")
		err = createResourceFromString(oc, namespace, memoryManifest)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create memory HPA")
		defer func() {
			if delErr := oc.AsAdmin().WithoutNamespace().Run("delete").Args("hpa", "hpa-resource-metrics-memory", "-n", namespace, "--ignore-not-found").Execute(); delErr != nil {
				e2e.Logf("Warning: failed to cleanup memory HPA: %v", delErr)
			}
		}()

		g.By("Verifying HPA scales up deployment")
		err = wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
			msg, _ := oc.AsAdmin().WithoutNamespace().Run("get").
				Args("deployment", deploymentName, "-o=jsonpath={.status.readyReplicas}", "-n", namespace).Output()
			numberOfWorkloads, _ := strconv.Atoi(msg)
			return numberOfWorkloads > 1, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "Deployment did not scale up")

		g.By("Patching memory HPA to trigger scale down")
		_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"hpa", "hpa-resource-metrics-memory",
			"-n", namespace,
			"--type=merge",
			"--patch", `{"spec":{"metrics":[{"resource":{"target":{"type":"AverageValue","averageValue":"150Mi"},"name":"memory"},"type":"Resource"}]}}`,
		).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to patch memory HPA")

		g.By("Removing memory HPA and scaling deployment back to 1")
		err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("hpa", "hpa-resource-metrics-memory", "-n", namespace, "--ignore-not-found").Execute()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to delete memory HPA")
		err = scaleDeployment(oc, deploymentName, 1, namespace)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to scale deployment back to 1 after removing memory HPA")

		g.By("Creating CPU-based HPA")
		cpuManifest := generateHPAYAML("hpa-resource-metrics-cpu", namespace, deploymentName, 1, 5, 20, "cpu", "1m")
		err = createResourceFromString(oc, namespace, cpuManifest)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create CPU HPA")
		defer func() {
			if delErr := oc.AsAdmin().WithoutNamespace().Run("delete").Args("hpa", "hpa-resource-metrics-cpu", "-n", namespace, "--ignore-not-found").Execute(); delErr != nil {
				e2e.Logf("Warning: failed to cleanup CPU HPA: %v", delErr)
			}
		}()

		g.By("Verifying HPA scales up deployment")
		err = wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
			msg, _ := oc.AsAdmin().WithoutNamespace().Run("get").
				Args("deployment", deploymentName, "-o=jsonpath={.status.readyReplicas}", "-n", namespace).Output()
			numberOfWorkloads, _ := strconv.Atoi(msg)
			return numberOfWorkloads > 1, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Patching CPU HPA to trigger scale down")
		_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args(
			"hpa", "hpa-resource-metrics-cpu",
			"-n", namespace,
			"--type=merge",
			"--patch", `{"spec":{"metrics":[{"resource":{"target":{"type":"AverageValue","averageValue":"500m"},"name":"cpu"},"type":"Resource"}]}}`,
		).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Removing CPU HPA and scaling deployment back to 1")
		err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("hpa", "hpa-resource-metrics-cpu", "-n", namespace, "--ignore-not-found").Execute()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to delete CPU HPA")
		err = scaleDeployment(oc, deploymentName, 1, namespace)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to scale deployment back to 1 after removing CPU HPA")
	})

	g.It("Author:rrasouli-Smokerun-Medium-87711-Windows workload with RuntimeClass [Serial]", func() {
		namespace := "winc-87711"
		deploymentName := "win-webserver"
		defer deleteProject(oc, namespace)
		createProject(oc, namespace)

		g.By("Step 1: Get Windows node and its build version")
		windowsNode, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("nodes", "-l", windowsNodeLabel, "-o=jsonpath={.items[0].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(windowsNode).NotTo(o.BeEmpty(), "At least one Windows node should exist")

		buildID, err := getWindowsBuildID(oc, windowsNode)
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Windows node %s has build ID: %s", windowsNode, buildID)

		g.By("Step 2: Create RuntimeClass for Windows build")
		runtimeClass := namespace + "-runtimeclass"
		runtimeClassYaml := generateRuntimeClassYAML(runtimeClass, buildID)
		err = createResourceFromString(oc, "", runtimeClassYaml)
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Created RuntimeClass: %s for build ID: %s", runtimeClass, buildID)

		defer func() {
			_, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args("runtimeclass", runtimeClass).Output()
			if err != nil {
				e2e.Logf("Warning: Failed to delete RuntimeClass %s: %v", runtimeClass, err)
			} else {
				e2e.Logf("Deleted RuntimeClass: %s", runtimeClass)
			}
		}()

		g.By("Step 3: Verify RuntimeClass was created successfully")
		runtimeClassOutput, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("runtimeclass", runtimeClass, "-o", "yaml").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(runtimeClassOutput).To(o.ContainSubstring("runhcs-wcow-process"), "RuntimeClass should have handler: runhcs-wcow-process")
		o.Expect(runtimeClassOutput).To(o.ContainSubstring(buildID), "RuntimeClass should have build ID in nodeSelector")
		e2e.Logf("RuntimeClass %s is properly configured with handler runhcs-wcow-process", runtimeClass)

		g.By("Step 4: Create Windows deployment with RuntimeClass")
		manifest := generateWindowsWebServerYAML(deploymentName, namespace, windowsDebugImage, 1, true, "", runtimeClass)
		err = createResourceFromString(oc, namespace, manifest)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = waitForDeploymentReady(oc, deploymentName, namespace, 10*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Step 5: Verify pod spec contains correct RuntimeClass")
		podName, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("pods", "-n", namespace, "-l=app="+deploymentName, "-o=jsonpath={.items[0].metadata.name}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(podName).NotTo(o.BeEmpty(), "Pod should exist")

		podRuntimeClass, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("pod", podName, "-n", namespace, "-o=jsonpath={.spec.runtimeClassName}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(podRuntimeClass).To(o.Equal(runtimeClass), "Pod should have runtimeClassName: %s", runtimeClass)
		e2e.Logf("Pod %s has correct runtimeClassName: %s", podName, podRuntimeClass)

		g.By("Step 6: Verify pod is scheduled on Windows node")
		nodeName, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("pod", podName, "-n", namespace, "-o=jsonpath={.spec.nodeName}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(nodeName).NotTo(o.BeEmpty(), "Pod should be scheduled on a node")

		nodeOS, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("node", nodeName, "-o=jsonpath={.metadata.labels.kubernetes\\.io/os}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(nodeOS).To(o.Equal("windows"), "Pod should be scheduled on Windows node")
		e2e.Logf("Pod is scheduled on Windows node: %s", nodeName)

		g.By("Step 7: Validate workload functionality via HTTP")
		if iaasPlatform != "vsphere" && iaasPlatform != "nutanix" && !isNone(oc) {
			externalIP, err := getExternalIP(iaasPlatform, oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			url := "http://" + net.JoinHostPort(externalIP, "80")
			var lastErr error
			success := false
			maxRetries := 20
			retryInterval := 15 * time.Second

			httpClient := &http.Client{Timeout: 15 * time.Second}
			e2e.Logf("Testing HTTP connectivity to %s (max %d retries)", url, maxRetries)
			for i := 0; i < maxRetries; i++ {
				resp, err := httpClient.Get(url)
				if err == nil && resp.StatusCode == 200 {
					resp.Body.Close()
					e2e.Logf("Windows workload with RuntimeClass is functional and accessible via HTTP")
					success = true
					break
				}
				if err != nil {
					lastErr = err
					e2e.Logf("Retry %d/%d: HTTP GET failed: %v", i+1, maxRetries, err)
				} else {
					lastErr = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
					resp.Body.Close()
					e2e.Logf("Retry %d/%d: Got status %d, expected 200", i+1, maxRetries, resp.StatusCode)
				}
				if i < maxRetries-1 {
					time.Sleep(retryInterval)
				}
			}
			o.Expect(success).To(o.BeTrue(), "Should be able to connect to Windows web server after %d retries. Last error: %v", maxRetries, lastErr)
		} else {
			e2e.Logf("Skipping HTTP connectivity test on platform %s (LoadBalancer not supported)", iaasPlatform)
		}

		g.By("Step 8: Verify pod events show no RuntimeClass errors")
		podEvents, err := oc.AsAdmin().WithoutNamespace().Run("get").
			Args("events", "-n", namespace, "--field-selector", fmt.Sprintf("involvedObject.name=%s", podName), "-o=jsonpath={.items[*].message}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(podEvents).NotTo(o.ContainSubstring("RuntimeClass not found"), "Pod events should not contain RuntimeClass not found errors")
		o.Expect(podEvents).NotTo(o.ContainSubstring("forbidden RuntimeClass"), "Pod events should not contain forbidden RuntimeClass errors")
		e2e.Logf("No RuntimeClass-related errors found in pod events")
	})

	g.It("Smokerun-Author:sgao-Critical-28632-Windows and Linux east west network during a long time", func() {
		namespace := "winc-28632"
		winDeployment := "win-webserver"
		linuxDeployment := "linux-webserver"
		defer deleteProject(oc, namespace)
		createProject(oc, namespace)

		g.By("Deploy Windows and Linux web servers")
		winManifest := generateWindowsWebServerYAML(winDeployment, namespace, windowsDebugImage, 1, false, "", "")
		err := createResourceFromString(oc, namespace, winManifest)
		o.Expect(err).NotTo(o.HaveOccurred())

		linuxManifest := generateLinuxWebServerYAML(linuxDeployment, namespace, linuxDebugImage, 1)
		err = createResourceFromString(oc, namespace, linuxManifest)
		o.Expect(err).NotTo(o.HaveOccurred())

		err = waitForDeploymentReady(oc, winDeployment, namespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = waitForDeploymentReady(oc, linuxDeployment, namespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Check communication: Windows pod <--> Linux pod")
		winPodNames, err := getWorkloadsNames(oc, winDeployment, namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(winPodNames).NotTo(o.BeEmpty(), "Windows pod names should not be empty")
		winPodIPs, err := getWorkloadsIP(oc, winDeployment, namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(winPodIPs).NotTo(o.BeEmpty(), "Windows pod IPs should not be empty")
		linuxPodNames, err := getWorkloadsNames(oc, linuxDeployment, namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(linuxPodNames).NotTo(o.BeEmpty(), "Linux pod names should not be empty")
		linuxPodIPs, err := getWorkloadsIP(oc, linuxDeployment, namespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(linuxPodIPs).NotTo(o.BeEmpty(), "Linux pod IPs should not be empty")

		g.By("Windows pod -> Linux pod")
		psCmd := buildInvokeWebRequestCommand("http://" + net.JoinHostPort(linuxPodIPs[0], "8080"))
		msg, err := oc.AsAdmin().WithoutNamespace().Run("exec").Args("-n", namespace, winPodNames[0], "--", "pwsh.exe", "-Command", psCmd).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(msg).To(o.ContainSubstring("Linux Container Web Server"), "Failed to access Linux web server from Windows pod")

		g.By("Linux pod -> Windows pod")
		msg, err = oc.AsAdmin().WithoutNamespace().Run("exec").Args("-n", namespace, linuxPodNames[0], "--", "curl", "-s", net.JoinHostPort(winPodIPs[0], "80")).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(msg).To(o.ContainSubstring("Windows Container Web Server"), "Failed to curl Windows web server from Linux pod")
	})

	g.It("Smokerun-Author:rrasouli-High-38186-[wmco] Windows LB service [Slow]", func() {
		if iaasPlatform == "vsphere" || iaasPlatform == "nutanix" {
			g.Skip(fmt.Sprintf("Platform %s does not support Load balancer, skipping", iaasPlatform))
		}
		if isNone(oc) {
			g.Skip("Platform none does not support Load balancer, skipping")
		}
		namespace := "winc-38186"
		deploymentName := "win-webserver"
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		defer deleteProject(oc, namespace)
		createProject(oc, namespace)

		g.By("Creating Windows web server deployment with LB service")
		manifest := generateWindowsWebServerYAML(deploymentName, namespace, windowsDebugImage, 1, true, "", "")
		err := createResourceFromString(oc, namespace, manifest)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = waitForDeploymentReady(oc, deploymentName, namespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		externalIP, err := getExternalIP(iaasPlatform, oc, deploymentName, namespace)
		o.Expect(err).NotTo(o.HaveOccurred())

		lbURL := "http://" + net.JoinHostPort(externalIP, "80")
		g.By("Waiting for LB endpoint to become reachable")
		err = wait.Poll(10*time.Second, 3*time.Minute, func() (bool, error) {
			curl := exec.CommandContext(ctx, "curl", "--connect-timeout", "5", "-s", lbURL)
			out, curlErr := curl.Output()
			if curlErr != nil {
				e2e.Logf("LB not yet reachable: %v", curlErr)
				return false, nil
			}
			if strings.Contains(string(out), "Windows Container Web Server") {
				return true, nil
			}
			return false, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "LB endpoint did not become reachable")

		g.By("Test LB " + externalIP + " connectivity")
		bgErr := runInBackground(ctx, cancel, checkConnectivity, externalIP, 5)

		g.By("Scale to 6 Windows workloads across nodes")
		err = scaleDeployment(oc, deploymentName, 6, namespace)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to scale deployment to 6 replicas")

		cancel()
		err = <-bgErr
		o.Expect(err).NotTo(o.HaveOccurred(), "Connectivity check failed during scaling")
	})

	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Critical-33783-Enable must gather on Windows node [Slow][Disruptive]", func() {
		destDir := "/tmp/must-gather-33783"
		defer os.RemoveAll(destDir)

		g.By("Run must-gather and verify Windows log paths")
		msg, err := oc.AsAdmin().WithoutNamespace().Run("adm").Args("must-gather", "--dest-dir="+destDir).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		expectedPaths := []string{
			"host_service_logs/windows/",
			"host_service_logs/windows/log_files/",
			"host_service_logs/windows/log_files/hybrid-overlay/",
			"host_service_logs/windows/log_files/hybrid-overlay/hybrid-overlay.log",
			"host_service_logs/windows/log_files/kube-proxy/",
			"host_service_logs/windows/log_files/kube-proxy/kube-proxy.log",
			"host_service_logs/windows/log_files/kubelet/",
			"host_service_logs/windows/log_files/kubelet/kubelet.log",
			"host_service_logs/windows/log_files/containerd/containerd.log",
			"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.ERROR",
			"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.INFO",
			"host_service_logs/windows/log_files/wicd/windows-instance-config-daemon.exe.WARNING",
			"host_service_logs/windows/log_files/csi-proxy/",
			"host_service_logs/windows/log_files/csi-proxy/csi-proxy.log",
		}
		for _, path := range expectedPaths {
			o.Expect(msg).To(o.ContainSubstring(path),
				"must-gather output should contain %s", path)
		}
	})

	// author: jfrancoa@redhat.com
	g.It("Smokerun-Author:jfrancoa-Medium-50403-wmco creates and maintains Windows services ConfigMap [Disruptive]", func() {
		g.By("Check service configmap exists")
		wmcoLogVersion, err := getWMCOVersionFromLogs(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		expectedCMName := "windows-services-" + wmcoLogVersion

		cmName, err := getLatestServicesCMName(oc)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(cmName).To(o.Equal(expectedCMName), "ConfigMap name should match WMCO version from logs")

		g.By("Check windowsmachineconfig/desired-version annotation")
		for _, winHostName := range getWindowsHostNames(oc) {
			desiredVersion, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("nodes", winHostName,
				"-o=jsonpath={.metadata.annotations.windowsmachineconfig\\.openshift\\.io\\/desired-version}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(strings.TrimSpace(desiredVersion)).To(o.Equal(wmcoLogVersion),
				"desired-version annotation mismatch on host %v", winHostName)
		}

		g.By("Check that windows-instance-config-daemon serviceaccount exists")
		_, err = oc.AsAdmin().WithoutNamespace().Run("get").Args("serviceaccount", "windows-instance-config-daemon", "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Delete windows-services configmap and wait for its recreation")
		_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", cmName, "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForServicesCM(oc, expectedCMName, 10*time.Minute)

		g.By("Attempt to modify the windows-services configmap data")
		_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("configmap", cmName, "-p", `{"data":{"services":"[]"}}`, "-n", wmcoNamespace).Output()
		if err == nil {
			e2e.Failf("It should not be possible to modify configmap %v", cmName)
		}

		g.By("Attempt to modify the immutable field")
		_, err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("configmap", cmName, "-p", `{"inmutable":false}`, "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		cmImmutable, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", cmName, "-n", wmcoNamespace, "-o=jsonpath={.immutable}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(strings.TrimSpace(cmImmutable)).To(o.Equal("true"),
			"Immutable field inside %v configmap should not be modifiable", cmName)

		g.By("Stop WMCO, delete existing windows-services configmap and create new dummy ones")
		defer func() {
			if scaleErr := scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace); scaleErr != nil {
				e2e.Logf("Warning: failed to scale WMCO back to 1: %v", scaleErr)
			}
		}()
		err = scaleDeployment(oc, wmcoDeploymentName, 0, wmcoNamespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", cmName, "-n", wmcoNamespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		for _, version := range []string{"8.8.8-55657c8", "0.0.1-55657c8", wmcoLogVersion} {
			manifest := generateWICDConfigMapYAML("windows-services-"+version, "[]")
			defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", "windows-services-"+version, "-n", wmcoNamespace, "--ignore-not-found").Execute()
			err = createResourceFromString(oc, wmcoNamespace, manifest)
			o.Expect(err).NotTo(o.HaveOccurred())
		}

		err = scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForServicesCM(oc, expectedCMName, 10*time.Minute)
	})

	// author: jfrancoa@redhat.com
	g.It("Author:jfrancoa-Smokerun-Medium-56354-Stop dependent services before stopping a service in WICD [Disruptive][Serial]", func() {
		targetService := "containerd"

		g.By("Ensure Windows nodes are Ready before proceeding")
		winHostNames := getWindowsHostNames(oc)
		expectedWindowsNodes := len(winHostNames)
		waitWindowsNodesReady(oc, expectedWindowsNodes, 10*time.Minute)

		for _, nodeName := range winHostNames {
			g.By(fmt.Sprintf("Modify %v service binPath and check that it gets restored on %v", targetService, nodeName))

			initialBinPath, err := getServiceBinPath(oc, nodeName, windowsDebugImage, targetService)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(initialBinPath).NotTo(o.BeEmpty(), "initial binPath should not be empty")

			modifiedBinPath := initialBinPath + " --service-name containerd"
			err = setServiceBinPath(oc, nodeName, windowsDebugImage, targetService, modifiedBinPath)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Poll for the service binPath to be restored by WICD")
			pollErr := wait.Poll(10*time.Second, 10*time.Minute, func() (bool, error) {
				currentBinPath, err := getServiceBinPath(oc, nodeName, windowsDebugImage, targetService)
				if err != nil {
					e2e.Logf("Error getting binPath: %v", err)
					return false, nil
				}
				return currentBinPath == initialBinPath, nil
			})
			o.Expect(pollErr).NotTo(o.HaveOccurred(),
				"Service binPath did not return to initial state within timeout")

			afterBinPath, err := getServiceBinPath(oc, nodeName, windowsDebugImage, targetService)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(afterBinPath).To(o.Equal(initialBinPath))

			g.By(fmt.Sprintf("Waiting for node %s to stabilize after WICD reconciliation", nodeName))
			waitWindowsNodeReady(oc, nodeName, 5*time.Minute)
			time.Sleep(30 * time.Second)
		}
	})

	// author: rrasouli@redhat.com
	g.It("Author:rrasouli-Longduration-Smokerun-Medium-76765-WICD-Remove-Services [Slow][Disruptive]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			wmcoLogVersion, err := getWMCOVersionFromLogs(oc)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Step 1: Fetch the WICD ConfigMap and verify its existence")
			windowsServicesCM, err := getLatestServicesCMName(oc)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(windowsServicesCM).NotTo(o.BeEmpty(), "Expected to find a WICD ConfigMap")

			g.By("Step 2: Extract services from the ConfigMap")
			payload, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", windowsServicesCM, "-n", wmcoNamespace, "-o=jsonpath={.data.services}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(payload).NotTo(o.BeEmpty(), "Expected non-empty services payload in ConfigMap")

			var configMapServices []Service
			err = json.Unmarshal([]byte(payload), &configMapServices)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(configMapServices).NotTo(o.BeEmpty(), "Expected to find services defined in the ConfigMap")

			g.By("Step 3: Retrieve Windows worker information")
			winHostNames := getWindowsHostNames(oc)
			o.Expect(winHostNames).NotTo(o.BeEmpty(), "Expected to find Windows worker nodes")

			g.By("Step 4: Verify that each service is running on the Windows worker nodes")
			for _, nodeName := range winHostNames {
				for _, svc := range configMapServices {
					g.By(fmt.Sprintf("Checking service %s on host %s", svc.Name, nodeName))
					pollErr := wait.Poll(5*time.Second, 2*time.Minute, func() (bool, error) {
						ok, err := checkWindowsServiceRunning(oc, nodeName, windowsDebugImage, svc.Name)
						if err != nil {
							e2e.Logf("Error checking service %s on %s: %v", svc.Name, nodeName, err)
							return false, nil
						}
						return ok, nil
					})
					o.Expect(pollErr).NotTo(o.HaveOccurred(),
						"service %v should be running on %v", svc.Name, nodeName)
				}
			}

			g.By("Step 5: Scale WMCO to 0 and remove the existing windows-services ConfigMap")
			defer func() {
				if scaleErr := scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace); scaleErr != nil {
					e2e.Logf("Warning: failed to scale WMCO back to 1: %v", scaleErr)
				}
			}()
			err = scaleDeployment(oc, wmcoDeploymentName, 0, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			err = oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", windowsServicesCM, "-n", wmcoNamespace).Execute()
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to delete windows-services ConfigMap %v", windowsServicesCM)

			g.By("Step 6: Generate new service ConfigMap with fake services")
			newServicesJSON := `[{"name":"new-service-1","path":"C:\\k\\new-service-1.exe --logfile C:\\var\\log\\new-service-1.log","bootstrap":false,"priority":2},{"name":"new-service-2","path":"C:\\k\\new-service-2.exe --logfile C:\\var\\log\\new-service-2.log","bootstrap":false,"priority":3}]`
			newCMManifest := generateWICDConfigMapYAML("windows-services-"+wmcoLogVersion, newServicesJSON)
			defer oc.AsAdmin().WithoutNamespace().Run("delete").Args("configmap", "windows-services-"+wmcoLogVersion, "-n", wmcoNamespace, "--ignore-not-found").Execute()

			err = createResourceFromString(oc, wmcoNamespace, newCMManifest)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create new windows-services ConfigMap")

			waitForServicesCM(oc, windowsServicesCM, 10*time.Minute)

			g.By("Step 7: Scale WMCO back to 1 and wait for node reconfiguration")
			err = scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			waitWindowsNodesReady(oc, len(winHostNames), 15*time.Minute)

			g.By("Step 8: Verify the initial state of services (all should be running)")
			for _, nodeName := range winHostNames {
				for _, svc := range configMapServices {
					pollErr := wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
						ok, err := checkWindowsServiceRunning(oc, nodeName, windowsDebugImage, svc.Name)
						if err != nil {
							e2e.Logf("Error checking service %s on %s: %v", svc.Name, nodeName, err)
							return false, nil
						}
						return ok, nil
					})
					o.Expect(pollErr).NotTo(o.HaveOccurred(),
						"Service %s is not running on %s after retries", svc.Name, nodeName)
				}
			}

			g.By("Step 9: Simulate service removal (in reverse order to respect priority)")
			removedServices := make([]string, 0)
			for i := len(configMapServices) - 1; i >= 0; i-- {
				serviceName := configMapServices[i].Name
				removedServices = append(removedServices, serviceName)
				e2e.Logf("Simulating removal of service: %s", serviceName)
			}

			g.By("Step 10: Verify service removal order")
			servicesByPriority := make(map[int][]string)
			for _, svc := range configMapServices {
				servicesByPriority[svc.Priority] = append(servicesByPriority[svc.Priority], svc.Name)
			}

			serviceRemovalOrder := make(map[string]int)
			for pos, serviceName := range removedServices {
				serviceRemovalOrder[serviceName] = pos
			}

			priorities := make([]int, 0, len(servicesByPriority))
			for priority := range servicesByPriority {
				priorities = append(priorities, priority)
			}
			sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

			for i := 0; i < len(priorities)-1; i++ {
				currentPriority := priorities[i]
				nextPriority := priorities[i+1]
				currentServices := servicesByPriority[currentPriority]
				nextServices := servicesByPriority[nextPriority]

				currentEarliestPos := -1
				nextEarliestPos := -1

				for _, svc := range currentServices {
					pos, exists := serviceRemovalOrder[svc]
					if exists && (currentEarliestPos == -1 || pos < currentEarliestPos) {
						currentEarliestPos = pos
					}
				}

				for _, svc := range nextServices {
					pos, exists := serviceRemovalOrder[svc]
					if exists && (nextEarliestPos == -1 || pos < nextEarliestPos) {
						nextEarliestPos = pos
					}
				}

				o.Expect(currentEarliestPos).To(o.BeNumerically("<", nextEarliestPos),
					"Expected services with priority %d to be removed before services with priority %d",
					currentPriority, nextPriority)
			}

			g.By("Step 11: Stop services on Windows workers")
			maxRetries := 3
			retryInterval := 30 * time.Second
			defer waitWindowsNodesReady(oc, len(winHostNames), 15*time.Minute)

			// Stop-Service -Force handles the dependency chain that sc.exe stop
			// cannot (error 1051). Kubelet and containerd are stopped together in
			// a single HostProcess pod at the end: kubelet must still be running
			// to schedule the pod, and stopping containerd is self-destructive
			// (kills the pod's own runtime), so the pod will report as failed.
			for _, nodeName := range winHostNames {
				e2e.Logf("Stopping services on Windows host: %s", nodeName)

				for _, serviceName := range removedServices {
					if serviceName == "kubelet" || serviceName == "containerd" {
						continue
					}
					var lastErr error
					lastStatus := ""
					for i := 0; i < maxRetries; i++ {
						e2e.Logf("Attempt %d to stop service %s on %s", i+1, serviceName, nodeName)
						cmd := fmt.Sprintf(
							"Stop-Service '%s' -Force -ErrorAction SilentlyContinue; "+
								"(Get-Service '%s').Status",
							serviceName, serviceName)
						status, err := runHostProcessPS(oc, nodeName, windowsDebugImage, cmd)
						if err != nil {
							e2e.Logf("Error stopping service %s on %s: %v", serviceName, nodeName, err)
							lastErr = err
							if i < maxRetries-1 {
								time.Sleep(retryInterval)
							}
							continue
						}
						lastErr = nil
						status = strings.TrimSpace(status)
						lastStatus = status
						e2e.Logf("Service %s status on %s: %s", serviceName, nodeName, status)
						if status != "Running" {
							break
						}
						e2e.Logf("Service %s still running on %s, retrying in %v...",
							serviceName, nodeName, retryInterval)
						time.Sleep(retryInterval)
					}
					o.Expect(lastErr).NotTo(o.HaveOccurred(),
						"Failed to stop service %s on host %s after retries", serviceName, nodeName)
					o.Expect(lastStatus).NotTo(o.Equal("Running"),
						"Service %s is still Running on host %s after %d stop attempts", serviceName, nodeName, maxRetries)
				}

				e2e.Logf("Stopping kubelet and containerd on %s (fire-and-forget)", nodeName)
				cmd := "Stop-Service 'kubelet' -Force -ErrorAction SilentlyContinue; " +
					"Stop-Service 'containerd' -Force -ErrorAction SilentlyContinue"
				_, err = runHostProcessPS(oc, nodeName, windowsDebugImage, cmd, false)
				o.Expect(err).NotTo(o.HaveOccurred(),
					"Failed to launch kubelet/containerd stop on %s", nodeName)
			}

			g.By("Step 12: Wait for nodes to recover and verify critical services")
			waitWindowsNodesReady(oc, len(winHostNames), 15*time.Minute)
			for _, nodeName := range winHostNames {
				for _, svcName := range []string{"kubelet", "containerd"} {
					ok, err := checkWindowsServiceRunning(oc, nodeName, windowsDebugImage, svcName)
					o.Expect(err).NotTo(o.HaveOccurred(),
						"Failed to check %s on %s", svcName, nodeName)
					o.Expect(ok).To(o.BeTrue(),
						"Service %s should be running on %s after node recovery", svcName, nodeName)
				}
			}

			g.By("Step 13: Verify no unexpected services are running")
			unexpectedServices := []string{"unwanted-service-1", "unwanted-service-2"}
			for _, nodeName := range winHostNames {
				for _, service := range unexpectedServices {
					checkCmd := fmt.Sprintf("Get-Service '%s' -ErrorAction SilentlyContinue", service)
					output, err := runHostProcessPS(oc, nodeName, windowsDebugImage, checkCmd)
					if err != nil {
						e2e.Logf("Error checking service %s on %s: %v", service, nodeName, err)
						continue
					}
					if strings.TrimSpace(output) == "" || strings.Contains(output, "Cannot find") {
						continue
					}
					statusCmd := fmt.Sprintf("Get-Service '%s' | Select-Object -ExpandProperty Status", service)
					statusOutput, err := runHostProcessPS(oc, nodeName, windowsDebugImage, statusCmd)
					if err != nil {
						e2e.Logf("Error checking status for %s on %s: %v", service, nodeName, err)
						continue
					}
					status := strings.TrimSpace(statusOutput)
					if status != "" && status != "Stopped" {
						e2e.Failf("Service %s is still running on host %s", service, nodeName)
					}
				}
				e2e.Logf("Finished checking for unexpected services on %s", nodeName)
			}
		})

	// author: jfrancoa@redhat.com
	g.It("Smokerun-Author:jfrancoa-Critical-50924-Windows instances react to kubelet CA rotation [Disruptive]", func() {
		const (
			caNamespace = "openshift-kube-apiserver-operator"
			caConfigMap = "kube-apiserver-to-kubelet-client-ca"
		)

		winHostNames := getWindowsHostNames(oc)
		expectedWindowsNodes := len(winHostNames)
		if expectedWindowsNodes == 0 {
			e2e.Failf("No Windows nodes detected in the cluster")
		}

		g.By("Ensure Windows nodes are Ready before proceeding")
		waitWindowsNodesReady(oc, expectedWindowsNodes, 15*time.Minute)

		initialCertNotBefore, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"secrets", "kube-apiserver-to-kubelet-signer", "-n", caNamespace,
			"-o=jsonpath={.metadata.annotations.auth\\.openshift\\.io\\/certificate-not-before}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		initialCertNotBeforeParsed, err := time.Parse(time.RFC3339, strings.TrimSpace(initialCertNotBefore))
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Initial kubelet CA certificate-not-before timestamp: %v", initialCertNotBeforeParsed)

		g.By("Trigger CA rotation")
		err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("secret", "-p",
			`{"metadata": {"annotations": {"auth.openshift.io/certificate-not-after": null}}}`,
			"kube-apiserver-to-kubelet-signer", "-n", caNamespace).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("Triggered CA rotation for kubelet")

		waitUntilWMCOStatusChanged(oc, "updating kubelet CA client certificates in", "1m")

		var rotatedCertNotBeforeParsed time.Time

		g.By("Poll to confirm CA rotation")
		err = wait.Poll(30*time.Second, 10*time.Minute, func() (bool, error) {
			rotatedCertNotBefore, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"secrets", "kube-apiserver-to-kubelet-signer", "-n", caNamespace,
				"-o=jsonpath={.metadata.annotations.auth\\.openshift\\.io\\/certificate-not-before}").Output()
			if err != nil {
				return false, nil
			}
			rotatedCertNotBeforeParsed, err = time.Parse(time.RFC3339, strings.TrimSpace(rotatedCertNotBefore))
			if err != nil {
				return false, nil
			}
			e2e.Logf("Polled kubelet CA certificate-not-before timestamp: %v", rotatedCertNotBeforeParsed)
			return !initialCertNotBeforeParsed.Equal(rotatedCertNotBeforeParsed), nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "Kubelet CA rotation did not happen")

		g.By("Waiting for Windows nodes to stabilize after CA rotation")
		waitWindowsNodesReady(oc, expectedWindowsNodes, 10*time.Minute)
		time.Sleep(3 * time.Minute)

		g.By("Verify kubelet client CA is updated in Windows workers")
		caBundlePath := `C:\host\k\kubelet-ca.crt`

		for _, nodeName := range winHostNames {
			g.By(fmt.Sprintf("Verify kubelet client CA content on Windows worker %v", nodeName))

			pollErr := wait.Poll(30*time.Second, 20*time.Minute, func() (bool, error) {
				kubeletCA, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
					"configmap", caConfigMap, "-n", caNamespace,
					"-o=jsonpath={.data.ca-bundle\\.crt}").Output()
				if err != nil || kubeletCA == "" {
					e2e.Logf("Error or empty kubelet client CA from ConfigMap: %v", err)
					return false, nil
				}

				bundleContent, err := runDebugNodePS(oc, nodeName, windowsDebugImage,
					fmt.Sprintf("Get-Content -Raw -Path '%s'", caBundlePath))
				if err != nil || bundleContent == "" {
					e2e.Logf("Failed fetching or empty CA bundle from Windows node %v: %v", nodeName, err)
					return false, nil
				}

				if strings.Contains(bundleContent, strings.TrimSpace(kubeletCA)) {
					e2e.Logf("Kubelet CA found in Windows worker node %v bundle", nodeName)
					return true, nil
				}
				e2e.Logf("Kubelet CA not found in Windows worker node %v bundle, retrying...", nodeName)
				return false, nil
			})

			o.Expect(pollErr).NotTo(o.HaveOccurred(),
				"Failed to verify kubelet client CA in Windows worker %v bundle", nodeName)
		}

		g.By("Ensure Windows workers were not restarted after CA rotation")
		for _, nodeName := range winHostNames {
			uptimeOutput, err := runHostProcessPS(oc, nodeName, windowsDebugImage,
				`(Get-CimInstance -ClassName Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")`)
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to get uptime from %s", nodeName)

			lastBootTime, err := time.Parse(time.RFC3339, strings.TrimSpace(uptimeOutput))
			o.Expect(err).NotTo(o.HaveOccurred(), "Failed to parse boot time from %s: %s", nodeName, uptimeOutput)

			e2e.Logf("Node %s last boot time: %v", nodeName, lastBootTime)
			if rotatedCertNotBeforeParsed.Before(lastBootTime) {
				e2e.Failf("Windows worker %v got restarted after CA rotation", nodeName)
			}
		}
	})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-Longduration-High-33794-Watch cloud private key secret [Slow][Disruptive]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			if isNone(oc) {
				g.Skip("platform none does not support changing namespace and scaling up machines")
			}
			if iaasPlatform == "vsphere" {
				g.Skip("vsphere does not support key replacement, skipping")
			}

			g.By("Step 1: Extract private key and clone MachineSet")
			privateKeyFile := extractPrivateKeyToFile(oc)
			defer os.Remove(privateKeyFile)

			zone := getAvailabilityZone(oc)
			sourceMSName := getWindowsMachineSetName(oc, defaultWindowsMS, iaasPlatform, zone)
			cloneMSName := strings.ReplaceAll(sourceMSName, "winworker", "winc-worker")
			cloneWindowsMachineSet(oc, sourceMSName, cloneMSName)
			defer func() {
				oc.AsAdmin().WithoutNamespace().Run("delete").Args(
					"machinesets.machine.openshift.io", cloneMSName, "-n", mcoNamespace,
					"--ignore-not-found").Execute()
			}()

			g.By("Step 2: Scale WMCO to 0 and delete secrets")
			defer scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
			err := scaleDeployment(oc, wmcoDeploymentName, 0, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			defer func() {
				oc.AsAdmin().WithoutNamespace().Run("create").Args(
					"secret", "generic", "cloud-private-key",
					"--from-file=private-key.pem="+privateKeyFile,
					"-n", wmcoNamespace).Execute()
			}()
			_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"secret", "cloud-private-key", "-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"secret", "windows-user-data", "-n", mcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Step 3: Scale WMCO to 1 and verify machine stuck without secrets")
			err = scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			scaleWindowsMachineSet(oc, cloneMSName, 2, 1, true)

			pollErr := wait.Poll(5*time.Second, 5*time.Minute, func() (bool, error) {
				events, _ := oc.AsAdmin().WithoutNamespace().Run("get").Args(
					"events", "-n", mcoNamespace).Output()
				status, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
					"machines.machine.openshift.io",
					"-o=jsonpath={.items[?(@.metadata.labels.machine\\.openshift\\.io\\/cluster-api-machineset==\""+cloneMSName+"\")].status.phase}",
					"-n", mcoNamespace).Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				if strings.Contains(events, "Secret \"windows-user-data\" not found") &&
					strings.EqualFold(status, "Provisioning") {
					return true, nil
				}
				return false, nil
			})
			o.Expect(pollErr).NotTo(o.HaveOccurred(),
				"Machine should be stuck in Provisioning without cloud-private-key")

			g.By("Step 4: Recreate private key and verify machine gets reconciled")
			_, err = oc.AsAdmin().WithoutNamespace().Run("create").Args(
				"secret", "generic", "cloud-private-key",
				"--from-file=private-key.pem="+privateKeyFile,
				"-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForMachinesetReady(oc, cloneMSName, 25, 1)

			g.By("Step 5: Scale down clone and test wrong key name")
			scaleWindowsMachineSet(oc, cloneMSName, 5, 0, false)

			_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"secret", "cloud-private-key", "-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			_, err = oc.AsAdmin().WithoutNamespace().Run("create").Args(
				"secret", "generic", "cloud-private-key",
				"--from-file=wrong-key.pem="+privateKeyFile,
				"-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Step 6: Scale up clone and verify WMCO detects missing key")
			scaleWindowsMachineSet(oc, cloneMSName, 2, 1, true)
			waitUntilWMCOStatusChanged(oc, "cloud-private-key missing", "1m")

			g.By("Step 7: Replace with correct key and verify reconciliation")
			_, err = oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"secret", "cloud-private-key", "-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			_, err = oc.AsAdmin().WithoutNamespace().Run("create").Args(
				"secret", "generic", "cloud-private-key",
				"--from-file=private-key.pem="+privateKeyFile,
				"-n", wmcoNamespace).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForMachinesetReady(oc, cloneMSName, 25, 1)
		})

	// author: rrasouli@redhat.com
	g.It("Smokerun-Author:rrasouli-Longduration-High-39451-Access Windows workload through clusterIP [Slow][Disruptive]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			if isNone(oc) {
				g.Skip("platform none does not support scaling up machineset tests")
			}

			namespace := "winc-39451"
			winDeployment := "win-webserver"
			linuxDeployment := "linux-webserver"
			defer deleteProject(oc, namespace)
			createProject(oc, namespace)

			g.By("Step 1: Deploy Windows and Linux web server workloads with ClusterIP services")
			winManifest := generateWindowsWebServerYAML(winDeployment, namespace, windowsDebugImage, 1, false, "", "")
			err := createResourceFromString(oc, namespace, winManifest)
			o.Expect(err).NotTo(o.HaveOccurred())
			winSvcManifest := generateClusterIPServiceYAML(winDeployment, namespace, winDeployment, 80)
			err = createResourceFromString(oc, namespace, winSvcManifest)
			o.Expect(err).NotTo(o.HaveOccurred())
			err = waitForDeploymentReady(oc, winDeployment, namespace, 5*time.Minute)
			o.Expect(err).NotTo(o.HaveOccurred())

			linuxManifest := generateLinuxWebServerYAML(linuxDeployment, namespace, linuxDebugImage, 1)
			err = createResourceFromString(oc, namespace, linuxManifest)
			o.Expect(err).NotTo(o.HaveOccurred())
			linuxSvcManifest := generateClusterIPServiceYAML(linuxDeployment, namespace, linuxDeployment, 8080)
			err = createResourceFromString(oc, namespace, linuxSvcManifest)
			o.Expect(err).NotTo(o.HaveOccurred())
			err = waitForDeploymentReady(oc, linuxDeployment, namespace, 5*time.Minute)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Step 2: Verify cross-platform ClusterIP connectivity")
			windowsClusterIP, err := getServiceClusterIP(oc, winDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			linuxClusterIP, err := getServiceClusterIP(oc, linuxDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			winPods, err := getWorkloadsNames(oc, winDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			linuxPods, err := getWorkloadsNames(oc, linuxDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			e2e.Logf("Windows ClusterIP: %s, Linux ClusterIP: %s", windowsClusterIP, linuxClusterIP)

			// Windows Pod -> Linux Service
			psCmd := buildInvokeWebRequestCommand("http://" + linuxClusterIP + ":8080")
			msg, err := oc.AsAdmin().WithoutNamespace().Run("exec").Args(
				"-n", namespace, winPods[0], "--", "pwsh.exe", "-Command", psCmd).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(msg).To(o.ContainSubstring("Linux Container Web Server"),
				"Failed to access Linux ClusterIP from Windows pod")

			// Linux Pod -> Windows Service
			msg, err = oc.AsAdmin().WithoutNamespace().Run("exec").Args(
				"-n", namespace, linuxPods[0], "--", "curl", "-s", windowsClusterIP).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(msg).To(o.ContainSubstring("Windows Container Web Server"),
				"Failed to access Windows ClusterIP from Linux pod")

			g.By("Step 3: Scale Windows deployment and verify new pod connectivity")
			err = scaleDeployment(oc, winDeployment, 2, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			winPods, err = getWorkloadsNames(oc, winDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			// New Windows Pod -> Linux Service
			psCmd = buildInvokeWebRequestCommand("http://" + linuxClusterIP + ":8080")
			msg, err = oc.AsAdmin().WithoutNamespace().Run("exec").Args(
				"-n", namespace, winPods[1], "--", "pwsh.exe", "-Command", psCmd).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(msg).To(o.ContainSubstring("Linux Container Web Server"),
				"Failed to access Linux ClusterIP from scaled Windows pod")

			g.By("Step 4: Scale Windows MachineSet and verify cross-node connectivity")
			if iaasPlatform == "azure" {
				zone := getAvailabilityZone(oc)
				windowsMachineSetName := getWindowsMachineSetName(oc, defaultWindowsMS, iaasPlatform, zone)
				publicIPValue, azErr := oc.AsAdmin().WithoutNamespace().Run("get").Args(
					"machineset", windowsMachineSetName, "-n", mcoNamespace,
					"-o", "jsonpath={.spec.template.spec.publicIP}").Output()
				if azErr == nil && strings.ToLower(strings.TrimSpace(publicIPValue)) == "false" {
					e2e.Logf("Skipping Step 4: Azure machineset has publicIP: false (OCPBUGS-9292)")
					return
				}
			}

			zone := getAvailabilityZone(oc)
			windowsMachineSetName := getWindowsMachineSetName(oc, defaultWindowsMS, iaasPlatform, zone)
			defer scaleWindowsMachineSet(oc, windowsMachineSetName, 10, 2, false)
			scaleWindowsMachineSet(oc, windowsMachineSetName, 15, 3, false)
			waitWindowsNodesReady(oc, 3, 1200*time.Second)

			winPods, err = getWorkloadsNames(oc, winDeployment, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			psCmd = buildInvokeWebRequestCommand("http://" + linuxClusterIP + ":8080")
			msg, err = oc.AsAdmin().WithoutNamespace().Run("exec").Args(
				"-n", namespace, winPods[1], "--", "pwsh.exe", "-Command", psCmd).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(msg).To(o.ContainSubstring("Linux Container Web Server"),
				"Failed to access Linux ClusterIP from Windows pod after MachineSet scale up")
		})

	// author: sgao@redhat.com
	g.It("Author:sgao-Longduration-Smokerun-Medium-39030-Re queue on Windows machines edge cases [Slow][Disruptive]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			if isNone(oc) {
				g.Skip("platform none does not support scaling up Windows machines")
			}

			g.By("Step 1: Scale down WMCO")
			defer scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
			err := scaleDeployment(oc, wmcoDeploymentName, 0, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())

			g.By("Step 2: Scale up the Windows MachineSet while WMCO is down")
			zone := getAvailabilityZone(oc)
			windowsMachineSetName := getWindowsMachineSetName(oc, defaultWindowsMS, iaasPlatform, zone)
			defer waitWindowsNodesReady(oc, 2, 1000*time.Second)
			defer scaleWindowsMachineSet(oc, windowsMachineSetName, 10, 2, false)
			scaleWindowsMachineSet(oc, windowsMachineSetName, 10, 3, true)

			g.By("Step 3: Scale up WMCO")
			err = scaleDeployment(oc, wmcoDeploymentName, 1, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForMachinesetReady(oc, windowsMachineSetName, 15, 3)

			g.By("Step 4: Verify machines created before WMCO starts are reconciled")
			waitWindowsNodesReady(oc, 3, 1200*time.Second)
		})

	// author: rrasouli@redhat.com
	g.It("Author:rrasouli-Smokerun-High-87809-Node drain with DaemonSet workloads during Windows reconciliation [Disruptive]", func() {
		if isNone(oc) {
			g.Skip("platform none does not support Windows node reconciliation")
		}

		namespace := "winc-87809"
		daemonSetName := "test-windows-daemonset"
		appLabel := "test-windows-daemon"

		createProject(oc, namespace)
		defer waitWindowsNodesReady(oc, 2, 15*time.Minute)
		defer func() {
			oc.AsAdmin().WithoutNamespace().Run("delete").Args("daemonset", daemonSetName, "-n", namespace, "--ignore-not-found").Execute()
		}()
		defer deleteProject(oc, namespace)

		g.By("Step 1: Deploy DaemonSet targeting Windows nodes")
		manifest := generateWindowsDaemonSetYAML(daemonSetName, namespace, appLabel, windowsDebugImage)
		err := createResourceFromString(oc, namespace, manifest)
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create DaemonSet")

		g.By("Step 2: Wait for DaemonSet to be ready on all Windows nodes")
		windowsNodes := getWindowsHostNames(oc)
		expectedCount := len(windowsNodes)
		o.Expect(expectedCount).To(o.BeNumerically(">", 0), "No Windows nodes found")

		pollErr := wait.Poll(30*time.Second, 10*time.Minute, func() (bool, error) {
			output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"daemonset", daemonSetName, "-n", namespace, "-o=jsonpath={.status.numberReady}").Output()
			if err != nil {
				e2e.Logf("Error getting DaemonSet status: %v", err)
				return false, nil
			}
			numberReady, err := strconv.Atoi(output)
			if err != nil {
				return false, nil
			}
			e2e.Logf("DaemonSet ready: %d/%d pods", numberReady, expectedCount)
			return numberReady == expectedCount, nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "DaemonSet did not become ready in time")

		windowsNode := windowsNodes[0]

		g.By(fmt.Sprintf("Step 3: Manually drain node %s with --ignore-daemonsets", windowsNode))
		_, err = oc.AsAdmin().WithoutNamespace().Run("adm").Args(
			"drain", windowsNode, "--ignore-daemonsets", "--force", "--delete-emptydir-data").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Manual drain should succeed with --ignore-daemonsets")

		g.By("Step 4: Verify DaemonSet pods remain on drained node")
		pollErr = wait.Poll(10*time.Second, 2*time.Minute, func() (bool, error) {
			pods, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"pods", "-n", namespace, "-l", "app="+appLabel,
				"-o=jsonpath={.items[*].spec.nodeName}").Output()
			if err != nil {
				return false, nil
			}
			return strings.Contains(pods, windowsNode), nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "DaemonSet pods should remain on drained node")

		g.By(fmt.Sprintf("Step 5: Uncordon node %s", windowsNode))
		_, err = oc.AsAdmin().WithoutNamespace().Run("adm").Args("uncordon", windowsNode).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to uncordon node")

		g.By("Step 6: Verify DaemonSet recovers after manual drain")
		pollErr = wait.Poll(10*time.Second, 5*time.Minute, func() (bool, error) {
			output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"daemonset", daemonSetName, "-n", namespace, "-o=jsonpath={.status.numberReady}").Output()
			if err != nil {
				return false, nil
			}
			numberReady, err := strconv.Atoi(output)
			if err != nil {
				return false, nil
			}
			e2e.Logf("DaemonSet recovery after manual drain: %d/%d pods ready", numberReady, expectedCount)
			return numberReady == expectedCount, nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "DaemonSet did not recover after manual drain")

		g.By(fmt.Sprintf("Step 7: Trigger WMCO reconciliation by setting invalidVersion on %s", windowsNode))
		_, err = oc.AsAdmin().WithoutNamespace().Run("annotate").Args(
			"node", windowsNode, "--overwrite",
			"windowsmachineconfig.openshift.io/version=invalidVersion").Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to set invalidVersion annotation")

		g.By("Step 8: Wait for WMCO to reconcile the node")
		waitVersionAnnotationReady(oc, windowsNode, 30*time.Second, 600*time.Second)

		g.By("Step 9: Verify DaemonSet recovers after WMCO reconciliation")
		pollErr = wait.Poll(30*time.Second, 10*time.Minute, func() (bool, error) {
			output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
				"daemonset", daemonSetName, "-n", namespace, "-o=jsonpath={.status.numberReady}").Output()
			if err != nil {
				return false, nil
			}
			numberReady, err := strconv.Atoi(output)
			if err != nil {
				return false, nil
			}
			e2e.Logf("DaemonSet recovery after WMCO reconciliation: %d/%d pods ready", numberReady, expectedCount)
			return numberReady == expectedCount, nil
		})
		o.Expect(pollErr).NotTo(o.HaveOccurred(), "DaemonSet did not recover after WMCO reconciliation")
	})

	// author: sgao@redhat.com
	g.It("Smokerun-Author:sgao-Medium-37472-Idempotent check of service running in Windows node [Disruptive]", func() {
		if isNone(oc) {
			g.Skip("platform none does not support load balancer nor external IP tests")
		}

		namespace := "winc-37472"
		deploymentName := "win-webserver"
		defer deleteProject(oc, namespace)
		createProject(oc, namespace)

		g.By("Step 1: Deploy Windows web server workload")
		manifest := generateWindowsWebServerYAML(deploymentName, namespace, windowsDebugImage, 1, true, "", "")
		err := createResourceFromString(oc, namespace, manifest)
		o.Expect(err).NotTo(o.HaveOccurred())
		err = waitForDeploymentReady(oc, deploymentName, namespace, 5*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Step 2: Remove version annotation to trigger reconciliation")
		windowsHostName := getWindowsHostNames(oc)[0]
		_, err = oc.AsAdmin().WithoutNamespace().Run("annotate").Args("node", windowsHostName, "windowsmachineconfig.openshift.io/version-").Output()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Step 3: Wait for WMCO to re-apply version annotation")
		waitVersionAnnotationReady(oc, windowsHostName, 60*time.Second, 1200*time.Second)

		g.By("Step 4: Verify LB service connectivity after reconciliation")
		if iaasPlatform != "vsphere" && iaasPlatform != "nutanix" {
			externalIP, err := getExternalIP(iaasPlatform, oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			pollErr := wait.Poll(20*time.Second, 5*time.Minute, func() (bool, error) {
				msg, _ := exec.Command("bash", "-c", "curl "+externalIP).Output()
				if !strings.Contains(string(msg), "Windows Container Web Server") {
					e2e.Logf("Load balancer is not ready yet, waiting up to 5 minutes ...")
					return false, nil
				}
				e2e.Logf("Load balancer is ready")
				return true, nil
			})
			o.Expect(pollErr).NotTo(o.HaveOccurred(), "Load balancer not ready after 5 minutes")
		} else {
			e2e.Logf("Platform %s does not support Load balancer, skipping LB check", iaasPlatform)
		}
	})

})
