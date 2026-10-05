package winc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	exutil "github.com/openshift/origin/test/extended/util"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

const (
	byohPoolConfigMap        = "windows-node-pool"
	byohInstancesConfigMap   = "windows-instances"
	byohPoolRequiredEnv      = "WMCO_BYOH_NODE_POOL_REQUIRED"
	byohResetPolicy          = "wmco-deconfigure-v1"
	byohPrivateKeySecret     = "cloud-private-key"
	byohPrivateKeyDataKey    = "private-key.pem"
	byohRegistrationOwner    = "windowsmachineconfig.openshift.io/test-registration-"
	byohPublicKeyHashAnno    = "windowsmachineconfig.openshift.io/pub-key-hash"
	byohUsernameAnno         = "windowsmachineconfig.openshift.io/username"
	byohVersionAnno          = "windowsmachineconfig.openshift.io/version"
	byohDesiredVersionAnno   = "windowsmachineconfig.openshift.io/desired-version"
	byohWorkloadDeployment   = "win-webserver"
	byohOperatorPodSelector  = "name=windows-machine-config-operator"
	byohOperatorContainer    = "manager"
	byohPoolPollTimeout      = 20 * time.Minute
	byohDeconfigurationLimit = 20 * time.Minute
	byohFixtureLimit         = 90 * time.Minute
	byohCleanupLimit         = 30 * time.Minute
	byohFallbackLimit        = 2 * time.Minute
	byohHostProcessCleanup   = 30 * time.Second
)

var byohIDMSGVR = schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1",
	Resource: "imagedigestmirrorsets"}

var errBYOHRegistrationConflict = errors.New("registration address is already present in windows-instances")

// byohInstancesState retains the exact windows-instances object identity and pre-test ownership
// metadata so entry withdrawal cannot replace the object or erase concurrent unrelated state.
type byohInstancesState struct {
	observed          bool
	initiallyExisted  bool
	initialUID        types.UID
	ownedUID          types.UID
	originalOwnership map[string]*string
}

// byohTestFixture owns the bounded lifetime and ordered cleanup state for one serial BYOH case.
type byohTestFixture struct {
	oc                          *exutil.CLI
	coreClient                  corev1client.CoreV1Interface
	appsClient                  appsv1client.AppsV1Interface
	dynamicClient               dynamic.Interface
	ctx                         context.Context
	cancel                      context.CancelFunc
	leases                      []*byohHostLease
	workloadNS                  string
	workloadNSUID               types.UID
	workloadDeploymentAttempted bool
	workloadDeploymentUID       types.UID
	workloadOwnershipNonce      string
	idmsName                    string
	idmsLeaseID                 string
	idmsAttempted               bool
	idmsOwned                   bool
	idmsUID                     types.UID
	instancesState              byohInstancesState
	secretSnapshot              *corev1.Secret
	secretLease                 *byohHostLease
	secretRestored              bool
	originalKeyHash             string
	replacementAuthorizedKey    string
	waitForLeaseLog             func(context.Context, *byohHostLease, string, time.Time) error
	placeAuthorizedKeyForTest   func(context.Context, *byohHostLease, string) error
	removeAuthorizedKeyForTest  func(context.Context, *byohHostLease, string) error
	verifySSHForTest            func(context.Context, *byohHostLease, []byte) error
	configuredServicesForTest   func(context.Context, *byohHostLease) error
	hostProcessLogsForTest      byohPodLogs
	sshClientForTest            func(context.Context, *byohHostLease) (*windowsSSHClient, error)
	versionConvergenceForTest   func(context.Context, corev1client.NodeInterface, *byohHostLease) error
	keyConvergenceForTest       func(context.Context, *byohHostLease, string, []byte, string, bool) error
}

func newBYOHTestFixture(oc *exutil.CLI) *byohTestFixture {
	ctx, cancel := context.WithTimeout(context.Background(), byohFixtureLimit)
	return &byohTestFixture{oc: oc, coreClient: oc.AdminKubeClient().CoreV1(),
		appsClient: oc.AdminKubeClient().AppsV1(), dynamicClient: oc.AdminDynamicClient(), ctx: ctx, cancel: cancel}
}

func wrapOptionalError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func registrationOwnershipAnnotation(address string) string {
	digest := sha256.Sum256([]byte(address))
	return byohRegistrationOwner + fmt.Sprintf("%x", digest[:8])
}

func registrationEntriesOwned(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) bool {
	if cm == nil {
		return false
	}
	if state != nil {
		expectedUID := state.ownedUID
		if expectedUID == "" && state.initiallyExisted {
			expectedUID = state.initialUID
		}
		if expectedUID != "" && cm.UID != expectedUID {
			return false
		}
	}
	for _, lease := range leases {
		if cm.Data[lease.Address] != "username="+lease.Username ||
			cm.Annotations[registrationOwnershipAnnotation(lease.Address)] != lease.LeaseID {
			return false
		}
	}
	return true
}

func registrationObjectMatchesRetainedUID(cm *corev1.ConfigMap, state *byohInstancesState) bool {
	if cm == nil || state == nil {
		return false
	}
	expectedUID := state.ownedUID
	if expectedUID == "" && state.initiallyExisted {
		expectedUID = state.initialUID
	}
	return expectedUID != "" && cm.UID == expectedUID
}

func markRegistrationOwned(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	if cm == nil || cm.UID == "" || !registrationEntriesOwned(cm, leases, state) {
		return fmt.Errorf("windows-instances ownership evidence or object identity does not match")
	}
	for _, lease := range leases {
		if lease.LeaseID == "" {
			return fmt.Errorf("cannot register a BYOH host without a lease identity")
		}
		lease.RegistrationOwned, lease.Registered = true, true
	}
	if state != nil {
		state.ownedUID = cm.UID
	}
	return nil
}

func addRegistrationOwnership(cm *corev1.ConfigMap, leases []*byohHostLease, state *byohInstancesState) error {
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	if state.originalOwnership == nil {
		state.originalOwnership = map[string]*string{}
	}
	for _, lease := range leases {
		key := registrationOwnershipAnnotation(lease.Address)
		if value, exists := cm.Annotations[key]; exists {
			if value == lease.LeaseID && cm.Data[lease.Address] == "username="+lease.Username {
				continue
			}
			return fmt.Errorf("%w: registration ownership metadata already exists", errBYOHRegistrationConflict)
		}
		if _, captured := state.originalOwnership[key]; !captured {
			state.originalOwnership[key] = nil
		}
		cm.Annotations[key] = lease.LeaseID
	}
	return nil
}

func restoreRegistrationOwnership(cm *corev1.ConfigMap, leases []*byohHostLease,
	state *byohInstancesState) error {
	for _, lease := range leases {
		key := registrationOwnershipAnnotation(lease.Address)
		if cm.Annotations[key] != lease.LeaseID {
			return fmt.Errorf("owned windows-instances registration metadata is missing or changed")
		}
		original, captured := state.originalOwnership[key]
		if !captured || original == nil {
			delete(cm.Annotations, key)
		} else {
			cm.Annotations[key] = *original
		}
	}
	if len(cm.Annotations) == 0 {
		cm.Annotations = nil
	}
	return nil
}

func reconcileBYOHRegistration(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState) error {
	var cm *corev1.ConfigMap
	err := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		current, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cm = current
		if !registrationEntriesOwned(cm, leases, state) {
			return fmt.Errorf("windows-instances does not contain the exact attempted registration and ownership evidence")
		}
		return nil
	})
	if err != nil {
		return err
	}
	return markRegistrationOwned(cm, leases, state)
}

func inspectBYOHRegistrationConflict(ctx context.Context, configMaps corev1client.ConfigMapInterface) error {
	return retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		_, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		return err
	})
}

func registerBYOHInstances(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState) error {
	writeAttempted := false
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if state != nil && !state.observed {
			state.observed = true
			if err == nil {
				state.initiallyExisted, state.initialUID = true, cm.UID
			} else if !apierrors.IsNotFound(err) {
				return err
			}
		}
		if apierrors.IsNotFound(err) {
			data := map[string]string{}
			for _, lease := range leases {
				data[lease.Address] = "username=" + lease.Username
			}
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
				Namespace: wmcoNamespace}, Data: data}
			if err := addRegistrationOwnership(cm, leases, state); err != nil {
				return err
			}
			writeAttempted = true
			created, err := configMaps.Create(ctx, cm, metav1.CreateOptions{})
			if err == nil {
				return markRegistrationOwned(created, leases, state)
			}
			return err
		}
		if err != nil {
			return err
		}
		if state != nil && state.initiallyExisted && !registrationObjectMatchesRetainedUID(cm, state) {
			return fmt.Errorf("%w: windows-instances object was replaced during registration",
				errBYOHRegistrationConflict)
		}
		if state != nil && !state.initiallyExisted && writeAttempted {
			if registrationEntriesOwned(cm, leases, state) {
				return markRegistrationOwned(cm, leases, state)
			}
			return fmt.Errorf("%w: windows-instances was concurrently created without exact ownership evidence",
				errBYOHRegistrationConflict)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if writeAttempted && registrationEntriesOwned(cm, leases, state) {
			return markRegistrationOwned(cm, leases, state)
		}
		for _, lease := range leases {
			if _, exists := cm.Data[lease.Address]; exists {
				return errBYOHRegistrationConflict
			}
			cm.Data[lease.Address] = "username=" + lease.Username
		}
		if err := addRegistrationOwnership(cm, leases, state); err != nil {
			return err
		}
		writeAttempted = true
		updated, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return markRegistrationOwned(updated, leases, state)
		}
		return err
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, errBYOHRegistrationConflict) {
		return errors.Join(fmt.Errorf("write windows-instances: %w", err),
			wrapOptionalError("inspect conflicting windows-instances state",
				inspectBYOHRegistrationConflict(ctx, configMaps)))
	}
	if writeAttempted {
		reconcileErr := reconcileBYOHRegistration(ctx, configMaps, leases, state)
		if reconcileErr != nil {
			return errors.Join(fmt.Errorf("write windows-instances: %w", err),
				fmt.Errorf("reconcile ambiguous windows-instances write: %w", reconcileErr))
		}
	}
	return fmt.Errorf("write windows-instances: %w", err)
}

func (f *byohTestFixture) registerBYOHInstances(leases []*byohHostLease) error {
	started := time.Now().UTC()
	for _, lease := range leases {
		lease.RegistrationAttempted = true
	}
	if err := registerBYOHInstances(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), leases,
		&f.instancesState); err != nil {
		for _, lease := range leases {
			if !lease.RegistrationOwned {
				lease.QuarantineRequired = true
			}
		}
		return err
	}
	for _, lease := range leases {
		lease.ReconcileStarted = started
	}
	return nil
}

func (f *byohTestFixture) registerBYOHInstancesPreservingConfigMap(leases []*byohHostLease) error {
	for _, lease := range leases {
		lease.PreserveInstancesConfigMap = true
	}
	return f.registerBYOHInstances(leases)
}

func unregisterBYOHInstances(ctx context.Context, configMaps corev1client.ConfigMapInterface,
	leases []*byohHostLease, state *byohInstancesState, preserveConfigMap bool,
	beforeMutation, mutationProvenUnapplied func()) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		cm, err := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if state == nil || state.ownedUID == "" || cm.UID != state.ownedUID {
			return fmt.Errorf("windows-instances object identity changed before unregister")
		}
		for _, lease := range leases {
			if !lease.RegistrationOwned {
				return fmt.Errorf("refusing to remove an unowned windows-instances entry")
			}
			expected := "username=" + lease.Username
			if value, ok := cm.Data[lease.Address]; !ok || value != expected {
				return fmt.Errorf("owned windows-instances entry is missing or changed")
			}
			delete(cm.Data, lease.Address)
		}
		if err := restoreRegistrationOwnership(cm, leases, state); err != nil {
			return err
		}
		if len(cm.Data) == 0 && !state.initiallyExisted && !preserveConfigMap {
			if beforeMutation != nil {
				beforeMutation()
			}
			return configMaps.Delete(ctx, cm.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
				UID: &state.ownedUID, ResourceVersion: &cm.ResourceVersion}})
		}
		if beforeMutation != nil {
			beforeMutation()
		}
		_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
		return err
	})
	if err == nil {
		return nil
	}
	// A delete/update can be applied even when the response is lost. Only absence of every owned
	// entry is safe evidence that unregister completed; unrelated concurrent data is preserved.
	cm, getErr := configMaps.Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) && state != nil && state.ownedUID != "" && !state.initiallyExisted &&
		!preserveConfigMap {
		return nil
	}
	if getErr == nil {
		if state == nil || cm.UID != state.ownedUID {
			return errors.Join(err, fmt.Errorf("windows-instances object identity changed during unregister"))
		}
		removed := true
		for _, lease := range leases {
			if _, present := cm.Data[lease.Address]; present ||
				cm.Annotations[registrationOwnershipAnnotation(lease.Address)] == lease.LeaseID {
				removed = false
			}
		}
		if removed {
			return nil
		}
		if registrationEntriesOwned(cm, leases, state) && mutationProvenUnapplied != nil {
			mutationProvenUnapplied()
		}
	}
	return errors.Join(err, wrapOptionalError("reconcile ambiguous windows-instances removal", getErr))
}

type byohLookupIP func(context.Context, string) ([]net.IP, error)

type byohDNSLookupError struct{ err error }

func (e *byohDNSLookupError) Error() string {
	return "resolve required DNS registration address: " + e.err.Error()
}
func (e *byohDNSLookupError) Unwrap() error { return e.err }

func leaseIPv4Addresses(ctx context.Context, lease *byohHostLease, lookup byohLookupIP) (map[string]bool, error) {
	wanted := map[string]bool{}
	if lease == nil {
		return wanted, nil
	}
	if lease.AddressType == byohAddressIP {
		wanted[lease.Address] = true
		return wanted, nil
	}
	resolved, err := lookup(ctx, lease.Address)
	if err != nil {
		return nil, &byohDNSLookupError{err: err}
	}
	// WMCO's BYOH contract is IPv4 or DNS resolving to IPv4. AAAA answers are deliberately ignored.
	for _, address := range resolved {
		if ip := address.To4(); ip != nil {
			wanted[ip.String()] = true
		}
	}
	return wanted, nil
}

func nodeMatchesLease(node *corev1.Node, wanted map[string]bool) bool {
	if node == nil {
		return false
	}
	for _, address := range node.Status.Addresses {
		if address.Type == corev1.NodeInternalIP && wanted[address.Address] {
			return true
		}
	}
	return false
}

func exactLeaseNode(ctx context.Context, items []corev1.Node, lease *byohHostLease, lookup byohLookupIP,
	rejectedUID types.UID) (*corev1.Node, bool, error) {
	wanted, err := leaseIPv4Addresses(ctx, lease, lookup)
	if err != nil {
		return nil, false, err
	}
	matches := make([]*corev1.Node, 0, 1)
	for i := range items {
		if nodeMatchesLease(&items[i], wanted) {
			matches = append(matches, &items[i])
		}
	}
	if len(matches) > 1 {
		return nil, false, fmt.Errorf("registration address matches multiple Windows Nodes")
	}
	if len(matches) == 0 || !nodeReadyAndConverged(matches[0]) || matches[0].UID == rejectedUID {
		return nil, false, nil
	}
	return matches[0], true, nil
}

func nodeReadyAndConverged(node *corev1.Node) bool {
	if node == nil || node.Spec.Unschedulable || node.Labels["windowsmachineconfig.openshift.io/byoh"] != "true" {
		return false
	}
	ready := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			ready = condition.Status == corev1.ConditionTrue
		}
	}
	version, desired := node.Annotations[byohVersionAnno], node.Annotations[byohDesiredVersionAnno]
	return ready && version != "" && version == desired
}

func waitForLeaseNodeWithOptions(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease,
	lookup byohLookupIP, rejectedUID types.UID, interval, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true,
		func(ctx context.Context) (bool, error) {
			list, err := nodes.List(ctx, metav1.ListOptions{LabelSelector: windowsNodeLabel})
			if err != nil {
				return false, err
			}
			node, ready, err := exactLeaseNode(ctx, list.Items, lease, lookup, rejectedUID)
			if err != nil {
				var resolverErr *byohDNSLookupError
				if errors.As(err, &resolverErr) {
					return false, nil
				}
				return false, err
			}
			if !ready {
				return false, nil
			}
			lease.NodeName, lease.NodeUID, lease.NodeResourceVersion = node.Name, node.UID, node.ResourceVersion
			return true, nil
		})
}

func waitForLeaseNode(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease,
	rejectedUID types.UID) error {
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	return waitForLeaseNodeWithOptions(ctx, nodes, lease, lookup, rejectedUID, 10*time.Second, byohPoolPollTimeout)
}

func distinctLeaseNodes(leases []*byohHostLease) error {
	names, uids := map[string]bool{}, map[types.UID]bool{}
	for _, lease := range leases {
		if lease.NodeName == "" || lease.NodeUID == "" {
			return fmt.Errorf("selected physical host has no retained Node identity")
		}
		if names[lease.NodeName] || uids[lease.NodeUID] {
			return fmt.Errorf("distinct physical-host leases resolved to the same Node identity")
		}
		names[lease.NodeName], uids[lease.NodeUID] = true, true
	}
	return nil
}

func (f *byohTestFixture) waitForLeaseNodeReady(leases ...*byohHostLease) error {
	for _, lease := range leases {
		if err := waitForLeaseNode(f.ctx, f.coreClient.Nodes(), lease, ""); err != nil {
			lease.QuarantineRequired = true
			return fmt.Errorf("selected BYOH host did not become Ready and converged: %w", err)
		}
	}
	if err := distinctLeaseNodes(leases); err != nil {
		for _, lease := range leases {
			lease.QuarantineRequired = true
		}
		return err
	}
	return nil
}

func waitForNodeUIDGone(ctx context.Context, nodes corev1client.NodeInterface, nodeName string,
	uid types.UID, interval, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		node, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return node.UID != uid, nil
	})
}

func waitForNodeDeleted(ctx context.Context, nodes corev1client.NodeInterface, nodeName string) error {
	if nodeName == "" {
		return fmt.Errorf("cannot wait for deletion of an unidentified Node")
	}
	return wait.PollUntilContextTimeout(ctx, 10*time.Second, byohDeconfigurationLimit, true,
		func(ctx context.Context) (bool, error) {
			_, err := nodes.Get(ctx, nodeName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		})
}

func wmcoLogMatchesLeaseSince(logs string, lease *byohHostLease, message string, since time.Time) bool {
	normalizedMessage := strings.ToLower(strings.ReplaceAll(message, " ", ""))
	for _, line := range strings.Split(logs, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		loggedAt, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil || loggedAt.Before(since) {
			continue
		}
		normalized := strings.ToLower(strings.ReplaceAll(line, " ", ""))
		if strings.Contains(normalized, normalizedMessage) &&
			((lease.NodeName != "" && containsExactLogIdentity(line, lease.NodeName)) ||
				containsExactLogIdentity(line, lease.Address)) {
			return true
		}
	}
	return false
}

func containsExactLogIdentity(line, identity string) bool {
	if identity == "" {
		return false
	}
	for offset := 0; offset <= len(line)-len(identity); {
		index := strings.Index(line[offset:], identity)
		if index < 0 {
			return false
		}
		index += offset
		beforeOK := index == 0 || !isLogIdentityByte(line[index-1])
		after := index + len(identity)
		afterOK := after == len(line) || !isLogIdentityByte(line[after])
		if beforeOK && afterOK {
			return true
		}
		offset = index + 1
	}
	return false
}

func isLogIdentityByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '.' || value == '_' || value == ':' || value == '-'
}

func readWMCOLogsSince(ctx context.Context, coreClient corev1client.CoreV1Interface, since time.Time) (string, error) {
	pods, err := coreClient.Pods(wmcoNamespace).List(ctx, metav1.ListOptions{LabelSelector: byohOperatorPodSelector})
	if err != nil {
		return "", fmt.Errorf("list WMCO operator Pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no WMCO operator Pod matched the fixed selector")
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	var logs strings.Builder
	for i := range pods.Items {
		options := &corev1.PodLogOptions{Container: byohOperatorContainer, Timestamps: true,
			SinceTime: &metav1.Time{Time: since}}
		output, err := coreClient.Pods(wmcoNamespace).GetLogs(pods.Items[i].Name, options).DoRaw(ctx)
		if err != nil {
			return "", fmt.Errorf("read WMCO operator Pod logs: %w", err)
		}
		logs.Write(output)
		logs.WriteByte('\n')
	}
	return logs.String(), nil
}

func waitForWMCOLogForLeaseSince(ctx context.Context, coreClient corev1client.CoreV1Interface,
	lease *byohHostLease, message string, since time.Time) error {
	return wait.PollUntilContextTimeout(ctx, 15*time.Second, byohDeconfigurationLimit, true,
		func(ctx context.Context) (bool, error) {
			logs, err := readWMCOLogsSince(ctx, coreClient, since)
			if err != nil {
				return false, err
			}
			return wmcoLogMatchesLeaseSince(logs, lease, message, since), nil
		})
}

func (f *byohTestFixture) waitForWMCOLogForLeaseSince(ctx context.Context, lease *byohHostLease,
	message string, since time.Time) error {
	if f.waitForLeaseLog != nil {
		return f.waitForLeaseLog(ctx, lease, message, since)
	}
	return waitForWMCOLogForLeaseSince(ctx, f.coreClient, lease, message, since)
}

type byohPodLogs func(context.Context, string, string, types.UID) ([]byte, error)

func detachedContextWithin(ctx context.Context, localCap time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(localCap)
	if outerDeadline, ok := ctx.Deadline(); ok && outerDeadline.Before(deadline) {
		deadline = outerDeadline
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

func hostProcessPodOwnedByAttempt(current, attempted *corev1.Pod, nonce string) bool {
	return current != nil && attempted != nil && current.UID != "" && current.Name == attempted.Name &&
		current.Namespace == attempted.Namespace &&
		current.Labels["windowsmachineconfig.openshift.io/test-id"] ==
			attempted.Labels["windowsmachineconfig.openshift.io/test-id"] &&
		current.Annotations["windowsmachineconfig.openshift.io/host-id"] ==
			attempted.Annotations["windowsmachineconfig.openshift.io/host-id"] &&
		current.Annotations["windowsmachineconfig.openshift.io/ownership-nonce"] == nonce
}

func normalizeHostProcessPodDefaults(spec *corev1.PodSpec) {
	if spec == nil {
		return
	}
	if spec.ServiceAccountName == "default" {
		spec.ServiceAccountName = ""
	}
	if spec.DeprecatedServiceAccount == "default" {
		spec.DeprecatedServiceAccount = ""
	}
	if spec.DNSPolicy == corev1.DNSClusterFirst {
		spec.DNSPolicy = ""
	}
	if spec.SchedulerName == corev1.DefaultSchedulerName {
		spec.SchedulerName = ""
	}
	if spec.TerminationGracePeriodSeconds != nil && *spec.TerminationGracePeriodSeconds == 30 {
		spec.TerminationGracePeriodSeconds = nil
	}
	if spec.EnableServiceLinks != nil && *spec.EnableServiceLinks {
		spec.EnableServiceLinks = nil
	}
	defaultedTolerations := spec.Tolerations[:0]
	for _, toleration := range spec.Tolerations {
		defaultNoExecute := (toleration.Key == "node.kubernetes.io/not-ready" ||
			toleration.Key == "node.kubernetes.io/unreachable") &&
			toleration.Operator == corev1.TolerationOpExists && toleration.Value == "" &&
			toleration.Effect == corev1.TaintEffectNoExecute && toleration.TolerationSeconds != nil &&
			*toleration.TolerationSeconds == 300
		if !defaultNoExecute {
			defaultedTolerations = append(defaultedTolerations, toleration)
		}
	}
	if len(defaultedTolerations) == 0 {
		spec.Tolerations = nil
	} else {
		spec.Tolerations = defaultedTolerations
	}
	for index := range spec.Containers {
		container := &spec.Containers[index]
		if container.TerminationMessagePath == corev1.TerminationMessagePathDefault {
			container.TerminationMessagePath = ""
		}
		if container.TerminationMessagePolicy == corev1.TerminationMessageReadFile {
			container.TerminationMessagePolicy = ""
		}
	}
}

// hostProcessPodCriticalSpecMatches compares the complete privileged execution spec after
// normalizing only documented Pod API defaults. Admission-added code, data, storage, identity,
// scheduling, lifecycle, or security inputs therefore fail closed.
func hostProcessPodCriticalSpecMatches(current, attempted *corev1.Pod) bool {
	if current == nil || attempted == nil || len(current.Spec.Containers) != 1 ||
		len(attempted.Spec.Containers) != 1 || len(current.Spec.InitContainers) != 0 ||
		len(attempted.Spec.InitContainers) != 0 || len(current.Spec.EphemeralContainers) != 0 ||
		len(attempted.Spec.EphemeralContainers) != 0 || len(current.Spec.Volumes) != 0 ||
		len(attempted.Spec.Volumes) != 0 {
		return false
	}
	currentSpec, attemptedSpec := current.Spec.DeepCopy(), attempted.Spec.DeepCopy()
	normalizeHostProcessPodDefaults(currentSpec)
	normalizeHostProcessPodDefaults(attemptedSpec)
	return reflect.DeepEqual(currentSpec, attemptedSpec)
}

// sanitizedPodAPIError preserves only safe request classification. Pod API errors can echo a
// rejected object and therefore must never wrap raw server or transport text containing scripts.
func sanitizedPodAPIError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s (canceled): %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s (timeout): %w", operation, context.DeadlineExceeded)
	}
	reason := apierrors.ReasonForError(err)
	safeReason := "request-failed"
	switch reason {
	case metav1.StatusReasonNotFound:
		safeReason = "not-found"
	case metav1.StatusReasonAlreadyExists:
		safeReason = "already-exists"
	case metav1.StatusReasonConflict:
		safeReason = "conflict"
	case metav1.StatusReasonInvalid:
		safeReason = "invalid"
	case metav1.StatusReasonForbidden:
		safeReason = "forbidden"
	case metav1.StatusReasonUnauthorized:
		safeReason = "unauthorized"
	case metav1.StatusReasonTimeout, metav1.StatusReasonServerTimeout:
		safeReason = "timeout"
	case metav1.StatusReasonTooManyRequests:
		safeReason = "too-many-requests"
	case metav1.StatusReasonInternalError:
		safeReason = "internal-error"
	}
	code := int32(0)
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code = status.Status().Code
	}
	return fmt.Errorf("%s (%s, code %d)", operation, safeReason, code)
}

// runBYOHHostProcessPS runs one bounded SYSTEM probe on a configured retained Node. The Pod name,
// lease labels, Pod UID, and Node UID are verified around every state/log read and UID-preconditioned
// deletion. Script and output content are never included in errors or test logs.
func runBYOHHostProcessPS(ctx context.Context, coreClient corev1client.CoreV1Interface,
	lease *byohHostLease, script string, readLogs byohPodLogs) (output string, retErr error) {
	if lease == nil || lease.NodeName == "" || lease.NodeUID == "" || lease.LeaseID == "" || readLogs == nil {
		return "", fmt.Errorf("incomplete configured-host HostProcess identity")
	}
	node, err := coreClient.Nodes().Get(ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read retained Node before HostProcess probe: %w", err)
	}
	if node.UID != lease.NodeUID {
		return "", fmt.Errorf("refusing HostProcess probe on replacement Node identity")
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate HostProcess ownership suffix: %w", err)
	}
	podName := fmt.Sprintf("winc-byoh-%x", random)
	nonce := fmt.Sprintf("%x", random)
	pods := coreClient.Pods(wmcoNamespace)
	if existing, err := pods.Get(ctx, podName, metav1.GetOptions{}); err == nil {
		return "", fmt.Errorf("refusing to replace pre-existing HostProcess Pod %q (uid %s)", podName, existing.UID)
	} else if !apierrors.IsNotFound(err) {
		return "", sanitizedPodAPIError("check HostProcess Pod ownership", err)
	}
	hostProcess := true
	automountServiceAccountToken := false
	activeDeadlineSeconds := int64(600)
	systemUser := `NT AUTHORITY\SYSTEM`
	labels := map[string]string{
		"windowsmachineconfig.openshift.io/test-id": lease.LeaseID,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: wmcoNamespace, Labels: labels,
		Annotations: map[string]string{"windowsmachineconfig.openshift.io/host-id": lease.HostID,
			"windowsmachineconfig.openshift.io/ownership-nonce": nonce}},
		Spec: corev1.PodSpec{
			NodeName: lease.NodeName, HostNetwork: true, RestartPolicy: corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &activeDeadlineSeconds, AutomountServiceAccountToken: &automountServiceAccountToken,
			OS: &corev1.PodOS{Name: corev1.Windows},
			SecurityContext: &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{
				HostProcess: &hostProcess, RunAsUserName: &systemUser}},
			Tolerations: []corev1.Toleration{{Key: "os", Operator: corev1.TolerationOpEqual,
				Value: "Windows", Effect: corev1.TaintEffectNoSchedule}},
			Containers: []corev1.Container{{Name: "probe", Image: windowsDebugImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script},
				SecurityContext: &corev1.SecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{
					HostProcess: &hostProcess, RunAsUserName: &systemUser}}}},
		}}
	created, createErr := pods.Create(ctx, pod, metav1.CreateOptions{})
	if createErr != nil {
		reconcileCtx, cancel := detachedContextWithin(ctx, byohHostProcessCleanup)
		current, getErr := pods.Get(reconcileCtx, podName, metav1.GetOptions{})
		cancel()
		if getErr != nil || !hostProcessPodOwnedByAttempt(current, pod, nonce) {
			lease.QuarantineRequired = true
			return "", errors.Join(sanitizedPodAPIError("create owned HostProcess Pod", createErr),
				sanitizedPodAPIError("reconcile ambiguous HostProcess Pod creation", getErr))
		}
		created = current
	}
	podUID := created.UID
	defer func() {
		cleanupCtx, cancel := detachedContextWithin(ctx, byohHostProcessCleanup)
		defer cancel()
		current, err := pods.Get(cleanupCtx, podName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			lease.QuarantineRequired = true
			retErr = errors.Join(retErr, sanitizedPodAPIError("read owned HostProcess Pod for cleanup", err))
			return
		}
		if current.Name != podName || current.UID != podUID {
			lease.QuarantineRequired = true
			retErr = errors.Join(retErr, fmt.Errorf("refusing to delete replacement HostProcess Pod"))
			return
		}
		if err := pods.Delete(cleanupCtx, podName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
			UID: &podUID, ResourceVersion: &current.ResourceVersion}}); err != nil && !apierrors.IsNotFound(err) {
			lease.QuarantineRequired = true
			retErr = errors.Join(retErr, sanitizedPodAPIError("delete owned HostProcess Pod", err))
		}
	}()
	if !hostProcessPodOwnedByAttempt(created, pod, nonce) || !hostProcessPodCriticalSpecMatches(created, pod) {
		lease.QuarantineRequired = true
		return "", fmt.Errorf("created HostProcess Pod does not match the retained privileged attempt")
	}
	var completed *corev1.Pod
	err = wait.PollUntilContextTimeout(ctx, time.Second, 10*time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := pods.Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, sanitizedPodAPIError("read owned HostProcess Pod while running", err)
		}
		if current.UID != podUID || !hostProcessPodOwnedByAttempt(current, pod, nonce) ||
			!hostProcessPodCriticalSpecMatches(current, pod) {
			lease.QuarantineRequired = true
			return false, fmt.Errorf("HostProcess Pod identity changed while running")
		}
		switch current.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			completed = current
			return true, nil
		default:
			return false, nil
		}
	})
	if err != nil {
		lease.QuarantineRequired = true
		return "", fmt.Errorf("wait for owned HostProcess Pod: %w", err)
	}
	if completed.Status.Phase != corev1.PodSucceeded {
		lease.QuarantineRequired = true
		return "", fmt.Errorf("owned HostProcess probe failed")
	}
	node, err = coreClient.Nodes().Get(ctx, lease.NodeName, metav1.GetOptions{})
	if err != nil {
		lease.QuarantineRequired = true
		return "", fmt.Errorf("read retained Node after HostProcess probe: %w", err)
	}
	if node.UID != lease.NodeUID {
		lease.QuarantineRequired = true
		return "", fmt.Errorf("retained Node identity changed during HostProcess probe")
	}
	logs, err := readLogs(ctx, wmcoNamespace, podName, podUID)
	if err != nil {
		lease.QuarantineRequired = true
		return "", sanitizedPodAPIError("read owned HostProcess Pod output", err)
	}
	current, err := pods.Get(ctx, podName, metav1.GetOptions{})
	if err != nil || current.UID != podUID || !hostProcessPodOwnedByAttempt(current, pod, nonce) ||
		!hostProcessPodCriticalSpecMatches(current, pod) {
		lease.QuarantineRequired = true
		return "", errors.Join(fmt.Errorf("HostProcess Pod identity changed during log read"),
			sanitizedPodAPIError("re-read HostProcess Pod after log retrieval", err))
	}
	return strings.TrimSpace(string(logs)), nil
}

func (f *byohTestFixture) hostProcessLogs(ctx context.Context, namespace, podName string,
	podUID types.UID) ([]byte, error) {
	pod, err := f.coreClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, sanitizedPodAPIError("read owned HostProcess Pod before logs", err)
	}
	if pod.Name != podName || pod.Namespace != namespace || pod.UID != podUID {
		return nil, fmt.Errorf("HostProcess Pod identity changed before log read")
	}
	logs, err := f.coreClient.Pods(namespace).GetLogs(podName,
		&corev1.PodLogOptions{Container: "probe"}).DoRaw(ctx)
	if err != nil {
		return nil, sanitizedPodAPIError("request owned HostProcess Pod logs", err)
	}
	pod, err = f.coreClient.Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, sanitizedPodAPIError("read owned HostProcess Pod after logs", err)
	}
	if pod.Name != podName || pod.Namespace != namespace || pod.UID != podUID {
		return nil, fmt.Errorf("HostProcess Pod identity changed during log read")
	}
	return logs, nil
}

func (f *byohTestFixture) runConfiguredHostProcessPS(ctx context.Context, lease *byohHostLease,
	script string) (string, error) {
	readLogs := f.hostProcessLogs
	if f.hostProcessLogsForTest != nil {
		readLogs = f.hostProcessLogsForTest
	}
	return runBYOHHostProcessPS(ctx, f.coreClient, lease, script, readLogs)
}

func configuredManagedServicesRunningScript() string {
	return "$names=@('" + strings.Join(byohManagedServices, "','") +
		"'); foreach($name in $names){$svc=Get-Service -Name $name -ErrorAction Stop; if($svc.Status -ne 'Running'){exit 1}}"
}

func (f *byohTestFixture) assertConfiguredServicesRunning(ctx context.Context, lease *byohHostLease) error {
	if f.configuredServicesForTest != nil {
		return f.configuredServicesForTest(ctx, lease)
	}
	_, err := f.runConfiguredHostProcessPS(ctx, lease, configuredManagedServicesRunningScript())
	return err
}

func (f *byohTestFixture) assertConfiguredPath(ctx context.Context, lease *byohHostLease,
	path string, present bool) error {
	expected := "$false"
	if present {
		expected = "$true"
	}
	_, err := f.runConfiguredHostProcessPS(ctx, lease, fmt.Sprintf(
		"if((Test-Path -LiteralPath '%s') -ne %s){exit 1}", strings.ReplaceAll(path, "'", "''"), expected))
	return err
}

func normalizeAuthorizedKeyLine(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("replacement authorized key must be one normalized OpenSSH line")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(value))
	if err != nil || comment != "" || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", fmt.Errorf("replacement authorized key must be one normalized OpenSSH line")
	}
	normalized := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n")
	if value != normalized {
		return "", fmt.Errorf("replacement authorized key must be one normalized OpenSSH line")
	}
	return normalized, nil
}

type authorizedKeyLine struct {
	value, ending string
}

// mutateAuthorizedKeyContent is the production mutation contract: it preserves UTF-8 BOM,
// unrelated bytes, and each unrelated line ending while changing only exact case-sensitive owned
// lines. Placement collapses owned duplicates to one; removal deletes every exact owned line.
func mutateAuthorizedKeyContent(content []byte, authorizedKey string, remove bool) ([]byte, error) {
	normalized, err := normalizeAuthorizedKeyLine(authorizedKey)
	if err != nil {
		return nil, err
	}
	bom := []byte(nil)
	body := content
	if bytes.HasPrefix(body, []byte{0xef, 0xbb, 0xbf}) {
		bom, body = append([]byte(nil), body[:3]...), body[3:]
	}
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("authorized-key file is not valid UTF-8")
	}
	text := string(body)
	lines := []authorizedKeyLine{}
	newline := ""
	for len(text) > 0 {
		index := strings.IndexByte(text, '\n')
		if index < 0 {
			lines = append(lines, authorizedKeyLine{value: text})
			break
		}
		value, ending := text[:index], "\n"
		if strings.HasSuffix(value, "\r") {
			value, ending = strings.TrimSuffix(value, "\r"), "\r\n"
		}
		if newline == "" {
			newline = ending
		}
		lines = append(lines, authorizedKeyLine{value: value, ending: ending})
		text = text[index+1:]
	}
	if newline == "" {
		newline = "\r\n"
	}
	var result strings.Builder
	found := false
	for _, line := range lines {
		if line.value == normalized {
			if remove || found {
				continue
			}
			found = true
		}
		result.WriteString(line.value)
		result.WriteString(line.ending)
	}
	if !remove && !found {
		if result.Len() > 0 && !strings.HasSuffix(result.String(), "\n") {
			result.WriteString(newline)
		}
		result.WriteString(normalized)
		if len(body) == 0 || bytes.HasSuffix(body, []byte("\n")) {
			result.WriteString(newline)
		}
	}
	return append(bom, []byte(result.String())...), nil
}

func (f *byohTestFixture) placeAuthorizedKeyWithHostProcess(ctx context.Context, lease *byohHostLease,
	authorizedKey string) error {
	normalized, err := normalizeAuthorizedKeyLine(authorizedKey)
	if err != nil {
		return err
	}
	if f.placeAuthorizedKeyForTest != nil {
		err := f.placeAuthorizedKeyForTest(ctx, lease, normalized)
		if err != nil {
			lease.QuarantineRequired = true
		}
		return err
	}
	return f.mutateAuthorizedKeyWithHostProcess(ctx, lease, normalized, false)
}

func (f *byohTestFixture) mutateAuthorizedKeyWithHostProcess(ctx context.Context, lease *byohHostLease,
	authorizedKey string, remove bool) error {
	readScript := `$ErrorActionPreference='Stop'; $path="$env:ProgramData\ssh\administrators_authorized_keys"; ` +
		`$bytes=if(Test-Path -LiteralPath $path -ErrorAction Stop){[IO.File]::ReadAllBytes($path)}else{[byte[]]@()}; ` +
		`[Convert]::ToBase64String($bytes)`
	encodedCurrent, err := f.runConfiguredHostProcessPS(ctx, lease, readScript)
	if err != nil {
		lease.QuarantineRequired = true
		return err
	}
	current, err := base64.StdEncoding.DecodeString(encodedCurrent)
	if err != nil {
		lease.QuarantineRequired = true
		return fmt.Errorf("decode owned authorized-key file snapshot")
	}
	desired, err := mutateAuthorizedKeyContent(current, authorizedKey, remove)
	if err != nil {
		return err
	}
	before := base64.StdEncoding.EncodeToString(current)
	after := base64.StdEncoding.EncodeToString(desired)
	writeScript := fmt.Sprintf(`$ErrorActionPreference='Stop'; $path="$env:ProgramData\ssh\administrators_authorized_keys"; `+
		`$current=if(Test-Path -LiteralPath $path -ErrorAction Stop){[IO.File]::ReadAllBytes($path)}else{[byte[]]@()}; `+
		`if([Convert]::ToBase64String($current) -cne '%s'){exit 3}; $desired=[Convert]::FromBase64String('%s'); `+
		`if([Convert]::ToBase64String($current) -cne '%s'){[IO.File]::WriteAllBytes($path,$desired)}; `+
		`$final=if(Test-Path -LiteralPath $path -ErrorAction Stop){[IO.File]::ReadAllBytes($path)}else{[byte[]]@()}; `+
		`if([Convert]::ToBase64String($final) -cne '%s'){exit 1}`,
		before, after, after, after)
	_, err = f.runConfiguredHostProcessPS(ctx, lease, writeScript)
	if err != nil {
		lease.QuarantineRequired = true
	}
	return err
}

func (f *byohTestFixture) removeAuthorizedKeyWithHostProcess(ctx context.Context, lease *byohHostLease,
	authorizedKey string) error {
	normalized, err := normalizeAuthorizedKeyLine(authorizedKey)
	if err != nil {
		return err
	}
	if f.removeAuthorizedKeyForTest != nil {
		err := f.removeAuthorizedKeyForTest(ctx, lease, normalized)
		if err != nil {
			lease.QuarantineRequired = true
		}
		return err
	}
	return f.mutateAuthorizedKeyWithHostProcess(ctx, lease, normalized, true)
}

func (f *byohTestFixture) invalidateVersionAndWait(lease *byohHostLease) error {
	if lease == nil || lease.NodeName == "" || lease.NodeUID == "" {
		return fmt.Errorf("cannot invalidate version without retained Node identity")
	}
	var updated *corev1.Node
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := f.coreClient.Nodes().Get(f.ctx, lease.NodeName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("refresh retained Node before version invalidation: %w", err)
		}
		if node.UID != lease.NodeUID || node.ResourceVersion == "" {
			return fmt.Errorf("retained Node identity changed before version invalidation")
		}
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}
		node.Annotations[byohVersionAnno] = "invalidVersion"
		updated, err = f.coreClient.Nodes().Update(f.ctx, node, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("set invalidVersion on retained Node: %w", err)
	}
	if updated == nil || updated.UID != lease.NodeUID || updated.ResourceVersion == "" ||
		updated.Annotations[byohVersionAnno] != "invalidVersion" {
		return fmt.Errorf("retained Node identity changed during version invalidation")
	}
	lease.NodeResourceVersion = updated.ResourceVersion
	if f.versionConvergenceForTest != nil {
		return f.versionConvergenceForTest(f.ctx, f.coreClient.Nodes(), lease)
	}
	return waitForStableNodeConvergence(f.ctx, f.coreClient.Nodes(), lease, 10*time.Second, 20*time.Minute)
}

func waitForStableNodeConvergence(ctx context.Context, nodes corev1client.NodeInterface, lease *byohHostLease,
	interval, timeout time.Duration) error {
	stable := 0
	return wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		node, err := nodes.Get(ctx, lease.NodeName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if node.UID != lease.NodeUID {
			return false, fmt.Errorf("retained Node identity changed during version convergence")
		}
		_, upgrading := node.Labels["windowsmachineconfig.openshift.io/upgrading"]
		_, rebootRequired := node.Annotations["windowsmachineconfig.openshift.io/reboot-required"]
		if !nodeReadyAndConverged(node) || upgrading || rebootRequired {
			stable = 0
			return false, nil
		}
		stable++
		lease.NodeResourceVersion = node.ResourceVersion
		return stable >= reconfigurationStableChecks, nil
	})
}

func (f *byohTestFixture) deleteNodeAndWaitForRecovery(lease *byohHostLease) error {
	if lease.NodeName == "" || lease.NodeUID == "" {
		return fmt.Errorf("cannot prove replacement of an unidentified Node")
	}
	originalName, originalUID := lease.NodeName, lease.NodeUID
	started := time.Now().UTC()
	if err := deleteNodeByRetainedIdentity(f.ctx, f.coreClient.Nodes(), originalName, originalUID); err != nil {
		lease.QuarantineRequired = true
		return err
	}
	if err := waitForNodeUIDGone(f.ctx, f.coreClient.Nodes(), originalName, originalUID,
		10*time.Second, byohPoolPollTimeout); err != nil {
		lease.QuarantineRequired = true
		return fmt.Errorf("original BYOH Node UID did not disappear: %w", err)
	}
	if err := f.waitForWMCOLogForLeaseSince(f.ctx, lease, "transferring files", started); err != nil {
		lease.QuarantineRequired = true
		return err
	}
	lease.NodeName, lease.NodeUID, lease.NodeResourceVersion = "", "", ""
	if err := waitForLeaseNode(f.ctx, f.coreClient.Nodes(), lease, originalUID); err != nil {
		lease.QuarantineRequired = true
		return fmt.Errorf("replacement BYOH Node did not become Ready and converged: %w", err)
	}
	return nil
}

func deleteNodeByRetainedIdentity(ctx context.Context, nodes corev1client.NodeInterface, name string,
	uid types.UID) error {
	if name == "" || uid == "" {
		return fmt.Errorf("cannot delete a Node without retained identity")
	}
	current, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("refresh retained Node identity before deletion: %w", err)
	}
	if current.UID != uid {
		return fmt.Errorf("refusing to delete replacement Node with changed UID")
	}
	if current.ResourceVersion == "" || !nodeReadyAndConverged(current) {
		return fmt.Errorf("refusing to delete retained Node before it is Ready and version-converged")
	}
	return nodes.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &current.ResourceVersion}})
}

func (f *byohTestFixture) createSyntheticIDMS(lease *byohHostLease) error {
	suffix := strings.TrimPrefix(lease.LeaseID, "winc-1974-")
	name := "winc-82694-" + suffix
	f.idmsName, f.idmsLeaseID = name, lease.LeaseID
	ownership, err := createOwnedIDMS(f.ctx, f.dynamicClient.Resource(byohIDMSGVR), name, lease.LeaseID)
	f.idmsAttempted, f.idmsOwned, f.idmsUID = ownership.attempted, ownership.owned, ownership.uid
	return err
}

func idmsOwnedByLease(object *unstructured.Unstructured, leaseID string) bool {
	return object != nil && object.GetAnnotations()["windowsmachineconfig.openshift.io/test-id"] == leaseID
}

type byohIDMSOwnership struct {
	attempted bool
	owned     bool
	uid       types.UID
}

func createOwnedIDMS(ctx context.Context, resource dynamic.ResourceInterface, name, leaseID string) (byohIDMSOwnership, error) {
	if existing, err := resource.Get(ctx, name, metav1.GetOptions{}); err == nil {
		return byohIDMSOwnership{}, fmt.Errorf("refusing to replace pre-existing ImageDigestMirrorSet %q (uid %s)",
			name, existing.GetUID())
	} else if !apierrors.IsNotFound(err) {
		return byohIDMSOwnership{}, fmt.Errorf("check synthetic ImageDigestMirrorSet ownership: %w", err)
	}
	object := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "ImageDigestMirrorSet",
		"metadata": map[string]interface{}{
			"name": name,
			"annotations": map[string]interface{}{
				"windowsmachineconfig.openshift.io/test-id": leaseID,
			},
		},
		"spec": map[string]interface{}{
			"imageDigestMirrors": []interface{}{map[string]interface{}{
				"source":  "registry.redhat.io/openshift4",
				"mirrors": []interface{}{"example.com/my-mirror/openshift"},
			}},
		},
	}}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err == nil {
		if created.GetUID() == "" {
			return byohIDMSOwnership{attempted: true}, fmt.Errorf("created synthetic ImageDigestMirrorSet has no UID")
		}
		return byohIDMSOwnership{attempted: true, owned: true, uid: created.GetUID()}, nil
	}
	ownership := byohIDMSOwnership{attempted: true}
	var current *unstructured.Unstructured
	getErr := retry.OnError(retry.DefaultBackoff, func(error) bool { return true }, func() error {
		candidate, candidateErr := resource.Get(ctx, name, metav1.GetOptions{})
		if candidateErr != nil {
			return candidateErr
		}
		if !idmsOwnedByLease(candidate, leaseID) || candidate.GetUID() == "" {
			return fmt.Errorf("synthetic ImageDigestMirrorSet does not contain exact lease ownership evidence")
		}
		current = candidate
		return nil
	})
	if getErr == nil {
		ownership.owned, ownership.uid = true, current.GetUID()
		return ownership, fmt.Errorf("create synthetic ImageDigestMirrorSet returned an ambiguous error: %w", err)
	}
	return ownership, errors.Join(fmt.Errorf("create synthetic ImageDigestMirrorSet: %w", err),
		fmt.Errorf("reconcile ambiguous ImageDigestMirrorSet creation: %w", getErr))
}

func (f *byohTestFixture) waitForMirrorPrerequisite(lease *byohHostLease) error {
	// This proves only that WMCO reconciled the generated hosts.toml. The placeholder mirror is
	// deliberately never contacted, so this is not a mirrored image-pull test.
	return wait.PollUntilContextTimeout(f.ctx, 15*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			output, err := f.runConfiguredHostProcessPS(ctx, lease,
				`$p='C:\k\containerd\registries\registry.redhat.io\hosts.toml'; if(Test-Path $p){Get-Content -Raw $p}`)
			if err != nil {
				return false, err
			}
			return strings.Contains(output, "example.com/my-mirror/openshift") &&
				strings.Contains(output, "registry.redhat.io"), nil
		})
}

func (f *byohTestFixture) deleteSyntheticIDMSWithContext(ctx context.Context) error {
	if f.idmsName == "" {
		return nil
	}
	if !f.idmsAttempted && !f.idmsOwned {
		f.idmsName, f.idmsLeaseID = "", ""
		return nil
	}
	resource := f.dynamicClient.Resource(byohIDMSGVR)
	current, err := resource.Get(ctx, f.idmsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		f.idmsName, f.idmsLeaseID, f.idmsAttempted, f.idmsOwned, f.idmsUID = "", "", false, false, ""
		return nil
	}
	if err != nil {
		return err
	}
	if !idmsOwnedByLease(current, f.idmsLeaseID) {
		return fmt.Errorf("synthetic ImageDigestMirrorSet ownership changed")
	}
	if f.idmsUID != "" && current.GetUID() != f.idmsUID {
		return fmt.Errorf("synthetic ImageDigestMirrorSet object identity changed")
	}
	if current.GetUID() == "" {
		return fmt.Errorf("synthetic ImageDigestMirrorSet has no retained object identity")
	}
	if f.idmsUID == "" {
		f.idmsUID = current.GetUID()
	}
	uid, resourceVersion := f.idmsUID, current.GetResourceVersion()
	if err := resource.Delete(ctx, f.idmsName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion}}); err != nil &&
		!apierrors.IsNotFound(err) {
		return err
	}
	f.idmsName, f.idmsLeaseID, f.idmsAttempted, f.idmsOwned, f.idmsUID = "", "", false, false, ""
	return nil
}

func (f *byohTestFixture) deleteSyntheticIDMS() error {
	return f.deleteSyntheticIDMSWithContext(f.ctx)
}

func (f *byohTestFixture) verifyLeaseDeconfigured(ctx context.Context, lease *byohHostLease) (retErr error) {
	defer func() {
		if retErr != nil {
			lease.QuarantineRequired = true
		}
	}()
	if lease.DeconfigurationStarted.IsZero() {
		return fmt.Errorf("deconfiguration mutation timestamp was not captured")
	}
	for _, message := range []string{"deconfiguring", "removing directories", "instance has been deconfigured"} {
		if err := f.waitForWMCOLogForLeaseSince(ctx, lease, message, lease.DeconfigurationStarted); err != nil {
			return err
		}
	}
	if err := waitForNodeDeleted(ctx, f.coreClient.Nodes(), lease.NodeName); err != nil {
		return err
	}
	client, err := f.sshClient(ctx, lease)
	if err != nil {
		return err
	}
	if err := assertManagedServicesStopped(ctx, client); err != nil {
		return errors.Join(err, wrapOptionalError("close post-deconfiguration SSH client", client.close()))
	}
	if err := assertManagedDirectoriesRemoved(ctx, client); err != nil {
		return errors.Join(err, wrapOptionalError("close post-deconfiguration SSH client", client.close()))
	}
	if err := client.close(); err != nil {
		return fmt.Errorf("close post-deconfiguration SSH client: %w", err)
	}
	lease.ResetVerified = true
	return nil
}

func captureDeconfigurationBoundary(lease *byohHostLease) {
	lease.DeconfigurationStarted = time.Now().UTC()
}

func clearDeconfigurationBoundary(lease *byohHostLease) {
	lease.DeconfigurationStarted = time.Time{}
}

func runLeaseDeconfiguration(lease *byohHostLease, mutate, verify func() error) error {
	if !lease.RegistrationOwned {
		return fmt.Errorf("cannot deconfigure a registration that is not provably owned")
	}
	if !lease.Unregistered {
		if err := mutate(); err != nil {
			return err
		}
		if lease.DeconfigurationStarted.IsZero() {
			return fmt.Errorf("deconfiguration mutation timestamp was not captured")
		}
		lease.Registered, lease.Unregistered = false, true
	}
	return verify()
}

func (f *byohTestFixture) deconfigureLeaseWithContext(ctx context.Context, lease *byohHostLease) error {
	mutate := func() error {
		return unregisterBYOHInstances(ctx, f.coreClient.ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, &f.instancesState, lease.PreserveInstancesConfigMap,
			func() { captureDeconfigurationBoundary(lease) },
			func() { clearDeconfigurationBoundary(lease) })
	}
	return runLeaseDeconfiguration(lease, mutate, func() error { return f.verifyLeaseDeconfigured(ctx, lease) })
}

func (f *byohTestFixture) deconfigureLease(lease *byohHostLease) error {
	return f.deconfigureLeaseWithContext(f.ctx, lease)
}

func (f *byohTestFixture) releaseVerified(leases ...*byohHostLease) error {
	return releaseBYOHHosts(f.ctx, f.coreClient.ConfigMaps(wmcoNamespace), leases)
}

func runCleanupFallback(ctx context.Context, operation func(context.Context) error) error {
	return operation(ctx)
}

func newBYOHCleanupContexts(total, reserved time.Duration) (context.Context, context.Context, context.CancelFunc) {
	if total < 0 {
		total = 0
	}
	if reserved < 0 {
		reserved = 0
	}
	if reserved > total {
		reserved = total
	}
	outerCtx, outerCancel := context.WithTimeout(context.Background(), total)
	mainCtx, mainCancel := context.WithTimeout(outerCtx, total-reserved)
	return mainCtx, outerCtx, func() {
		mainCancel()
		outerCancel()
	}
}

func (f *byohTestFixture) cleanupWithBudget(total, reserved time.Duration) error {
	cleanupCtx, fallbackCtx, cleanupCancel := newBYOHCleanupContexts(total, reserved)
	defer cleanupCancel()
	defer f.cancel()
	var cleanupErrors []error
	var settleAfterIDMS []*byohHostLease
	if err := f.deleteWorkload(cleanupCtx); err != nil {
		fallbackErr := runCleanupFallback(fallbackCtx, f.deleteWorkload)
		cleanupErrors = append(cleanupErrors, errors.Join(fmt.Errorf("delete BYOH workload: %w", err),
			wrapOptionalError("bounded workload-deletion fallback", fallbackErr)))
	}
	if err := f.restoreBYOHPrivateKeyWithContext(cleanupCtx); err != nil {
		fallbackErr := runCleanupFallback(fallbackCtx, f.restoreBYOHPrivateKeyWithContext)
		cleanupErrors = append(cleanupErrors, errors.Join(fmt.Errorf("restore cloud private key: %w", err),
			wrapOptionalError("bounded key-restoration fallback", fallbackErr)))
	}
	for _, lease := range f.leases {
		if lease.CleanupDone {
			continue
		}
		if lease.RegistrationOwned && !lease.ResetVerified {
			if err := f.deconfigureLeaseWithContext(cleanupCtx, lease); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("prove BYOH host reset: %w", err))
			}
		}
		if f.idmsName != "" && lease.LeaseID == f.idmsLeaseID {
			// OCP-82694 keeps the IDMS through every deconfiguration/path-removal attempt. Its
			// release or quarantine decision is always deferred until after IDMS cleanup.
			settleAfterIDMS = append(settleAfterIDMS, lease)
			continue
		}
		if lease.Disposable || lease.QuarantineRequired ||
			(lease.RegistrationAttempted && (!lease.RegistrationOwned || !lease.ResetVerified)) {
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine BYOH host: %w", err))
			}
		} else {
			// A physical host allocated but never registered was not mutated by WMCO.
			if !lease.RegistrationAttempted {
				lease.ResetVerified = true
			}
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return releaseBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("release BYOH host: %w", err))
			}
		}
	}
	// OCP-82694 intentionally retains the IDMS through deconfiguration and path removal checks.
	if err := f.deleteSyntheticIDMSWithContext(cleanupCtx); err != nil {
		fallbackErr := runCleanupFallback(fallbackCtx, f.deleteSyntheticIDMSWithContext)
		cleanupErrors = append(cleanupErrors, fmt.Errorf("delete synthetic IDMS: %w", err))
		if fallbackErr != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("bounded IDMS deletion fallback: %w", fallbackErr))
		}
		for _, lease := range settleAfterIDMS {
			if quarantineErr := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); quarantineErr != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine host after IDMS cleanup failure: %w", quarantineErr))
			}
		}
	} else {
		for _, lease := range settleAfterIDMS {
			if lease.Disposable || lease.QuarantineRequired || !lease.ResetVerified {
				if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
					return quarantineBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
				}); err != nil {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("quarantine host after IDMS cleanup: %w", err))
				}
				continue
			}
			if err := runCleanupFallback(fallbackCtx, func(ctx context.Context) error {
				return releaseBYOHHosts(ctx, f.coreClient.ConfigMaps(wmcoNamespace), []*byohHostLease{lease})
			}); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("release BYOH host after IDMS cleanup: %w", err))
			}
		}
	}
	return errors.Join(cleanupErrors...)
}

func (f *byohTestFixture) cleanup() error {
	return f.cleanupWithBudget(byohCleanupLimit, byohFallbackLimit)
}
