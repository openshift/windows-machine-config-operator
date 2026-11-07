package winc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	suppliedPool       = "windows-node-pool"
	suppliedInstances  = "windows-instances"
	lifecycleOwner     = "windowsmachineconfig.openshift.io/byoh-lifecycle-owner"
	byohLabel          = "windowsmachineconfig.openshift.io/byoh"
	wmcoVersion        = "windowsmachineconfig.openshift.io/version"
	wmcoDesiredVersion = "windowsmachineconfig.openshift.io/desired-version"
)

type suppliedBYOHLifecycle struct {
	core                     corev1client.CoreV1Interface
	pool, instances          *corev1.ConfigMap
	address, username, owner string
	node                     *corev1.Node
	initialUID               types.UID
	jobID                    string
	checkHost                func(context.Context, string) (string, error)
	mutated                  bool
}

// Only the existing dedicated three-IP handoff is supported. No DNS alias discovery,
// cross-case lease manager or recovery of a previous attempt belongs in this milestone.
// The explicitly disposable host is selected so reusable hosts remain untouched.
func suppliedHost(pool, instances *corev1.ConfigMap) (string, string, error) {
	if pool.UID == "" || instances.UID == "" || len(pool.Data) != 3 || (len(instances.Data) != 3 && len(instances.Data) != 2) {
		return "", "", fmt.Errorf("expected three supplied IP records and two or three existing registrations with object UIDs")
	}
	if owner := pool.Annotations[lifecycleOwner]; owner != "" {
		return "", "", fmt.Errorf("existing BYOH lifecycle ownership prevents adoption of %s", pool.Name)
	}
	if owner := instances.Annotations[lifecycleOwner]; owner != "" {
		return "", "", fmt.Errorf("existing BYOH lifecycle ownership prevents adoption of %s", instances.Name)
	}
	addresses := slices.Sorted(maps.Keys(pool.Data))
	selected, username, disposable := "", "", 0
	for _, address := range addresses {
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil || ip.String() != address {
			return "", "", fmt.Errorf("first lifecycle requires canonical IPv4 inventory, not aliases")
		}
		fields := map[string]string{}
		for _, line := range strings.Split(pool.Data[address], "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			key, value, ok := strings.Cut(line, ":")
			key, value = strings.TrimSpace(key), strings.TrimSpace(value)
			if !ok {
				return "", "", fmt.Errorf("invalid producer record for %s", address)
			}
			if _, duplicate := fields[key]; duplicate {
				return "", "", fmt.Errorf("duplicate producer field %s", key)
			}
			fields[key] = value
		}
		for _, key := range []string{"status", "username", "address-type", "platform", "test-id", "allocated-at", "last-updated", "disposable"} {
			if _, ok := fields[key]; !ok {
				return "", "", fmt.Errorf("producer record %s missing %s", address, key)
			}
		}
		if len(fields) != 8 || (fields["status"] != "available" && fields["status"] != "allocated") || fields["address-type"] != "ip" || fields["platform"] == "" || fields["username"] == "" || len(strings.Fields(fields["username"])) != 1 {
			return "", "", fmt.Errorf("producer record %s is not an unowned available IP host", address)
		}
		if fields["status"] == "available" && (fields["test-id"] != "" || fields["allocated-at"] != "") {
			return "", "", fmt.Errorf("producer record %s is not an unowned available IP host", address)
		}
		if _, err := time.Parse(time.RFC3339, fields["last-updated"]); err != nil {
			return "", "", fmt.Errorf("invalid producer timestamp for %s", address)
		}
		if registration, exists := instances.Data[address]; exists {
			if registration != "username="+fields["username"] {
				return "", "", fmt.Errorf("registration does not match supplied username for %s", address)
			}
		} else if fields["disposable"] != "true" || fields["status"] != "allocated" {
			return "", "", fmt.Errorf("missing registration for non-retry host %s", address)
		}
		switch fields["disposable"] {
		case "true":
			disposable++
			if selected == "" {
				selected, username = address, fields["username"]
			}
		case "false":
		default:
			return "", "", fmt.Errorf("invalid disposable designation for %s", address)
		}
	}
	if disposable != 1 || len(instances.Data) < 2 || len(instances.Data) > 3 {
		return "", "", fmt.Errorf("expected two reusable hosts, one designated disposable host, and a valid retry registration set")
	}
	return selected, username, nil
}

func newSuppliedBYOHLifecycle(ctx context.Context, core corev1client.CoreV1Interface) (*suppliedBYOHLifecycle, error) {
	var job struct {
		ID string `json:"prowjobid"`
	}
	if json.Unmarshal([]byte(os.Getenv("JOB_SPEC")), &job) != nil || !regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`).MatchString(job.ID) || job.ID == "00000000-0000-0000-0000-000000000000" {
		return nil, fmt.Errorf("dedicated lifecycle requires a valid JOB_SPEC.prowjobid")
	}
	f := &suppliedBYOHLifecycle{core: core, jobID: job.ID, owner: job.ID + ":" + string(uuid.NewUUID())}
	f.checkHost = f.checkHostProcess
	cm := core.ConfigMaps(wmcoNamespace)
	var err error
	if f.pool, err = cm.Get(ctx, suppliedPool, metav1.GetOptions{}); err != nil {
		return nil, err
	}
	if f.instances, err = cm.Get(ctx, suppliedInstances, metav1.GetOptions{}); err != nil {
		return nil, err
	}
	if f.address, f.username, err = suppliedHost(f.pool, f.instances); err != nil {
		return nil, err
	}
	if f.node, err = f.selectedNode(ctx); err != nil {
		return nil, err
	}
	_, registered := f.instances.Data[f.address]
	retryAdoption := !registered && strings.Contains(f.pool.Data[f.address], "status: allocated")
	if f.node == nil && !retryAdoption {
		return nil, fmt.Errorf("selected supplied host is not a converged Ready BYOH Node")
	}
	if f.node != nil && !suppliedNodeReady(f.node) {
		return nil, fmt.Errorf("selected supplied host is not a converged Ready BYOH Node")
	}
	if f.node != nil {
		f.initialUID = f.node.UID
	}
	if err = f.noWorkloads(ctx); err != nil {
		return nil, err
	}
	reserved := f.pool.DeepCopy()
	if reserved.Annotations == nil {
		reserved.Annotations = map[string]string{}
	}
	reserved.Annotations[lifecycleOwner] = f.owner
	lines := strings.Split(reserved.Data[f.address], "\n")
	values := map[string]string{"status": "allocated", "test-id": f.owner, "allocated-at": time.Now().UTC().Format(time.RFC3339), "last-updated": time.Now().UTC().Format(time.RFC3339)}
	for i, line := range lines {
		key, _, _ := strings.Cut(line, ":")
		if value, ok := values[strings.TrimSpace(key)]; ok {
			lines[i] = strings.TrimSpace(key) + ": " + value
		}
	}
	reserved.Data[f.address] = strings.Join(lines, "\n")
	// One atomic claim. Retain it until cleanup proves the host is safe to retry.
	if f.pool, err = cm.Update(ctx, reserved, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("pool claim uncertain or conflicted; no host changes attempted: %w", err)
	}
	if f.node != nil {
		e2e.Logf("BYOH lifecycle: reserved host %s, Node %s UID %s until job teardown", f.address, f.node.Name, f.initialUID)
	} else {
		e2e.Logf("BYOH lifecycle: adopted deconfigured retry host %s until job teardown", f.address)
	}
	return f, nil
}

func suppliedNodeReady(node *corev1.Node) bool {
	if _, upgrading := node.Labels["windowsmachineconfig.openshift.io/upgrading"]; upgrading {
		return false
	}
	if node.Labels[byohLabel] != "true" || node.DeletionTimestamp != nil || node.Spec.Unschedulable || node.Annotations[wmcoVersion] == "" {
		return false
	}
	if wanted := node.Annotations[wmcoDesiredVersion]; wanted != "" && wanted != node.Annotations[wmcoVersion] {
		return false
	}
	for key := range node.Annotations {
		if strings.Contains(key, "reboot") {
			return false
		}
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (f *suppliedBYOHLifecycle) selectedNode(ctx context.Context) (*corev1.Node, error) {
	nodes, err := f.core.Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var found *corev1.Node
	for i := range nodes.Items {
		node := &nodes.Items[i]
		for _, address := range node.Status.Addresses {
			if address.Type != corev1.NodeInternalIP || address.Address != f.address {
				continue
			}
			if found != nil || node.UID == "" || node.Labels["kubernetes.io/os"] != "windows" || len(node.OwnerReferences) != 0 || node.Annotations["machine.openshift.io/machine"] != "" {
				return nil, fmt.Errorf("ambiguous or non-BYOH Node for selected IP")
			}
			if node.Labels[byohLabel] != "true" && (f.instances.Annotations[lifecycleOwner] != f.owner || f.instances.Data[f.address] != "username="+f.username) {
				return nil, fmt.Errorf("selected Node is not BYOH and not being configured by this attempt")
			}
			found = node
		}
	}
	if f.node != nil {
		retained, err := f.core.Nodes().Get(ctx, f.node.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err == nil && (retained.UID != f.node.UID || found == nil) {
			return nil, fmt.Errorf("retained Node was replaced or its IP changed")
		}
	}
	if found != nil && f.node != nil && found.UID != f.node.UID {
		return nil, fmt.Errorf("selected Node UID changed unexpectedly")
	}
	return found, nil
}

func (f *suppliedBYOHLifecycle) noWorkloads(ctx context.Context) error {
	pods, err := f.core.Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.Spec.NodeName == "" && (pod.Spec.OS == nil || pod.Spec.OS.Name != corev1.Linux) && pod.Spec.NodeSelector["kubernetes.io/os"] != "linux" {
			return fmt.Errorf("unscheduled workload %s/%s might target the selected host", pod.Namespace, pod.Name)
		}
		if f.node != nil && pod.Spec.NodeName == f.node.Name {
			return fmt.Errorf("selected host has active workload %s/%s; refusing to drain it", pod.Namespace, pod.Name)
		}
	}
	return nil
}

func (f *suppliedBYOHLifecycle) authority(ctx context.Context) error {
	pool, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pool.UID != f.pool.UID || pool.Annotations[lifecycleOwner] != f.owner || pool.Data[f.address] != f.pool.Data[f.address] {
		return fmt.Errorf("selected host reservation changed; refusing mutation")
	}
	return nil
}

func (f *suppliedBYOHLifecycle) setRegistration(ctx context.Context, present bool) error {
	if err := f.authority(ctx); err != nil {
		return err
	}
	cm, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if cm.UID != f.instances.UID || !maps.Equal(cm.Data, f.instances.Data) || cm.Annotations[lifecycleOwner] != f.instances.Annotations[lifecycleOwner] {
		return fmt.Errorf("registration set or ownership changed; refusing update")
	}
	if err := f.authority(ctx); err != nil {
		return err
	}
	node, err := f.selectedNode(ctx)
	if err != nil {
		return err
	}
	if present && node != nil {
		return fmt.Errorf("host is still registered as a Node")
	}
	if !present && f.node == nil {
		f.node = node
	}
	if err := f.noWorkloads(ctx); err != nil {
		return err
	}
	wanted := cm.DeepCopy()
	if wanted.Annotations == nil {
		wanted.Annotations = map[string]string{}
	}
	wanted.Annotations[lifecycleOwner] = f.owner
	if present {
		wanted.Data[f.address] = "username=" + f.username
	} else {
		delete(wanted.Data, f.address)
	}
	f.mutated = true
	f.instances = wanted
	_, updateErr := f.core.ConfigMaps(wmcoNamespace).Update(ctx, wanted, metav1.UpdateOptions{})
	// Read back even an error: a lost response is not proof that the update did not happen.
	current, readErr := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
	if readErr == nil && current.UID == wanted.UID && maps.Equal(current.Data, wanted.Data) && current.Annotations[lifecycleOwner] == f.owner {
		f.instances = current
		return nil
	}
	return errors.Join(fmt.Errorf("registration update not confirmed; reservation retained"), updateErr, readErr)
}

func (f *suppliedBYOHLifecycle) reset(ctx context.Context) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 15*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := f.authority(ctx); err != nil {
			return false, err
		}
		cm, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if cm.UID != f.instances.UID || !maps.Equal(cm.Data, f.instances.Data) || cm.Annotations[lifecycleOwner] != f.owner {
			return false, fmt.Errorf("registration changed during reset")
		}
		node, err := f.selectedNode(ctx)
		if err != nil {
			return false, err
		}
		if node != nil {
			last = fmt.Errorf("waiting for Node UID %s to disappear", node.UID)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return errors.Join(fmt.Errorf("host reset not proved: %w", err), last)
	}
	f.node = nil
	e2e.Logf("BYOH lifecycle: WMCO deconfiguration removed Node for host %s", f.address)
	return nil
}

func (f *suppliedBYOHLifecycle) assertHostConfigured(ctx context.Context) error {
	if _, err := f.checkHost(ctx, lifecycleConfiguredCommand); err != nil {
		return fmt.Errorf("host configured-state check failed: %w", err)
	}
	return nil
}

func (f *suppliedBYOHLifecycle) deconfigure(ctx context.Context) error {
	if err := f.assertHostConfigured(ctx); err != nil {
		return err
	}
	if err := f.setRegistration(ctx, false); err != nil {
		return err
	}
	return f.reset(ctx)
}

func (f *suppliedBYOHLifecycle) register(ctx context.Context) error {
	if err := f.setRegistration(ctx, true); err != nil {
		return err
	}
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 20*time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := f.authority(ctx); err != nil {
			return false, err
		}
		node, err := f.selectedNode(ctx)
		if err != nil || node == nil {
			return false, err
		}
		if node.UID == f.initialUID {
			return false, fmt.Errorf("old Node UID reused instead of a fresh registration")
		}
		f.node = node
		return suppliedNodeReady(node), nil
	})
	if err != nil {
		return fmt.Errorf("selected host did not become Ready: %w", err)
	}
	if _, err := f.checkHost(ctx, lifecycleConfiguredCommand); err != nil {
		return err
	}
	e2e.Logf("BYOH lifecycle: host %s is configured with new Ready Node UID %s", f.address, f.node.UID)
	return nil
}

func (f *suppliedBYOHLifecycle) cleanup(ctx context.Context) error {
	if !f.mutated {
		return nil
	}
	// Never recreate initial registrations or free an uncertain host. Terraform owns final destruction.
	if err := f.deconfigure(ctx); err != nil {
		return fmt.Errorf("cleanup unproved; selected host remains allocated: %w", err)
	}
	if err := f.releaseOwnership(ctx); err != nil {
		return fmt.Errorf("cleanup ownership release failed; selected host remains allocated: %w", err)
	}
	f.mutated = false
	return nil
}

func (f *suppliedBYOHLifecycle) releaseOwnership(ctx context.Context) error {
	instances, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if instances.UID != f.instances.UID || instances.Annotations[lifecycleOwner] != f.owner {
		return fmt.Errorf("instances ownership changed during cleanup; refusing release")
	}
	releasedInstances := instances.DeepCopy()
	delete(releasedInstances.Annotations, lifecycleOwner)
	if len(releasedInstances.Annotations) == 0 {
		releasedInstances.Annotations = nil
	}
	updatedInstances, err := f.core.ConfigMaps(wmcoNamespace).Update(ctx, releasedInstances, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	if updatedInstances.UID != instances.UID || !maps.Equal(updatedInstances.Data, instances.Data) || updatedInstances.Annotations[lifecycleOwner] != "" {
		return fmt.Errorf("instances ownership release not confirmed")
	}
	f.instances = updatedInstances

	pool, err := f.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedPool, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pool.UID != f.pool.UID || pool.Annotations[lifecycleOwner] != f.owner {
		return fmt.Errorf("pool ownership changed during cleanup; refusing release")
	}
	releasedPool := pool.DeepCopy()
	delete(releasedPool.Annotations, lifecycleOwner)
	if len(releasedPool.Annotations) == 0 {
		releasedPool.Annotations = nil
	}
	updatedPool, err := f.core.ConfigMaps(wmcoNamespace).Update(ctx, releasedPool, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	if updatedPool.UID != pool.UID || !maps.Equal(updatedPool.Data, pool.Data) || updatedPool.Annotations[lifecycleOwner] != "" {
		return fmt.Errorf("pool ownership release not confirmed")
	}
	f.pool = updatedPool
	return nil
}
