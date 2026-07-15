package winc

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
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
		privateKey, err = compat_otp.GetPrivateKey()
		o.Expect(err).NotTo(o.HaveOccurred())
		publicKey, err = compat_otp.GetPublicKey()
		o.Expect(err).NotTo(o.HaveOccurred())
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
			command := []string{"exec", "-n", wmcoNamespace, "deployment.apps/windows-machine-config-operator", "--", "ls", checkFolder.folder}
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
		g.By("Check secret windows-user-data generated and contain correct public key")
		output, err := os.ReadFile(publicKey)
		o.Expect(err).NotTo(o.HaveOccurred())
		keyParts := strings.Split(string(output), " ")
		o.Expect(len(keyParts)).To(o.BeNumerically(">=", 2), "unexpected public key format")
		publicKeyContent := keyParts[1]
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
		command := []string{"exec", "-n", wmcoNamespace, "deployment.apps/windows-machine-config-operator", "--", "curl", "--http0.9", curlDest}
		msg, err := oc.AsAdmin().WithoutNamespace().Run(command...).Args().Output()
		if err != nil {
			e2e.Logf("curl exec error (may be expected for SSH banner): %v", err)
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
		msg, err := oc.AsAdmin().WithoutNamespace().Run("logs").Args("deployment.apps/windows-machine-config-operator", "-n", wmcoNamespace).Output()
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

})
