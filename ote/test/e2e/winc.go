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
		queryResult, err := mon.SimpleQuery("instance:node_cpu_utilisation:rate1m{job=\"windows-exporter\"}")
		o.Expect(err).NotTo(o.HaveOccurred(), "Error querying instance:node_cpu_utilisation:rate1m")
		metricValue := extractMetricValue(queryResult)
		o.Expect(metricValue).NotTo(o.BeEmpty(), "No result for instance:node_cpu_utilisation:rate1m recording rule")

		cpuUtil, convErr := strconv.ParseFloat(metricValue, 64)
		o.Expect(convErr).NotTo(o.HaveOccurred(), "Failed to parse CPU utilisation value: %s", metricValue)
		e2e.Logf("instance:node_cpu_utilisation:rate1m = %f (should be utilization, not idle)", cpuUtil)
		o.Expect(cpuUtil).To(o.BeNumerically("<", 0.5),
			"CPU utilisation recording rule value %f suggests it is recording idle rate instead of utilization (OCPBUGS-85061)", cpuUtil)
	})

})
