package controllers

import (
	"context"
	"testing"
	"time"

	mapi "github.com/openshift/api/machine/v1beta1"
	mclientfake "github.com/openshift/client-go/machine/clientset/versioned/fake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
)

// runningPhase and machineSetKind are convenience values used to build test fixtures below.
const (
	testRunningPhase   = "Running"
	testMachineSetKind = "MachineSet"
	testMachineSetName = "windows-machineset"
)

// newTestMachine returns a Windows Machine owned by testMachineSetName, with the given name, phase, and (optional)
// associated Node name. If deleting is true, a non-zero DeletionTimestamp is set on the Machine, simulating a
// delete already being in progress.
func newTestMachine(name, phase, nodeName string, deleting bool) *mapi.Machine {
	m := &mapi.Machine{
		TypeMeta: meta.TypeMeta{Kind: "Machine"},
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: cluster.MachineAPINamespace,
			Labels:    map[string]string{MachineOSLabel: "Windows"},
			OwnerReferences: []meta.OwnerReference{
				{Kind: testMachineSetKind, Name: testMachineSetName},
			},
		},
	}
	if phase != "" {
		m.Status.Phase = &phase
	}
	if nodeName != "" {
		m.Status.NodeRef = &core.ObjectReference{Name: nodeName}
	}
	if deleting {
		now := meta.Now()
		m.DeletionTimestamp = &now
		// A finalizer is required for the fake client to retain the object with a DeletionTimestamp instead of
		// removing it outright.
		m.Finalizers = []string{"windowsmachineconfig.openshift.io/test-finalizer"}
	}
	return m
}

// newTestMachineSet returns a MachineSet named testMachineSetName with the given replica count.
func newTestMachineSet(replicas int32) *mapi.MachineSet {
	return &mapi.MachineSet{
		TypeMeta: meta.TypeMeta{Kind: "MachineSet"},
		ObjectMeta: meta.ObjectMeta{
			Name:      testMachineSetName,
			Namespace: cluster.MachineAPINamespace,
		},
		Spec: mapi.MachineSetSpec{
			Replicas: &replicas,
		},
	}
}

// newTestNode returns a Node with the given name. If configured is true, the Node carries the VersionAnnotation
// that marks it as fully configured, a precondition for being considered healthy.
func newTestNode(name string, configured bool) *core.Node {
	n := &core.Node{
		ObjectMeta: meta.ObjectMeta{
			Name: name,
		},
	}
	if configured {
		n.Annotations = map[string]string{metadata.VersionAnnotation: "test-version"}
	}
	return n
}

// newDeletionTestReconciler returns a WindowsMachineReconciler backed by:
//   - a fake typed Machine clientset (seeded with machines and machineSet), used by isAllowedDeletion's
//     List/Get calls, matching how the real machineClient field is used in production.
//   - a fake controller-runtime client (seeded with machines and nodes), used for Node lookups
//     (isWindowsMachineHealthy) and for the Delete call in deleteMachine, matching how r.client is used in
//     production.
//
// It also returns the FakeRecorder so tests can assert on emitted events.
func newDeletionTestReconciler(t *testing.T, machines []*mapi.Machine, machineSet *mapi.MachineSet,
	nodes []*core.Node) (*WindowsMachineReconciler, *record.FakeRecorder) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, core.AddToScheme(scheme))
	require.NoError(t, mapi.AddToScheme(scheme))

	clientObjs := make([]client.Object, 0, len(machines)+len(nodes))
	machineClientObjs := make([]runtime.Object, 0, len(machines)+1)
	for _, m := range machines {
		clientObjs = append(clientObjs, m)
		machineClientObjs = append(machineClientObjs, m)
	}
	for _, n := range nodes {
		clientObjs = append(clientObjs, n)
	}
	if machineSet != nil {
		machineClientObjs = append(machineClientObjs, machineSet)
	}

	fc := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(clientObjs...).Build()
	mc := mclientfake.NewSimpleClientset(machineClientObjs...)
	recorder := record.NewFakeRecorder(20)

	r := &WindowsMachineReconciler{
		instanceReconciler: instanceReconciler{
			client:   fc,
			log:      logf.Log.WithName("test"),
			recorder: recorder,
		},
		machineClient: mc.MachineV1beta1(),
	}
	return r, recorder
}

// TestIsAllowedDeletion covers the deletion-gate matrix described in the WMCO private-key-rotation fix plan:
// it verifies that isAllowedDeletion consistently enforces maxUnhealthyCount regardless of which remediation
// path (authentication failure vs. stale private key) triggered the check.
func TestIsAllowedDeletion(t *testing.T) {
	ctx := context.Background()

	t.Run("2 replicas, 1 unhealthy, target still healthy -> restricted", func(t *testing.T) {
		// Reproduces the exact bug scenario: one Machine (siblingUnhealthy) never finished configuring and is
		// unhealthy; the target Machine is still healthy (its Node hasn't been touched yet) but is the one being
		// evaluated for deletion. maxUnhealthyCount(1) should block this second deletion.
		target := newTestMachine("target", testRunningPhase, "target-node", false)
		siblingUnhealthy := newTestMachine("sibling-unhealthy", "Provisioning", "", false)
		machineSet := newTestMachineSet(2)
		nodes := []*core.Node{newTestNode("target-node", true)}

		r, _ := newDeletionTestReconciler(t, []*mapi.Machine{target, siblingUnhealthy}, machineSet, nodes)

		allowed, err := r.isAllowedDeletion(ctx, target)
		require.NoError(t, err)
		assert.False(t, allowed, "deletion should be restricted when a sibling Machine is already unhealthy")
	})

	t.Run("2 replicas, both healthy -> allowed", func(t *testing.T) {
		target := newTestMachine("target", testRunningPhase, "target-node", false)
		siblingHealthy := newTestMachine("sibling-healthy", testRunningPhase, "sibling-node", false)
		machineSet := newTestMachineSet(2)
		nodes := []*core.Node{newTestNode("target-node", true), newTestNode("sibling-node", true)}

		r, _ := newDeletionTestReconciler(t, []*mapi.Machine{target, siblingHealthy}, machineSet, nodes)

		allowed, err := r.isAllowedDeletion(ctx, target)
		require.NoError(t, err)
		assert.True(t, allowed, "deletion should be allowed when unhealthy count is 0")
	})

	t.Run("1 replica MachineSet -> always allowed", func(t *testing.T) {
		// Special case: when maxUnhealthyCount == totalWindowsMachineCount (a MachineSet of size 1), deletion must
		// always be allowed, since there can never be a "healthy" sibling to wait for.
		target := newTestMachine("target", "Provisioning", "", false) // deliberately unhealthy
		machineSet := newTestMachineSet(1)

		r, _ := newDeletionTestReconciler(t, []*mapi.Machine{target}, machineSet, nil)

		allowed, err := r.isAllowedDeletion(ctx, target)
		require.NoError(t, err)
		assert.True(t, allowed, "deletion must always be allowed for a MachineSet of size 1")
	})

	t.Run("3 replicas, 1 already unhealthy, target healthy -> restricted at boundary", func(t *testing.T) {
		target := newTestMachine("target", testRunningPhase, "target-node", false)
		siblingHealthy := newTestMachine("sibling-healthy", testRunningPhase, "sibling-node", false)
		siblingUnhealthy := newTestMachine("sibling-unhealthy", "Provisioning", "", false)
		machineSet := newTestMachineSet(3)
		nodes := []*core.Node{newTestNode("target-node", true), newTestNode("sibling-node", true)}

		r, _ := newDeletionTestReconciler(t,
			[]*mapi.Machine{target, siblingHealthy, siblingUnhealthy}, machineSet, nodes)

		allowed, err := r.isAllowedDeletion(ctx, target)
		require.NoError(t, err)
		assert.False(t, allowed, "deletion should be restricted: unhealthy count (1) already meets maxUnhealthyCount")
	})

	t.Run("sibling already being deleted is excluded from healthy count -> restricted", func(t *testing.T) {
		// A Machine with a non-zero DeletionTimestamp must not count as healthy, even if its Node still looks
		// fully configured (deletion may not have propagated to the Node yet).
		target := newTestMachine("target", testRunningPhase, "target-node", false)
		siblingDeleting := newTestMachine("sibling-deleting", testRunningPhase, "sibling-node", true)
		machineSet := newTestMachineSet(2)
		nodes := []*core.Node{newTestNode("target-node", true), newTestNode("sibling-node", true)}

		r, _ := newDeletionTestReconciler(t, []*mapi.Machine{target, siblingDeleting}, machineSet, nodes)

		allowed, err := r.isAllowedDeletion(ctx, target)
		require.NoError(t, err)
		assert.False(t, allowed,
			"deletion should be restricted: a Machine already being deleted must not count as healthy")
	})
}

// TestDeleteMachine_AlreadyDeleting verifies that deleteMachine is a no-op (does not error and does not attempt a
// second delete) when the Machine already has a DeletionTimestamp set, i.e. its deletion was already initiated.
func TestDeleteMachine_AlreadyDeleting(t *testing.T) {
	ctx := context.Background()
	m := newTestMachine("already-deleting", testRunningPhase, "some-node", true)
	r, recorder := newDeletionTestReconciler(t, []*mapi.Machine{m}, newTestMachineSet(2), nil)

	err := r.deleteMachine(ctx, m)
	require.NoError(t, err)

	select {
	case e := <-recorder.Events:
		t.Fatalf("expected no event to be emitted for a Machine already being deleted, got: %s", e)
	default:
		// expected: no event emitted, since deleteMachine returns early
	}
}

// TestDeleteMachineIfAllowed verifies the shared gate helper used by both the authentication-failure and
// stale-private-key remediation paths: it must delete the Machine when allowed, and must restrict + requeue with
// backoff (emitting a MachineDeletionRestricted event) when not allowed - regardless of the "reason" passed in,
// confirming both former code paths now behave consistently.
func TestDeleteMachineIfAllowed(t *testing.T) {
	ctx := context.Background()

	for _, reason := range []string{"authentication failure", "private key out of date"} {
		t.Run(reason+"/allowed", func(t *testing.T) {
			target := newTestMachine("target", testRunningPhase, "target-node", false)
			siblingHealthy := newTestMachine("sibling-healthy", testRunningPhase, "sibling-node", false)
			machineSet := newTestMachineSet(2)
			nodes := []*core.Node{newTestNode("target-node", true), newTestNode("sibling-node", true)}
			r, recorder := newDeletionTestReconciler(t, []*mapi.Machine{target, siblingHealthy}, machineSet, nodes)

			result, err := r.deleteMachineIfAllowed(ctx, target, reason)
			require.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, result, "no requeue expected when deletion proceeds")

			// The Machine should have been deleted from the fake client.
			err = r.client.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name},
				&mapi.Machine{})
			assert.True(t, apierrors.IsNotFound(err), "expected Machine to have been deleted")

			assertNoEventWithReason(t, recorder, "MachineDeletionRestricted")
		})

		t.Run(reason+"/restricted", func(t *testing.T) {
			target := newTestMachine("target", testRunningPhase, "target-node", false)
			siblingUnhealthy := newTestMachine("sibling-unhealthy", "Provisioning", "", false)
			machineSet := newTestMachineSet(2)
			nodes := []*core.Node{newTestNode("target-node", true)}
			r, recorder := newDeletionTestReconciler(t, []*mapi.Machine{target, siblingUnhealthy}, machineSet, nodes)

			result, err := r.deleteMachineIfAllowed(ctx, target, reason)
			require.NoError(t, err)
			assert.Zero(t, result.Requeue, "should use RequeueAfter backoff, not an immediate requeue")
			assert.Equal(t, machineDeletionRestrictedRequeueInterval, result.RequeueAfter)

			// The Machine should NOT have been deleted.
			err = r.client.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Name},
				&mapi.Machine{})
			require.NoError(t, err, "expected Machine to still exist")

			assertEventWithReason(t, recorder, "MachineDeletionRestricted")
		})
	}
}

// TestIsWindowsMachineHealthy_RunningWithoutNodeRef is a regression test ensuring a Machine reporting phase
// "Running" with a nil NodeRef is treated as unhealthy instead of panicking on a nil pointer dereference.
func TestIsWindowsMachineHealthy_RunningWithoutNodeRef(t *testing.T) {
	ctx := context.Background()
	m := newTestMachine("running-no-noderef", testRunningPhase, "", false)
	r, _ := newDeletionTestReconciler(t, []*mapi.Machine{m}, newTestMachineSet(2), nil)

	assert.NotPanics(t, func() {
		healthy := r.isWindowsMachineHealthy(ctx, m)
		assert.False(t, healthy)
	})
}

// assertEventWithReason drains the given FakeRecorder's channel (non-blocking with a short timeout) and asserts
// that at least one event with the given reason was recorded.
func assertEventWithReason(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	select {
	case e := <-recorder.Events:
		assert.Contains(t, e, reason)
	case <-time.After(time.Second):
		t.Fatalf("expected an event with reason %q, but none was recorded", reason)
	}
}

// assertNoEventWithReason asserts that no event with the given reason was recorded.
func assertNoEventWithReason(t *testing.T, recorder *record.FakeRecorder, reason string) {
	t.Helper()
	select {
	case e := <-recorder.Events:
		assert.NotContains(t, e, reason)
	default:
		// expected: no event recorded
	}
}
