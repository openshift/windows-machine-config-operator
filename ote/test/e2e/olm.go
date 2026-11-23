package winc

import (
	"fmt"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	exutil "github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

// generateSubscriptionYAML returns a Subscription that installs WMCO into namespace from the
// CatalogSource source in sourceNamespace. The channel is copied from the operator's existing
// Subscription: a packagemanifests catalog exposes its only channel without a name, and hardcoding
// "stable" makes OLM fail to resolve the package against it.
func generateSubscriptionYAML(namespace, source, sourceNamespace, channel string) string {
	channelLine := ""
	if channel != "" {
		channelLine = fmt.Sprintf("  channel: %q\n", channel)
	}
	return fmt.Sprintf(`apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: %s
  namespace: %s
spec:
%s  installPlanApproval: Automatic
  name: %s
  source: %s
  sourceNamespace: %s
`, wmcoDeploymentName, namespace, channelLine, wmcoDeploymentName, source, sourceNamespace)
}

// generateOperatorGroupYAML returns an OperatorGroup for WMCO scoped to namespace.
func generateOperatorGroupYAML(namespace string) string {
	return fmt.Sprintf(`apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  annotations:
    olm.providedAPIs: WindowsMachineConfig.v1alpha1.wmc.openshift.io
  name: %s
  namespace: %s
spec:
  targetNamespaces:
  - %s
`, wmcoDeploymentName, namespace, namespace)
}

// wmcoSubscription returns the CatalogSource name and namespace backing the WMCO Subscription in
// namespace. installed is false when WMCO was not installed through OLM, for example when a job
// deploys the operator without a Subscription.
// generateCatalogSourceYAML returns a grpc CatalogSource that points at an existing catalog
// registry. OLM serves a CatalogSource to Subscriptions in its own namespace and in
// openshift-marketplace only, so a namespace scoped catalog is made reachable by copying its address
// into the namespace that needs it.
func generateCatalogSourceYAML(name, namespace, address, displayName string) string {
	return fmt.Sprintf(`apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: %s
  namespace: %s
spec:
  displayName: %s
  publisher: wmco-test
  sourceType: grpc
  address: %s
`, name, namespace, displayName, address)
}

// catalogSourceBacking returns how an existing CatalogSource serves its packages: the grpc address
// of an address-backed catalog, or the image of an image-backed one. A copy of the catalog can
// then be created in another namespace in the same form, because OLM resolves neither form across
// namespaces unless the catalog lives in openshift-marketplace.
func catalogSourceBacking(oc *exutil.CLI, name, namespace string) (address, image string, err error) {
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("catalogsource", name, "-n", namespace,
		"-o=jsonpath={.spec.sourceType}|{.spec.address}|{.spec.image}").Output()
	if err != nil {
		return "", "", fmt.Errorf("failed to read CatalogSource %s/%s: %w", namespace, name, err)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 {
		return "", "", fmt.Errorf("unexpected description of CatalogSource %s/%s: %q", namespace, name, out)
	}
	sourceType := strings.TrimSpace(fields[0])
	address = strings.TrimSpace(fields[1])
	image = strings.TrimSpace(fields[2])
	if sourceType == "grpc" && address != "" {
		return address, "", nil
	}
	if image != "" {
		return "", image, nil
	}
	return "", "", fmt.Errorf("CatalogSource %s/%s has neither a grpc address nor an image", namespace, name)
}

// generateCatalogSourceImageYAML returns an image-backed CatalogSource manifest.
func generateCatalogSourceImageYAML(name, namespace, image, displayName string) string {
	return fmt.Sprintf(`apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: %s
  namespace: %s
spec:
  displayName: %s
  publisher: wmco-test
  sourceType: grpc
  image: %s
`, name, namespace, displayName, image)
}

// waitForCatalogSourceReady waits until OLM reports that it has connected to the catalog.
func waitForCatalogSourceReady(oc *exutil.CLI, namespace, name string, timeout time.Duration) error {
	return wait.Poll(5*time.Second, timeout, func() (bool, error) {
		state, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("catalogsource", name, "-n", namespace,
			"-o=jsonpath={.status.connectionState.lastObservedState}").Output()
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(state) == "READY", nil
	})
}

func wmcoSubscription(oc *exutil.CLI, namespace string) (source, sourceNamespace, channel string, installed bool, err error) {
	filter := ".items[?(@.spec.name=='" + wmcoDeploymentName + "')]"
	out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"subscription", "-n", namespace,
		"-o=jsonpath={range "+filter+"}{.spec.source}{\"|\"}{.spec.sourceNamespace}{\"|\"}{.spec.channel}{end}").Output()
	if err != nil {
		// A failed read says nothing about whether WMCO is installed, so the error is returned and the
		// caller fails instead of skipping the test.
		return "", "", "", false, fmt.Errorf("failed to inspect WMCO Subscriptions in %s: %w", namespace, err)
	}
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 3 || fields[0] == "" || fields[1] == "" {
		e2e.Logf("WMCO in %s is not backed by an OLM Subscription", namespace)
		return "", "", "", false, nil
	}
	return fields[0], fields[1], strings.TrimSpace(fields[2]), true, nil
}

// deleteResourceAndWait deletes a resource and ignores a resource that is already gone.
// An empty namespace targets a cluster scoped resource.
func deleteResourceAndWait(oc *exutil.CLI, resourceType, name, namespace string) error {
	args := []string{resourceType, name, "--ignore-not-found", "--wait=true"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	_, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(args...).Output()
	return err
}

// installWMCO installs WMCO from source into namespace and waits for its Deployment to become
// ready. The Subscription is retried because OLM frequently needs a second attempt after a
// reinstall.
func installWMCO(oc *exutil.CLI, namespace, source, sourceNamespace, channel, privateKeyPath string) {
	g.By(fmt.Sprintf("Installing WMCO in namespace %s from CatalogSource %s", namespace, source))

	// WMCO runs privileged pods, so the namespace must carry the pod security labels that a
	// project created by an ordinary user does not get in a restricted cluster.
	namespaceManifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    security.openshift.io/scc.podSecurityLabelSync: "true"
    pod-security.kubernetes.io/enforce: privileged
    openshift.io/cluster-monitoring: "True"
`, namespace)
	o.Expect(createResourceFromString(oc, "", namespaceManifest)).NotTo(o.HaveOccurred(),
		"Failed to apply the namespace %s", namespace)

	namespaceActiveErr := wait.Poll(5*time.Second, time.Minute, func() (bool, error) {
		phase, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
			"namespace", namespace, "-o=jsonpath={.status.phase}").Output()
		return err == nil && strings.TrimSpace(phase) == "Active", nil
	})
	compat_otp.AssertWaitPollNoErr(namespaceActiveErr, fmt.Sprintf("Namespace %s did not become active", namespace))

	if _, err := oc.AsAdmin().WithoutNamespace().Run("get").Args(
		"secret", "cloud-private-key", "-n", namespace).Output(); err != nil {
		_, err = oc.AsAdmin().WithoutNamespace().Run("create").Args("secret", "generic",
			"cloud-private-key", "--from-file=private-key.pem="+privateKeyPath, "-n", namespace).Output()
		o.Expect(err).NotTo(o.HaveOccurred(), "Failed to create the cloud-private-key secret in %s", namespace)
	}

	installErr := wait.Poll(30*time.Second, 10*time.Minute, func() (bool, error) {
		// OLM keeps the previous CSV around after a reinstall; remove it so the new
		// Subscription resolves a fresh install plan.
		oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"csv", "--all", "-n", namespace, "--ignore-not-found").Execute()
		csvGoneErr := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
			out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", namespace, "-o=name").Output()
			return err == nil && strings.TrimSpace(out) == "", nil
		})
		if csvGoneErr != nil {
			e2e.Logf("Warning: CSVs may still exist in %s after deletion, continuing anyway: %v", namespace, csvGoneErr)
		}
		oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"operatorgroup", wmcoDeploymentName, "-n", namespace, "--ignore-not-found").Execute()
		oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"subscription", wmcoDeploymentName, "-n", namespace, "--ignore-not-found").Execute()

		if err := createResourceFromString(oc, namespace, generateSubscriptionYAML(namespace, source, sourceNamespace, channel)); err != nil {
			e2e.Logf("Error applying the WMCO Subscription: %v", err)
			return false, nil
		}
		if err := createResourceFromString(oc, namespace, generateOperatorGroupYAML(namespace)); err != nil {
			e2e.Logf("Error applying the WMCO OperatorGroup: %v", err)
			return false, nil
		}
		if err := wait.Poll(20*time.Second, 5*time.Minute, func() (bool, error) {
			return checkWorkloadCreated(oc, wmcoDeploymentName, namespace, 1), nil
		}); err != nil {
			e2e.Logf("WMCO deployment did not become ready in %s, retrying: %v", namespace, err)
			oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"subscription", wmcoDeploymentName, "-n", namespace, "--ignore-not-found").Execute()
			oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"operatorgroup", wmcoDeploymentName, "-n", namespace, "--ignore-not-found").Execute()
			oc.AsAdmin().WithoutNamespace().Run("delete").Args(
				"csv", "-l", "operators.coreos.com/"+wmcoDeploymentName, "-n", namespace, "--ignore-not-found").Execute()
			return false, nil
		}

		e2e.Logf("WMCO is installed in namespace %s", namespace)
		return true, nil
	})
	o.Expect(installErr).NotTo(o.HaveOccurred(), "Failed to install WMCO in namespace %s", namespace)
}

// uninstallWMCO removes WMCO's OLM artifacts (CSV, OperatorGroup, Subscription) from namespace.
// The namespace and its CatalogSource are left in place: WMCO CI installs the operator with
// operator-sdk run packagemanifests, whose CatalogSource is an address-based source served from
// that namespace and cannot be recreated from an index image. deleteNamespace is set for the
// temporary namespace used by the reinstall test.
// uninstallWMCOWithError removes WMCO's OLM artifacts and returns any failure, so a caller that has
// to restore the cluster can record the failure and continue with the remaining restore steps.
func uninstallWMCOWithError(oc *exutil.CLI, namespace string, deleteNamespace bool) error {
	e2e.Logf("Uninstalling WMCO from namespace %s", namespace)

	// The Subscription has to go before the CSV, otherwise OLM recreates a CSV that disappears while
	// its Subscription still exists. An install made by operator-sdk names neither the Subscription
	// nor the OperatorGroup the way this test names the objects it creates, so everything is deleted
	// by kind instead of by name.
	g.By(fmt.Sprintf("Deleting WMCO Subscriptions in %s", namespace))
	if _, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
		"subscription", "--all", "-n", namespace, "--ignore-not-found").Output(); err != nil {
		return fmt.Errorf("failed to delete Subscriptions in %s: %w", namespace, err)
	}

	g.By(fmt.Sprintf("Deleting WMCO CSVs in %s", namespace))
	if _, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
		"csv", "--all", "-n", namespace, "--ignore-not-found").Output(); err != nil {
		return fmt.Errorf("failed to delete CSVs in %s: %w", namespace, err)
	}
	csvGoneErr := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("csv", "-n", namespace, "-o=name").Output()
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(out) == "", nil
	})
	if csvGoneErr != nil {
		return fmt.Errorf("CSVs lingered in namespace %s", namespace)
	}

	if _, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
		"operatorgroup", "--all", "-n", namespace, "--ignore-not-found").Output(); err != nil {
		return fmt.Errorf("failed to delete OperatorGroups in %s: %w", namespace, err)
	}

	// The operator pod holds a host port on the control plane node, so a reinstall in another
	// namespace cannot schedule until this deployment is gone.
	deploymentGoneErr := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		out, err := oc.AsAdmin().WithoutNamespace().Run("get").Args("deployment", wmcoDeploymentName,
			"-n", namespace, "--ignore-not-found", "-o=name").Output()
		if err != nil {
			return false, nil
		}
		return strings.TrimSpace(out) == "", nil
	})
	if deploymentGoneErr != nil {
		return fmt.Errorf("WMCO deployment lingered in namespace %s", namespace)
	}

	if deleteNamespace {
		g.By(fmt.Sprintf("Deleting project %s", namespace))
		if _, err := oc.AsAdmin().WithoutNamespace().Run("delete").Args(
			"project", namespace, "--ignore-not-found", "--wait=true", "--timeout=5m").Output(); err != nil {
			return fmt.Errorf("failed to delete project %s: %w", namespace, err)
		}
	}
	e2e.Logf("WMCO uninstalled from namespace %s", namespace)
	return nil
}

func uninstallWMCO(oc *exutil.CLI, namespace string, deleteNamespace bool) {
	o.Expect(uninstallWMCOWithError(oc, namespace, deleteNamespace)).NotTo(o.HaveOccurred())
}
