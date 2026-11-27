package winc

import (
	"context"
	"os"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var _ = g.Describe("[sig-windows] Provisioner-supplied BYOH lifecycle", func() {
	oc := compat_otp.NewCLIWithoutNamespace("byoh-lifecycle")

	// This integration milestone is not the complete OCP-42484 workload/recovery case.
	g.It("Author:rrasouli-OCP-91035 BYOH host lifecycle verification [BYOHPool][Serial][Disruptive]", func(ctx g.SpecContext) {
		if !byohLifecycleOptIn(os.Getenv("TEST_SUITE")) {
			g.Skip("requires the dedicated provisioner-supplied BYOH job")
		}
		lifecycle, err := newSuppliedBYOHLifecycle(ctx, oc.AdminKubeClient().CoreV1())
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to initialize supplied BYOH lifecycle")
		e2e.Logf("BYOH lifecycle: test attempt started for host %s", lifecycle.address)
		g.DeferCleanup(func() {
			if !lifecycle.mutated {
				e2e.Logf("BYOH lifecycle: deferred cleanup skipped; no cleanup action taken for host %s", lifecycle.address)
				return
			}
			e2e.Logf("BYOH lifecycle: deferred cleanup started for host %s", lifecycle.address)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			err := lifecycle.cleanup(cleanupCtx)
			e2e.Logf("BYOH lifecycle: deferred cleanup finished for host %s: %v", lifecycle.address, err)
			o.Expect(err).To(o.Succeed(), "selected host remains reserved for Terraform teardown on failure")
		})
		g.By("Prepare the supplied host for registration")
		o.Expect(lifecycle.prepareForRegistration(ctx)).To(o.Succeed(), "failed to deconfigure the initial registration")
		g.By("Register the same host and wait for a new Ready Node")
		o.Expect(lifecycle.register(ctx)).To(o.Succeed(), "failed to register the host by IP")
		g.By("Withdraw the test registration and verify cleanup")
		e2e.Logf("BYOH lifecycle: explicit cleanup started for host %s", lifecycle.address)
		o.Expect(lifecycle.cleanup(ctx)).To(o.Succeed(), "failed to deconfigure the test registration")
		e2e.Logf("BYOH lifecycle: explicit cleanup completed for host %s", lifecycle.address)
	}, g.SpecTimeout(55*time.Minute), g.FlakeAttempts(1))

	// The Machine API side of a private key change is covered by OCP-39640. This case runs only where no
	// Machine-backed Windows Node exists, so the key change affects BYOH Nodes alone.
	g.It("Author:rrasouli-OCP-44099 BYOH username annotation follows private key replacement [BYOHPool][Serial][Disruptive]", func(ctx g.SpecContext) {
		if !byohLifecycleOptIn(os.Getenv("TEST_SUITE")) {
			g.Skip("requires the dedicated provisioner-supplied BYOH job")
		}
		lifecycle, err := newSuppliedBYOHLifecycle(ctx, oc.AdminKubeClient().CoreV1())
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to initialize supplied BYOH lifecycle")
		e2e.Logf("BYOH key rotation: test attempt started for host %s", lifecycle.address)
		var rotation *byohKeyRotation
		// Registered first so it runs last, after the deferred key restore.
		g.DeferCleanup(func() {
			if !lifecycle.mutated {
				return
			}
			// Releasing ownership with the replacement key still installed would return a
			// key-mutated host to the pool, so retain the reservation until restore is confirmed.
			if rotation != nil && !rotation.restored {
				e2e.Logf("BYOH key rotation: key restoration not confirmed; retaining reservation for host %s", lifecycle.address)
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			err := lifecycle.cleanup(cleanupCtx)
			e2e.Logf("BYOH key rotation: deferred host cleanup finished for host %s: %v", lifecycle.address, err)
			o.Expect(err).To(o.Succeed(), "selected host remains reserved for Terraform teardown on failure")
		})
		if lifecycle.node == nil {
			g.By("Register the deconfigured retry host")
			o.Expect(lifecycle.prepareForRegistration(ctx)).To(o.Succeed(), "failed to prepare the retry host")
			o.Expect(lifecycle.register(ctx)).To(o.Succeed(), "failed to register the retry host")
		} else {
			// The host is deconfigured at the end of this case, so cleanup must run from here on.
			lifecycle.mutated = true
		}

		rotation, err = newBYOHKeyRotation(ctx, oc.AdminKubeClient().CoreV1(), oc.AdminKubeClient().AppsV1(),
			oc.AdminDynamicClient(), lifecycle)
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to initialize BYOH key rotation")
		// Registered before the restore so it runs after every restore attempt.
		g.DeferCleanup(func() { rotation.dispose() })
		g.DeferCleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			// WMCO can only reach the host with the replacement key, so finish any pending
			// deconfiguration before restoring the original key and its annotations.
			if !lifecycle.cleanupDeconfigured {
				if err := lifecycle.deconfigure(cleanupCtx); err != nil {
					e2e.Logf("BYOH key rotation: deferred deconfiguration failed; skipping key restore and retaining host %s: %v", lifecycle.address, err)
					return
				}
				lifecycle.cleanupDeconfigured = true
			}
			err := rotation.restore(cleanupCtx)
			e2e.Logf("BYOH key rotation: deferred key restore finished: %v", err)
			o.Expect(err).To(o.Succeed(), "original private key and BYOH annotations must be restored")
		})

		g.By("Verify no Machine-backed Windows Node exists and BYOH Nodes reflect the original key")
		o.Expect(rotation.verifyPreconditions(ctx)).To(o.Succeed(), "preconditions for key rotation are not met")
		g.By("Authorize the replacement public key on the selected host only")
		o.Expect(rotation.authorizeReplacement(ctx)).To(o.Succeed(), "failed to authorize the replacement key on the selected host")
		g.By("Replace the private key and verify the public key hash and decrypted username annotations")
		o.Expect(rotation.rotate(ctx)).To(o.Succeed(), "failed to rotate the private key and update BYOH annotations")
		g.By("Verify WMCO-managed services on the selected host")
		o.Expect(lifecycle.assertHostConfigured(ctx)).To(o.Succeed(), "WMCO-managed services are not healthy on the selected host")
		g.By("Deconfigure the selected host, which requires WMCO SSH access with the replacement key")
		o.Expect(lifecycle.deconfigure(ctx)).To(o.Succeed(), "failed to deconfigure the selected host with the replacement key")
		lifecycle.cleanupDeconfigured = true
		g.By("Restore the original private key and verify the remaining BYOH Node annotations")
		o.Expect(rotation.restore(ctx)).To(o.Succeed(), "failed to restore the original key and annotations")
		g.By("Release the reservation; the key-mutated host stays allocated for Terraform teardown")
		o.Expect(lifecycle.cleanup(ctx)).To(o.Succeed(), "failed to release the reservation")
	}, g.SpecTimeout(55*time.Minute), g.FlakeAttempts(1))
})

func byohLifecycleOptIn(suite string) bool {
	return suite == "windows-machine-config-operator/byoh-pool"
}
