package winc

import (
	"fmt"
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

	// iaasPlatform is a package level variable set by a BeforeEach of every Describe that reads it.
	// This Describe has its own, otherwise the CSI test sees an empty platform and skips on every
	// cluster, vSphere and Azure included.
	g.BeforeEach(func() {
		output, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("infrastructure", "cluster", "-o=jsonpath={.status.platformStatus.type}").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		iaasPlatform = strings.ToLower(output)
	})

	// author jfrancoa@redhat.com
	g.It("Author:jfrancoa-Smokerun-Critical-66352-Windows workloads support CSI persistent storage [Serial][platform:vsphere][platform:azure][Timeout:45m]", func() {
		// Only vSphere and Azure have a Windows CSI node driver in the WMCO repository.
		if iaasPlatform != "vsphere" && iaasPlatform != "azure" {
			g.Skip(iaasPlatform + " is not implemented yet, skipping")
		}

		namespace := "winc-66352"
		pvcs := []string{"pvc1", "pvc2"}
		installedCSIDriver := false
		// The workloads have to be gone before the CSI node driver is removed: uninstalling the
		// driver first leaves Windows pods that cannot terminate, and their namespace then stays
		// Terminating for hours because the pods are never deleted.
		defer func() {
			deleteProject(oc, namespace)
			if installedCSIDriver {
				if !waitForNamespaceDeleted(oc, namespace, 5*time.Minute) {
					e2e.Logf("Warning: namespace %s still exists; leaving the Windows CSI driver installed", namespace)
					return
				}
				if err := uninstallWindowsCSIDriver(oc, iaasPlatform); err != nil {
					e2e.Logf("Warning: failed to uninstall the Windows CSI driver: %v", err)
				}
			}
		}()
		createProject(oc, namespace)

		driverName, err := windowsCSIDriverName(iaasPlatform)
		o.Expect(err).NotTo(o.HaveOccurred())
		daemonSetInfo, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("daemonset", driverName,
			"-n", csiDriverNamespace, "--ignore-not-found").Output()
		o.Expect(err).NotTo(o.HaveOccurred())
		if strings.TrimSpace(daemonSetInfo) == "" {
			// The pre-check above proved there was no driver before this spec, so the flag is set
			// before the install: if the install aborts the spec, the deferred cleanup still removes
			// the DaemonSet it created.
			installedCSIDriver = true
			g.By("Installing the Windows CSI driver")
			o.Expect(installWindowsCSIDriver(oc, iaasPlatform)).NotTo(o.HaveOccurred())
		}

		g.By("Creating a Windows capable StorageClass")
		storageClassName := "fast-66352"
		storageClass, err := generateStorageClassYAML(iaasPlatform, storageClassName)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(createResourceFromString(oc, "", storageClass)).NotTo(o.HaveOccurred())
		defer func() {
			if err := deleteResourceAndWait(oc, "storageclass", storageClassName, ""); err != nil {
				e2e.Logf("Warning: failed to delete StorageClass %s: %v", storageClassName, err)
			}
		}()

		windowsHosts := getWindowsHostNames(oc)
		if len(windowsHosts) < len(pvcs) {
			e2e.Logf("Only %d Windows node(s) available, limiting PVCs from %d to %d", len(windowsHosts), len(pvcs), len(windowsHosts))
			pvcs = pvcs[:len(windowsHosts)]
		}
		o.Expect(pvcs).NotTo(o.BeEmpty(), "The CSI storage test requires at least one Windows node")

		accessMode := "ReadWriteMany"
		if iaasPlatform == "vsphere" {
			accessMode = "ReadWriteOnce"
		}
		for idx, pvc := range pvcs {
			g.By(fmt.Sprintf("Creating PersistentVolumeClaim %s", pvc))
			o.Expect(createResourceFromString(oc, namespace, generatePVCYAML(pvc, namespace, storageClassName, accessMode, "500Mi"))).
				NotTo(o.HaveOccurred())

			winHost := windowsHosts[idx]
			nodeSelector := ""
			if iaasPlatform == "vsphere" {
				// A ReadWriteOnce claim is only mountable from the node owning the volume, so pin the
				// deployment to its own Windows node.
				g.By(fmt.Sprintf("Labeling Windows node %v for PVC %v", winHost, pvc))
				_, err := oc.AsAdmin().WithoutNamespace().Run("label").Args("node", winHost, "nodepvc="+pvc, "--overwrite").Output()
				o.Expect(err).NotTo(o.HaveOccurred())
				defer func(node string) {
					if out, err := oc.AsAdmin().WithoutNamespace().Run("label").Args("node", node, "nodepvc-").Output(); err != nil {
						e2e.Logf("Warning: failed to remove the nodepvc label from node %s: %v (%s)", node, err, out)
					}
				}(winHost)
				nodeSelector = "nodepvc: " + pvc
			}

			deploymentName := windowsWorkloads + "-" + pvc
			g.By(fmt.Sprintf("Creating Windows deployment %v with persistent volume %v", deploymentName, pvc))
			manifest := generateWindowsWebServerYAML(windowsWebServerConfig{
				Name:           deploymentName,
				Namespace:      namespace,
				Image:          windowsDebugImage,
				Replicas:       3,
				IncludeService: true,
				PVCName:        pvc,
				NodeSelector:   nodeSelector,
			})
			o.Expect(createResourceFromString(oc, namespace, manifest)).NotTo(o.HaveOccurred())
			pollErr := wait.Poll(30*time.Second, 20*time.Minute, func() (bool, error) {
				return checkWorkloadCreated(oc, deploymentName, namespace, 3), nil
			})
			o.Expect(pollErr).NotTo(o.HaveOccurred(), "Windows workload %s is not ready after waiting up to 20 minutes", deploymentName)

			g.By(fmt.Sprintf("Verifying that the CSI volume is mounted in a pod of %s", deploymentName))
			podNames, err := getWorkloadsNames(oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(podNames).NotTo(o.BeEmpty(), "No pods found for %s", deploymentName)
			verifyOut, err := oc.AsAdmin().WithoutNamespace().Run("exec").Args(
				"-n", namespace, podNames[0], "--", "pwsh.exe", "-Command", `Test-Path C:\html\index.html`).Output()
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(verifyOut).To(o.ContainSubstring("True"), "CSI volume not mounted correctly in a pod on node %v", winHost)
		}

		// Checks that run after every PVC exists, so that pods of a claim can be compared with
		// each other and with the service serving them.
		for _, pvc := range pvcs {
			deploymentName := windowsWorkloads + "-" + pvc
			podNames, err := getWorkloadsNames(oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(len(podNames)).To(o.BeNumerically(">=", 1), "Expected at least 1 pod for %s", deploymentName)
			podIPs, err := getWorkloadsIP(oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			// The deployment runs three replicas and each of them has to serve the page the writer
			// pod stores on the claim, so every replica IP is checked below.
			o.Expect(len(podIPs)).To(o.BeNumerically(">=", 3), "Expected a pod IP for each of the 3 replicas of %s", deploymentName)

			g.By("Checking that the service serves the web server content")
			clusterIP, err := getServiceClusterIP(oc, deploymentName, namespace)
			o.Expect(err).NotTo(o.HaveOccurred())
			o.Expect(waitForWebContent(oc, namespace, podNames[0], "http://"+clusterIP,
				"Windows Container Web Server", "Service not ready yet for "+pvc)).NotTo(o.HaveOccurred(),
				"Service %s did not become available", deploymentName)

			modifiedContent := "Windows Container Web Server " + strings.ToUpper(pvc) + " Modified"
			g.By(fmt.Sprintf("Writing %q through pod %s and verifying every replica serves it", modifiedContent, podNames[0]))
			_, err = oc.AsAdmin().WithoutNamespace().Run("exec").Args("-n", namespace, podNames[0], "--", "pwsh.exe",
				"-Command", fmt.Sprintf(`echo "<html><body><H1>%s</H1></body></html>" > C:\html\index.html`, modifiedContent)).Output()
			o.Expect(err).NotTo(o.HaveOccurred())

			o.Expect(waitForWebContent(oc, namespace, podNames[0], "http://"+clusterIP, modifiedContent,
				"Modified content not yet available via service for "+pvc)).NotTo(o.HaveOccurred())
			for _, podIP := range podIPs {
				o.Expect(waitForWebContent(oc, namespace, podNames[0], "http://"+podIP, modifiedContent,
					"Modified content not yet available via pod IP "+podIP+" for "+pvc)).NotTo(o.HaveOccurred())
			}
		}
	})
})
