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
		o.Expect(fixture.requireBYOHPool()).To(o.Succeed(), "the dedicated BYOH inventory must be opted in and trusted before allocation")
	})

	g.It("Longduration-High-42496-BYOH InternalDNS registration and deconfiguration [BYOHPool][Serial][Disruptive]", func() {
		g.By("claiming one reusable physical host by its exact DNS registration address")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressDNS, RequireTrustedSSH: true})
		o.Expect(err).NotTo(o.HaveOccurred(), "claim the reusable DNS host and all of its aliases")
		lease := leases[0]

		g.By("registering and then withdrawing only the test-owned DNS entry")
		o.Expect(fixture.registerBYOHInstancesPreservingConfigMap(leases)).To(o.Succeed(), "register only the owned DNS entry while retaining ConfigMap identity")
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed(), "discover one Ready converged Node for the retained DNS lease")
		o.Expect(fixture.deconfigureLease(lease)).To(o.Succeed(), "withdraw only the owned DNS entry and prove out-of-band reset")
		o.Expect(fixture.releaseVerified(lease)).To(o.Succeed(), "release the reusable host only after verified reset")
	})

	g.It("Longduration-High-42484-BYOH InternalIP workload and recovery [BYOHPool][Serial][Disruptive]", func() {
		g.By("registering one reusable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, RequireTrustedSSH: true})
		o.Expect(err).NotTo(o.HaveOccurred(), "claim the reusable InternalIP host")
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed(), "register only the claimed InternalIP entry")
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed(), "retain the original Ready converged Node identity")

		g.By("running five Windows webserver replicas on the selected BYOH host")
		o.Expect(fixture.createBYOHWebServer("winc-42484", leases)).To(o.Succeed(), "create exactly five current owned Ready pods on the retained Node")

		g.By("forcing an invalid WMCO version and waiting for workload recovery")
		o.Expect(fixture.invalidateVersionAndWait(lease)).To(o.Succeed(), "set invalidVersion through the typed API and wait for stable convergence")
		o.Expect(fixture.waitForBYOHWebServer(leases)).To(o.Succeed(), "restore the five-pod invariant after reconfiguration")

		g.By("deleting the BYOH Node and waiting for file transfer, recreation, and workload recovery")
		o.Expect(fixture.deleteNodeAndWaitForRecovery(lease)).To(o.Succeed(), "delete the retained UID and require a different Ready replacement UID")
		o.Expect(fixture.waitForBYOHWebServer(leases)).To(o.Succeed(), "restore the five-pod invariant on the replacement Node identity")
	})

	g.It("Longduration-High-42516-BYOH mixed InternalIP and InternalDNS physical hosts [BYOHPool][Serial][Disruptive]", func() {
		g.By("claiming two distinct reusable physical hosts, one by IP and one by DNS")
		leases, err := fixture.allocateBYOHHosts(
			byohHostRequest{AddressType: byohAddressIP, RequireTrustedSSH: true},
			byohHostRequest{AddressType: byohAddressDNS, RequireTrustedSSH: true})
		o.Expect(err).NotTo(o.HaveOccurred(), "claim one IP and one DNS host from distinct physical machines")
		o.Expect(leases[0].HostID).NotTo(o.Equal(leases[1].HostID), "OCP-42516 requires two distinct physical hosts")

		g.By("registering both selected hosts and running five Windows webserver replicas")
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed(), "register exactly the selected IP and DNS entries")
		o.Expect(fixture.waitForLeaseNodeReady(leases...)).To(o.Succeed(), "resolve leases to distinct Node names and UIDs")
		o.Expect(fixture.createBYOHWebServer("winc-42516", leases)).To(o.Succeed(), "create five current owned Ready pods constrained to both selected Nodes")
	})

	g.It("Longduration-High-82694-BYOH IDMS reconciliation and deconfiguration [BYOHPool][Serial][Disruptive][apigroup:config.openshift.io]", func() {
		g.By("registering one reusable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, RequireTrustedSSH: true})
		o.Expect(err).NotTo(o.HaveOccurred(), "claim the reusable IDMS test host")
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstancesPreservingConfigMap(leases)).To(o.Succeed(), "register only the owned IP entry while retaining ConfigMap identity")
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed(), "retain the configured Node name and UID")
		o.Expect(fixture.waitForWMCOLogForLeaseSince(fixture.ctx, lease, "configured", lease.ReconcileStarted)).To(o.Succeed(), "require fresh host-specific configured evidence")
		o.Expect(fixture.assertConfiguredPath(fixture.ctx, lease, `C:\k\containerd`, true)).To(o.Succeed(),
			"containerd must exist on the configured retained Node")

		g.By("creating a synthetic IDMS and proving the containerd mirror prerequisite was reconciled")
		o.Expect(fixture.createSyntheticIDMS(lease)).To(o.Succeed(), "create an exactly lease-owned synthetic IDMS")
		o.Expect(fixture.waitForMirrorPrerequisite(lease)).To(o.Succeed(), "prove hosts.toml reconciliation through an owned HostProcess probe")

		g.By("keeping the IDMS present through deconfiguration and direct containerd-path removal verification")
		o.Expect(fixture.deconfigureLease(lease)).To(o.Succeed(), "keep the IDMS present while withdrawing registration and proving reset")
		o.Expect(fixture.assertPostDeconfigurationPathAbsent(fixture.ctx, lease, `C:\k\containerd`)).To(o.Succeed(),
			"trusted out-of-band SSH must prove containerd is absent after Node removal")
		o.Expect(fixture.deleteSyntheticIDMS()).To(o.Succeed(), "UID-delete the IDMS before final host settlement")
		o.Expect(fixture.releaseVerified(lease)).To(o.Succeed(), "release only after reset proof and IDMS deletion")
	})

	g.It("Longduration-High-44099-BYOH private-key rotation on a disposable host [BYOHPool][Serial][Disruptive]", func() {
		g.By("registering one isolated disposable physical host by InternalIP")
		leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP, Disposable: true, RequireTrustedSSH: true})
		o.Expect(err).NotTo(o.HaveOccurred(), "claim the isolated disposable key-rotation host")
		lease := leases[0]
		o.Expect(fixture.registerBYOHInstances(leases)).To(o.Succeed(), "register only the disposable key-rotation host")
		o.Expect(fixture.waitForLeaseNodeReady(lease)).To(o.Succeed(), "retain the disposable host Node UID before rotation")

		g.By("rotating the cloud private key and verifying annotation, SSH, and service convergence")
		o.Expect(fixture.rotateBYOHPrivateKey(lease)).To(o.Succeed(), "rotate through HostProcess placement, Secret UID update, annotation convergence, and direct SSH")

		g.By("restoring the original key and verifying hash, username decryptability, and original-key SSH")
		o.Expect(fixture.restoreBYOHPrivateKey()).To(o.Succeed(), "restore the original Secret key/hash/decryptability and direct SSH authentication")
	})
})
