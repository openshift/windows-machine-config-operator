package winc

import (
	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
)

var _ = g.Describe("[OTP][sig-windows] BYOH node pool", func() {
	oc := compat_otp.NewCLIWithoutNamespace("default")
	var fixture *byohTestFixture

	g.BeforeEach(func() {
		fixture = newBYOHTestFixture(oc)
		g.DeferCleanup(fixture.cleanup)
		o.Expect(fixture.requireBYOHPool()).To(o.Succeed())
	})

	g.It("Longduration-High-42496-BYOH InternalDNS registration and deconfiguration [BYOHPool][Serial][Disruptive]", func() {
		g.By("claiming one reusable physical host by its exact DNS registration address")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressDNS})
		o.Expect(err).NotTo(o.HaveOccurred())
		lease := leases[0]

		g.By("registering and then withdrawing only the test-owned DNS entry")
		o.Expect(fixture.registerBYOHInstancesPreservingConfigMap(leases)).To(o.Succeed())
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed())
		o.Expect(fixture.deconfigureLease(lease)).To(o.Succeed())
		o.Expect(fixture.releaseVerified(lease)).To(o.Succeed())
	})

	g.It("Longduration-High-42484-BYOH InternalIP workload and recovery [BYOHPool][Serial][Disruptive]", func() {
		g.By("registering one reusable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP})
		o.Expect(err).NotTo(o.HaveOccurred())
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed())
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed())

		g.By("running five Windows webserver replicas on the selected BYOH host")
		o.Expect(fixture.createBYOHWebServer("winc-42484", leases)).To(o.Succeed())

		g.By("forcing an invalid WMCO version and waiting for workload recovery")
		o.Expect(fixture.invalidateVersionAndWait(lease)).To(o.Succeed())
		o.Expect(fixture.waitForBYOHWebServer(leases)).To(o.Succeed())

		g.By("deleting the BYOH Node and waiting for file transfer, recreation, and workload recovery")
		o.Expect(fixture.deleteNodeAndWaitForRecovery(lease)).To(o.Succeed())
		o.Expect(fixture.waitForBYOHWebServer(leases)).To(o.Succeed())
	})

	g.It("Longduration-High-42516-BYOH mixed InternalIP and InternalDNS physical hosts [BYOHPool][Serial][Disruptive]", func() {
		g.By("claiming two distinct reusable physical hosts, one by IP and one by DNS")
		leases, err := fixture.allocateBYOHHosts(
			byohHostRequest{AddressType: byohAddressIP},
			byohHostRequest{AddressType: byohAddressDNS})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(leases[0].HostID).NotTo(o.Equal(leases[1].HostID))

		g.By("registering both selected hosts and running five Windows webserver replicas")
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed())
		o.Expect(fixture.waitForLeaseNodeReady(leases...)).To(o.Succeed())
		o.Expect(fixture.createBYOHWebServer("winc-42516", leases)).To(o.Succeed())
	})

	g.It("Longduration-High-82694-BYOH IDMS reconciliation and deconfiguration [BYOHPool][Serial][Disruptive]", func() {
		g.By("registering one reusable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP})
		o.Expect(err).NotTo(o.HaveOccurred())
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstancesPreservingConfigMap(leases)).To(o.Succeed())
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed())
		o.Expect(fixture.waitForWMCOLogForLeaseSince(fixture.ctx, lease, "configured", lease.ReconcileStarted)).To(o.Succeed())
		o.Expect(fixture.assertLeasePath(lease, `C:\k\containerd`, true)).To(o.Succeed())

		g.By("creating a synthetic IDMS and proving the containerd mirror prerequisite was reconciled")
		o.Expect(fixture.createSyntheticIDMS(lease)).To(o.Succeed())
		o.Expect(fixture.waitForMirrorPrerequisite(lease)).To(o.Succeed())

		g.By("keeping the IDMS present through deconfiguration and direct containerd-path removal verification")
		o.Expect(fixture.deconfigureLease(lease)).To(o.Succeed())
		o.Expect(fixture.assertLeasePath(lease, `C:\k\containerd`, false)).To(o.Succeed())
		o.Expect(fixture.deleteSyntheticIDMS()).To(o.Succeed())
		o.Expect(fixture.releaseVerified(lease)).To(o.Succeed())
	})

	g.It("Longduration-High-44099-BYOH private-key rotation on a disposable host [BYOHPool][Serial][Disruptive]", func() {
		g.By("registering one isolated disposable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, Disposable: true})
		o.Expect(err).NotTo(o.HaveOccurred())
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed())
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed())

		g.By("rotating the cloud private key and verifying annotation, SSH, and service convergence")
		o.Expect(fixture.rotateBYOHPrivateKey(lease)).To(o.Succeed())

		g.By("restoring the original key and verifying hash, username decryptability, and original-key SSH")
		o.Expect(fixture.restoreBYOHPrivateKey()).To(o.Succeed())
	})
})
