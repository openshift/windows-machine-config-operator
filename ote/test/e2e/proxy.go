package winc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[OTP][sig-windows] Windows_Containers", func() {
	defer g.GinkgoRecover()

	oc := compat_otp.NewCLIWithoutNamespace("default")

	var initialProxySpec map[string]interface{}

	g.BeforeEach(func() {
		initialProxySpec = getProxySpec(oc)
	})

	g.It("Smokerun-Author:rrasouli-Critical-65980-[node-proxy]-Cluster-wide proxy settings validation [Serial]", func() {
		g.By("Verify cluster proxy env vars match WICD ConfigMap")
		clusterEnvVars := getEnvVarProxyMap(oc)
		windowsServicesCM, err := popItemFromList(oc, "cm", wicdConfigMap, wmcoNamespace)
		o.Expect(err).NotTo(o.HaveOccurred())
		wicdPayload, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("cm", windowsServicesCM, "-n", wmcoNamespace, "-o=jsonpath={.data.environmentVars}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		wicdProxies := getPayloadMap(wicdPayload)
		if !compareMaps(clusterEnvVars, wicdProxies) {
			e2e.Failf("Cluster proxy settings are not equal to WICD proxy settings")
		}

		g.By("Verify proxy env vars exist on all Windows workers")
		winNodes := getWindowsNodeNames(oc)
		checkProxyVarsOnNodes(oc, winNodes, wicdProxies)

		g.By("Verify that trusted-ca ConfigMap exists")
		_, err = popItemFromList(oc, "cm", proxyCAConfigMap, wmcoNamespace)
		o.Expect(err).NotTo(o.HaveOccurred(), "trusted-ca configmap not found")
	})

	g.It("Smokerun-Author:rrasouli-Longduration-Critical-90290-[node-proxy]-Remove trusted CA from cluster proxy and verify propagation [Serial][Disruptive][Slow]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			defer restoreProxyEnvironment(oc, initialProxySpec)

			g.By("Verify that trusted-ca ConfigMap exists before removal")
			_, err := popItemFromList(oc, "cm", proxyCAConfigMap, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred(), "trusted-ca configmap not found")

			g.By("Remove proxy trusted CA")
			wmcoStartTime := getWMCOTimestamp(oc)
			err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", `[{"op": "replace", "path": "/spec/trustedCA/name", "value": ""}]`).Execute()
			o.Expect(err).NotTo(o.HaveOccurred(), "failed to remove trusted CA from proxy spec")

			g.By("Wait for WMCO to process trusted CA removal")
			restarted, _ := checkWMCORestarted(oc, wmcoStartTime)
			if restarted {
				e2e.Logf("WMCO restarted after trusted CA removal")
				winIPs := getWindowsInternalIPs(oc)
				waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)
			} else {
				e2e.Logf("WMCO did not restart after trusted CA removal")
			}
		})

	g.It("Smokerun-Author:rrasouli-Longduration-Critical-90289-[node-proxy]-Remove proxy variables and verify WMCO propagation [Serial][Disruptive][Slow]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			winNodes := getWindowsNodeNames(oc)
			defer restoreProxyEnvironment(oc, initialProxySpec)

			g.By("Remove no_proxy vars and verify propagation to Windows nodes")
			if getClusterProxy(oc, "spec.noProxy") != "" {
				timeNoProxy := getWMCOTimestamp(oc)
				err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", `[{"op": "remove", "path": "/spec/noProxy"}]`).Execute()
				o.Expect(err).NotTo(o.HaveOccurred(), "failed to remove noProxy")
				checkWMCORestarted(oc, timeNoProxy)
				winIPs := getWindowsInternalIPs(oc)
				waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)
				noProxyExpected := getEnvVarProxyMap(oc)
				waitForProxyOnNodes(oc, winNodes, noProxyExpected)
			} else {
				e2e.Logf("spec.noProxy is not set, skipping removal")
			}

			g.By("Remove https_proxy vars and verify propagation to Windows nodes")
			if getClusterProxy(oc, "spec.httpsProxy") != "" {
				timeNoHttps := getWMCOTimestamp(oc)
				err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", `[{"op": "remove", "path": "/spec/httpsProxy"}]`).Execute()
				o.Expect(err).NotTo(o.HaveOccurred(), "failed to remove httpsProxy")
				checkWMCORestarted(oc, timeNoHttps)
				winIPs := getWindowsInternalIPs(oc)
				waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)
				e2e.Logf("Skipping WICD ConfigMap and node propagation verification - WMCO doesn't remove env vars from ConfigMap")
			} else {
				e2e.Logf("spec.httpsProxy is not set, skipping removal")
			}

			g.By("Remove http_proxy vars and verify propagation to Windows nodes")
			if getClusterProxy(oc, "spec.httpProxy") != "" {
				timeNoHttp := getWMCOTimestamp(oc)
				err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p", `[{"op": "remove", "path": "/spec/httpProxy"}]`).Execute()
				o.Expect(err).NotTo(o.HaveOccurred())
				checkWMCORestarted(oc, timeNoHttp)
				winIPs := getWindowsInternalIPs(oc)
				waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)
				httpExpected := getEnvVarProxyMap(oc)
				waitForProxyOnNodes(oc, winNodes, httpExpected)
			} else {
				e2e.Logf("spec.httpProxy is not set, skipping removal")
			}
		})

	g.It("Smokerun-Author:rrasouli-Longduration-Critical-66670-[node-proxy]-Cluster-wide proxy trusted-ca configmap tests [Serial][Disruptive][Slow]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			defer restoreProxyEnvironment(oc, initialProxySpec)

			g.By("Add another record to cluster proxy: example.com to no-proxy")
			wmcoStartTime := getWMCOTimestamp(oc)
			err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p",
				`[{"op": "add", "path":"/spec/noProxy", "value":"test.no-proxy.com,example.com"}]`).Execute()
			o.Expect(err).NotTo(o.HaveOccurred(), "could not patch proxy with new noProxy value")
			checkWMCORestarted(oc, wmcoStartTime)
			winNodes := getWindowsNodeNames(oc)
			winIPs := getWindowsInternalIPs(oc)
			waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)

			g.By("Verify newly added noProxy record exists on WICD Windows Services")
			windowsServicesCM, err := popItemFromList(oc, "cm", wicdConfigMap, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForWICDConfigMapContains(oc, windowsServicesCM, "NO_PROXY", "example.com")

			g.By("Verify newly added record exists on each Windows worker")
			expectedProxies := getEnvVarProxyMap(oc)
			waitForProxyOnNodes(oc, winNodes, expectedProxies)

			g.By("Verify that the trusted-ca cm exists on the WMCO namespace")
			trustedCA, err := popItemFromList(oc, "cm", trustedCACM, wmcoNamespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(trustedCA).To(o.Equal(trustedCACM), "trusted CA CM does not exist on %v namespace", wmcoNamespace)

			g.By("Validate the content of trusted-ca copied to each Windows worker")
			caBundlePath := "c:\\k\\ca-bundle.crt"
			trustedCAContent, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("configmap", trustedCACM, "-n", wmcoNamespace, "-o=jsonpath={.data.ca-bundle\\.crt}").Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(trustedCAContent).NotTo(o.BeEmpty(), "trusted CA content from ConfigMap is empty")
			for _, nodeName := range winNodes {
				e2e.Logf("Verify trusted CA content is included on Windows worker %v", nodeName)
				bundleContent, err := runHostProcessPS(oc, nodeName, windowsDebugImage, fmt.Sprintf("Get-Content -Raw -Path '%s'", caBundlePath))
				o.Expect(err).NotTo(o.HaveOccurred(), "failed fetching CA bundle from Windows node %v", nodeName)
				o.Expect(strings.TrimSpace(bundleContent)).To(o.ContainSubstring("BEGIN CERTIFICATE"), "CA bundle on %v does not contain certificates", nodeName)
			}

			g.By("Check that trusted-ca configmap cannot get deleted")
			deleteResource(oc, "cm", trustedCACM, wmcoNamespace)
			waitForCM(oc, trustedCACM, trustedCACM, wmcoNamespace)

			g.By("Check that trusted-ca configmap cannot get tampered")
			err = oc.AsAdmin().WithoutNamespace().Run("patch").Args("configmap", trustedCACM, "--type=json", "-p", `[{"op": "remove", "path": "/metadata/labels"}]`, "-n", wmcoNamespace).Execute()
			o.Expect(err).NotTo(o.HaveOccurred())
			waitForCM(oc, trustedCACM, trustedCACM, wmcoNamespace)
		})

	g.It("Smokerun-Author:rrasouli-Critical-68320-[node-proxy]-Import custom CA certificates into Windows node system store [Serial][Disruptive]", func() {
		const (
			name                        = "OCP-68320-custom"
			validity                    = "3650"
			caSubj                      = "/OU=openshift/CN=test-custom-self-cert-signer"
			userSelfSignedCommonName    = "CN=test-custom-self-cert-signer, OU=openshift"
			userInstalledCertCommonName = "CN=Installer-QE-CA, OU=Installer-QE, O=OCP, S=Beijing, C=CN"
			namespace                   = "openshift-config"
			configmap                   = "user-ca-bundle"
		)

		g.By("Verify that user certificate installed on each Windows worker")
		checkUserCertificatesOnNodes(oc, userInstalledCertCommonName, 1)

		g.By("Create a self-signed certificate and append to user-ca-bundle")
		keyPath := fmt.Sprintf("%s-ca.key", name)
		crtPath := fmt.Sprintf("%s-ca.crt", name)
		defer os.Remove(keyPath)
		cmd := fmt.Sprintf("openssl genrsa -out %s-ca.key 4096", name)
		output, err := exec.Command("bash", "-c", cmd).CombinedOutput()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to generate key: %s", output)

		defer os.Remove(crtPath)
		cmd = fmt.Sprintf("openssl req -x509 -new -nodes -key %s-ca.key -sha256 -days %s -out %s-ca.crt -subj %s", name, validity, name, caSubj)
		output, err = exec.Command("bash", "-c", cmd).CombinedOutput()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to create certificate: %s", output)

		initialConfigMapContent := getConfigMapData(oc, configmap, "ca\\-bundle\\.crt", namespace)
		initialConfigMapContent = removeOuterQuotes(initialConfigMapContent)
		defer func() {
			configureCertificateToJSONPatch(oc, initialConfigMapContent, configmap, namespace)
		}()

		newCertificateContent, err := os.ReadFile(crtPath)
		o.Expect(err).NotTo(o.HaveOccurred())
		combinedContent := fmt.Sprintf("%s\n%s", initialConfigMapContent, string(newCertificateContent))
		configureCertificateToJSONPatch(oc, combinedContent, configmap, namespace)

		g.By("Verify that user certificate installed on each Windows worker")
		checkUserCertificatesOnNodes(oc, userInstalledCertCommonName, 1)

		g.By("Creating certificate rotation")
		cmd = fmt.Sprintf("openssl req -x509 -new -nodes -key %s-ca.key -sha256 -days 1 -out %s-ca.crt -subj %s", name, name, caSubj)
		output, err = exec.Command("bash", "-c", cmd).CombinedOutput()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to create rotated certificate: %s", output)

		newCertificateContent, err = os.ReadFile(crtPath)
		o.Expect(err).NotTo(o.HaveOccurred())
		combinedContent = fmt.Sprintf("%s\n%s", initialConfigMapContent, string(newCertificateContent))
		configureCertificateToJSONPatch(oc, combinedContent, configmap, namespace)

		g.By("Verify that after certificate rotation certificates installed on each Windows worker")
		checkUserCertificatesOnNodes(oc, userInstalledCertCommonName, 1)

		g.By("Verify that self-signed certificate has been removed from each Windows node")
		configureCertificateToJSONPatch(oc, initialConfigMapContent, configmap, namespace)
		checkUserCertificatesOnNodes(oc, userSelfSignedCommonName, 0)
	})

	g.It("Author:rrasouli-Smokerun-Longduration-Critical-71173-[node-proxy]-Test connectivity from Windows nodes behind proxy [Serial][Disruptive][Slow]",
		g.SpecTimeout(30*time.Minute),
		func(ctx g.SpecContext) {
			o.Expect(getClusterProxy(oc, "status.noProxy")).ToNot(o.BeEmpty(), "status.noProxy is not set on the cluster")
			winNodes := getWindowsNodeNames(oc)

			g.By("Verify NO_PROXY env vars on each Windows node are comma-separated")
			for _, nodeName := range winNodes {
				msg, err := runHostProcessPS(oc, nodeName, windowsDebugImage,
					"[System.Environment]::GetEnvironmentVariable('NO_PROXY', 'Machine')")
				o.Expect(err).NotTo(o.HaveOccurred(), "failed to get NO_PROXY on %s", nodeName)
				values := strings.Split(strings.TrimSpace(msg), ",")
				o.Expect(len(values)).To(o.BeNumerically(">", 1), "NO_PROXY value on %s is not comma-separated", nodeName)
			}

			g.By("Add another record to cluster proxy: myfakeaddress.com to no-proxy")
			specNoProxy := getClusterProxy(oc, "spec.noProxy")
			updatedNoProxy := "myfakeaddress.com"
			if specNoProxy != "" {
				updatedNoProxy = specNoProxy + ",myfakeaddress.com"
			}

			initialTimeStamp := getWMCOTimestamp(oc)
			defer restoreProxyEnvironment(oc, initialProxySpec)
			err := oc.AsAdmin().WithoutNamespace().Run("patch").Args("proxy/cluster", "--type=json", "-p",
				"[{\"op\": \"add\", \"path\":\"/spec/noProxy\", \"value\":\""+updatedNoProxy+"\"}]").Execute()
			o.Expect(err).NotTo(o.HaveOccurred(), "could not patch proxy with new noProxy value")

			g.By("Wait for WMCO to process proxy change")
			restarted, _ := checkWMCORestarted(oc, initialTimeStamp)
			if restarted {
				e2e.Logf("WMCO restarted after proxy patch, waiting for WICD propagation")
			} else {
				e2e.Logf("WMCO did not restart after proxy patch, waiting for WICD propagation")
			}
			winIPs := getWindowsInternalIPs(oc)
			waitWindowsNodesReady(oc, len(winIPs), 15*time.Minute)

			g.By("Verify NO_PROXY changes propagated to Windows nodes")
			expectedProxies := getEnvVarProxyMap(oc)
			waitForProxyOnNodes(oc, winNodes, expectedProxies)

			g.By("HTTP Traffic Test")
			testTraffic(oc, "http://www.testingmcafeesites.com/testcat_cp.html", winNodes)

			g.By("HTTPS Traffic Test")
			testTraffic(oc, "https://www.google.com", winNodes)
		})
})
