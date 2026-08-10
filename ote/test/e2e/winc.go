package winc

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
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
		waitWindowsNodesReady(oc, expectedNodes, 15*time.Minute)

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

})
