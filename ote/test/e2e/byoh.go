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
	g.It("OCP-91035 BYOH host lifecycle verification [BYOHPool][Serial][Disruptive]", func(ctx g.SpecContext) {
		if !byohLifecycleOptIn(os.Getenv("TEST_SUITE")) {
			g.Skip("requires the dedicated provisioner-supplied BYOH job")
		}
		lifecycle, err := newSuppliedBYOHLifecycle(ctx, oc.AdminKubeClient().CoreV1())
		o.Expect(err).NotTo(o.HaveOccurred())
		e2e.Logf("BYOH lifecycle: test attempt started for host %s", lifecycle.address)
		g.DeferCleanup(func() {
			if !lifecycle.mutated {
				e2e.Logf("BYOH lifecycle: deferred cleanup skipped; ownership already released for host %s", lifecycle.address)
				return
			}
			e2e.Logf("BYOH lifecycle: deferred cleanup started for host %s", lifecycle.address)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			err := lifecycle.cleanup(cleanupCtx)
			e2e.Logf("BYOH lifecycle: deferred cleanup finished for host %s: %v", lifecycle.address, err)
			o.Expect(err).To(o.Succeed(), "selected host remains reserved for Terraform teardown on failure")
		})
		g.By("Verify the configured host, withdraw its registration, and wait for WMCO teardown")
		o.Expect(lifecycle.deconfigure(ctx)).To(o.Succeed())
		g.By("Register the same host and wait for a new Ready Node")
		o.Expect(lifecycle.register(ctx)).To(o.Succeed())
		g.By("Withdraw the test registration and verify cleanup")
		e2e.Logf("BYOH lifecycle: explicit cleanup started for host %s", lifecycle.address)
		o.Expect(lifecycle.cleanup(ctx)).To(o.Succeed())
		e2e.Logf("BYOH lifecycle: explicit cleanup completed for host %s", lifecycle.address)
	}, g.SpecTimeout(55*time.Minute), g.FlakeAttempts(1))
})

func byohLifecycleOptIn(suite string) bool {
	return suite == "windows-machine-config-operator/byoh-pool"
}
