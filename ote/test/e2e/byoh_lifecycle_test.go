package winc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestRegisterBYOHInstancesPreservesUnrelatedEntries(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap, Namespace: wmcoNamespace,
		UID: "instances-uid", ResourceVersion: "1"},
		Data: map[string]string{"unrelated.example.test": "username=existing-user"}}
	client := fake.NewSimpleClientset(cm)
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
	state := &byohInstancesState{}
	if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}, state); err != nil {
		t.Fatalf("registerBYOHInstances() returned an error: %v", err)
	}
	updated, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
		metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Data["unrelated.example.test"] != "username=existing-user" ||
		updated.Data[lease.Address] != "username=test-user" {
		t.Fatalf("windows-instances data was not preserved: %#v", updated.Data)
	}
}

func readyBYOHNode(name string, uid types.UID, ip string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid, ResourceVersion: "1",
		Labels:      map[string]string{"kubernetes.io/os": "windows", "windowsmachineconfig.openshift.io/byoh": "true"},
		Annotations: map[string]string{byohVersionAnno: "v1", byohDesiredVersionAnno: "v1"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
}

func fakeClientAssigningConfigMapUID(t *testing.T) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		created.UID = "created-configmap-uid"
		created.ResourceVersion = "1"
		if err := client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			created, created.Namespace); err != nil {
			return true, nil, err
		}
		return true, created, nil
	})
	return client
}

func TestRegistrationAmbiguityAndConflictOwnership(t *testing.T) {
	tests := []struct {
		name       string
		existing   *corev1.ConfigMap
		verb       string
		applyWrite bool
		wantOwned  bool
	}{
		{name: "create applied before response error", verb: "create", applyWrite: true, wantOwned: true},
		{name: "update applied before response error", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: byohInstancesConfigMap, Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1"},
			Data: map[string]string{"other": "username=other"}},
			verb: "update", applyWrite: true, wantOwned: true},
		{name: "pre-existing address conflict", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: byohInstancesConfigMap, Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1"},
			Data: map[string]string{"192.0.2.10": "username=other"}},
			wantOwned: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []runtime.Object{}
			if test.existing != nil {
				objects = append(objects, test.existing.DeepCopy())
			}
			client := fake.NewSimpleClientset(objects...)
			if test.applyWrite {
				client.PrependReactor(test.verb, "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
					resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
					var err error
					if test.verb == "create" {
						created := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
						created.UID = "created-uid"
						err = client.Tracker().Create(resource, created, wmcoNamespace)
					} else {
						updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
						err = client.Tracker().Update(resource, updated, wmcoNamespace)
					}
					if err != nil {
						t.Fatal(err)
					}
					return true, nil, errors.New("simulated response loss")
				})
			}
			lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
			state := &byohInstancesState{}
			err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
				[]*byohHostLease{lease}, state)
			if err == nil {
				t.Fatal("expected registration failure")
			}
			if lease.RegistrationOwned != test.wantOwned {
				t.Fatalf("RegistrationOwned=%t, want %t", lease.RegistrationOwned, test.wantOwned)
			}
		})
	}
}

func TestExactNodeDiscoveryAndDistinctIdentity(t *testing.T) {
	lease := &byohHostLease{Address: "host.example.test", AddressType: byohAddressDNS}
	lookup := func(context.Context, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("192.0.2.11")}, nil
	}
	nodeA := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	nodeB := readyBYOHNode("node-b", "uid-b", "192.0.2.11")
	if _, _, err := exactLeaseNode(context.Background(), []corev1.Node{*nodeA, *nodeB}, lease, lookup, ""); err == nil {
		t.Fatal("multi-A DNS matching multiple Nodes was accepted")
	}
	node, ready, err := exactLeaseNode(context.Background(), []corev1.Node{*nodeA}, lease, lookup, "")
	if err != nil || !ready || node.UID != nodeA.UID {
		t.Fatalf("exact DNS Node was not discovered: node=%#v ready=%t err=%v", node, ready, err)
	}
	if err := distinctLeaseNodes([]*byohHostLease{{NodeName: "same", NodeUID: "uid-a"},
		{NodeName: "same", NodeUID: "uid-a"}}); err == nil {
		t.Fatal("same Node identity was accepted for two physical hosts")
	}
	if err := distinctLeaseNodes([]*byohHostLease{{NodeName: "node-a", NodeUID: "uid-a"},
		{NodeName: "node-b", NodeUID: "uid-b"}}); err != nil {
		t.Fatalf("distinct IP/DNS Nodes were rejected: %v", err)
	}
}

func TestDNSResolutionIsContextBoundCachedAndIPv4Only(t *testing.T) {
	lease := &byohHostLease{Address: "host.example.test", AddressType: byohAddressDNS}
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	nonMatch := readyBYOHNode("node-v6", "uid-v6", "2001:db8::10")
	lookups := 0
	lookup := func(context.Context, string) ([]net.IP, error) {
		lookups++
		return []net.IP{net.ParseIP("2001:db8::10"), net.ParseIP("192.0.2.10")}, nil
	}
	selected, ready, err := exactLeaseNode(context.Background(), []corev1.Node{*node, *nonMatch}, lease, lookup, "")
	if err != nil || !ready || selected.UID != node.UID || lookups != 1 {
		t.Fatalf("mixed A/AAAA discovery was not IPv4-only and cached: node=%#v ready=%t lookups=%d err=%v",
			selected, ready, lookups, err)
	}

	t.Run("resolver error alone is retryable", func(t *testing.T) {
		attempts := 0
		retryingLookup := func(context.Context, string) ([]net.IP, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("temporary resolver failure")
			}
			return []net.IP{net.ParseIP("192.0.2.10")}, nil
		}
		client := fake.NewSimpleClientset(node)
		if err := waitForLeaseNodeWithOptions(context.Background(), client.CoreV1().Nodes(), lease,
			retryingLookup, "", time.Millisecond, 100*time.Millisecond); err != nil || attempts != 2 {
			t.Fatalf("resolver-only retry did not converge exactly once: attempts=%d err=%v", attempts, err)
		}
	})

	t.Run("Node API errors are fatal", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		lists := 0
		client.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			lists++
			return true, nil, errors.New("Node API unavailable")
		})
		err := waitForLeaseNodeWithOptions(context.Background(), client.CoreV1().Nodes(), lease,
			lookup, "", time.Millisecond, 100*time.Millisecond)
		if err == nil || lists != 1 {
			t.Fatalf("fatal Node API error was retried or ignored: lists=%d err=%v", lists, err)
		}
	})

	t.Run("resolver observes cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		blockingLookup := func(ctx context.Context, _ string) ([]net.IP, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if _, _, err := exactLeaseNode(ctx, []corev1.Node{*node}, lease, blockingLookup, ""); err == nil {
			t.Fatal("canceled DNS resolution was accepted")
		}
	})
}

func TestNodeUIDReplacementSequences(t *testing.T) {
	old := readyBYOHNode("node-a", "old-uid", "192.0.2.10")
	t.Run("stale old UID times out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		time.AfterFunc(20*time.Millisecond, cancel)
		err := waitForNodeUIDGone(ctx, fake.NewSimpleClientset(old.DeepCopy()).CoreV1().Nodes(),
			old.Name, old.UID, time.Millisecond, 15*time.Millisecond)
		if err == nil {
			t.Fatal("stale old Node UID was treated as gone")
		}
	})
	t.Run("same name new UID", func(t *testing.T) {
		newNode := old.DeepCopy()
		newNode.UID = "new-uid"
		if err := waitForNodeUIDGone(context.Background(), fake.NewSimpleClientset(newNode).CoreV1().Nodes(),
			old.Name, old.UID, time.Millisecond, 20*time.Millisecond); err != nil {
			t.Fatalf("new UID with retained name was rejected: %v", err)
		}
	})
	t.Run("API error", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("node API unavailable")
		})
		if err := waitForNodeUIDGone(context.Background(), client.CoreV1().Nodes(), old.Name, old.UID,
			time.Millisecond, 20*time.Millisecond); err == nil {
			t.Fatal("Node API error was ignored")
		}
	})
}

func TestStableTypedNodeConvergenceRequiresCleanConsecutiveObservations(t *testing.T) {
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	client := fake.NewSimpleClientset(node)
	gets := 0
	client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		current := node.DeepCopy()
		switch gets {
		case 1:
			current.Spec.Unschedulable = true
		case 2:
			current.Labels["windowsmachineconfig.openshift.io/upgrading"] = "true"
		case 3:
			current.Annotations["windowsmachineconfig.openshift.io/reboot-required"] = ""
		}
		return true, current, nil
	})
	if err := waitForStableNodeConvergence(context.Background(), client.CoreV1().Nodes(), lease,
		time.Millisecond, 100*time.Millisecond); err != nil {
		t.Fatalf("stable typed convergence did not recover after transient markers: %v", err)
	}
	if gets != 3+reconfigurationStableChecks {
		t.Fatalf("convergence did not require %d consecutive clean observations: gets=%d",
			reconfigurationStableChecks, gets)
	}

	replacement := node.DeepCopy()
	replacement.UID = "replacement-uid"
	if err := waitForStableNodeConvergence(context.Background(), fake.NewSimpleClientset(replacement).CoreV1().Nodes(),
		lease, time.Millisecond, 20*time.Millisecond); err == nil {
		t.Fatal("stable convergence accepted a replacement Node UID")
	}
}

func TestInvalidateVersionUsesRetainedUIDFreshRVConflictRefreshAndStableObservations(t *testing.T) {
	node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
	node.ResourceVersion = "1"
	client := fake.NewSimpleClientset(node)
	resource := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	updates := 0
	seenRVs := []string{}
	client.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if updates == 1 {
			refreshed := node.DeepCopy()
			refreshed.ResourceVersion = "2"
			return true, refreshed, nil
		}
		return false, nil, nil
	})
	client.PrependReactor("update", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Node).DeepCopy()
		updates++
		seenRVs = append(seenRVs, updated.ResourceVersion)
		if updated.UID != node.UID || updated.Annotations[byohVersionAnno] != "invalidVersion" {
			t.Fatalf("Update did not retain UID and exact invalidVersion mutation: %#v", updated)
		}
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, node.Name,
				errors.New("conflict"))
		}
		response := updated.DeepCopy()
		response.ResourceVersion = "3"
		converged := node.DeepCopy()
		converged.ResourceVersion = "4"
		if err := client.Tracker().Update(resource, converged, ""); err != nil {
			t.Fatal(err)
		}
		return true, response, nil
	})
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	fixture := &byohTestFixture{coreClient: client.CoreV1(), ctx: context.Background(),
		versionConvergenceForTest: func(ctx context.Context, nodes corev1client.NodeInterface,
			lease *byohHostLease) error {
			return waitForStableNodeConvergence(ctx, nodes, lease, 0, time.Second)
		}}
	if err := fixture.invalidateVersionAndWait(lease); err != nil {
		t.Fatalf("typed invalidVersion production method failed: %v", err)
	}
	if !reflect.DeepEqual(seenRVs, []string{"1", "2"}) || lease.NodeResourceVersion != "4" {
		t.Fatalf("conflict retry did not refresh resourceVersion and converge: updates=%v retained=%q",
			seenRVs, lease.NodeResourceVersion)
	}

	replacement := node.DeepCopy()
	replacement.UID = "replacement-uid"
	replacement.ResourceVersion = "5"
	replacementClient := fake.NewSimpleClientset(replacement)
	updatedReplacement := false
	replacementClient.PrependReactor("update", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		updatedReplacement = true
		return false, nil, nil
	})
	fixture = &byohTestFixture{coreClient: replacementClient.CoreV1(), ctx: context.Background()}
	if err := fixture.invalidateVersionAndWait(lease); err == nil || updatedReplacement {
		t.Fatalf("replacement Node was mutated: updated=%t err=%v", updatedReplacement, err)
	}
}

func TestWMCOLogMatcherRejectsOldEvidence(t *testing.T) {
	lease := &byohHostLease{Address: "192.0.2.10", NodeName: "node-a"}
	since := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	oldLine := "2026-10-04T11:59:59Z deconfiguring node-a"
	newLine := "2026-10-04T12:00:01Z deconfiguring node-a"
	if wmcoLogMatchesLeaseSince(oldLine, lease, "deconfiguring", since) {
		t.Fatal("pre-mutation log evidence was accepted")
	}
	if !wmcoLogMatchesLeaseSince(oldLine+"\n"+newLine, lease, "deconfiguring", since) {
		t.Fatal("post-mutation log evidence was rejected")
	}
}

func TestOwnedIDMSAmbiguousCreateAndPreexistingName(t *testing.T) {
	scheme := runtime.NewScheme()
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme)
	resource := dynamicClient.Resource(byohIDMSGVR)
	dynamicClient.PrependReactor("create", "imagedigestmirrorsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		created := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		created.SetUID("idms-uid")
		created.SetResourceVersion("1")
		if err := dynamicClient.Tracker().Create(byohIDMSGVR, created, ""); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("simulated response loss")
	})
	ownership, err := createOwnedIDMS(context.Background(), resource, "winc-82694-test", "lease-a")
	if err == nil || !ownership.owned {
		t.Fatalf("ambiguous IDMS creation ownership was lost: ownership=%#v err=%v", ownership, err)
	}
	preexisting := &unstructured.Unstructured{}
	preexisting.SetAPIVersion("config.openshift.io/v1")
	preexisting.SetKind("ImageDigestMirrorSet")
	preexisting.SetName("preexisting")
	preexistingClient := dynamicfake.NewSimpleDynamicClient(scheme, preexisting)
	ownership, err = createOwnedIDMS(context.Background(), preexistingClient.Resource(byohIDMSGVR),
		"preexisting", "lease-a")
	if err == nil || ownership.owned || ownership.attempted {
		t.Fatalf("pre-existing IDMS name was treated as owned: ownership=%#v err=%v", ownership, err)
	}
}

func TestRegistrationConfigMapRestoration(t *testing.T) {
	t.Run("absent created absent", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
			metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("test-created empty ConfigMap was not removed: %v", err)
		}
	})
	t.Run("preserve mode keeps a test-created empty object", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		lease := &byohHostLease{Address: "host.example.test", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		ownedUID := state.ownedUID
		deletes := 0
		client.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			deletes++
			return false, nil, nil
		})
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil || deletes != 0 || current.UID != ownedUID || len(current.Data) != 0 {
			t.Fatalf("preserve mode restored absence instead of retaining the empty object: deletes=%d current=%#v err=%v",
				deletes, current, err)
		}
	})
	t.Run("concurrent unrelated key preserved", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		cm, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
			metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read owned windows-instances before concurrent update: %v", err)
		}
		cm.Data["unrelated"] = "username=other"
		cm.Annotations["example.test/consumer"] = "preserve"
		if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Update(context.Background(), cm,
			metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		cm, err = client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
			metav1.GetOptions{})
		if err != nil || cm.Data["unrelated"] != "username=other" ||
			cm.Annotations["example.test/consumer"] != "preserve" ||
			cm.Annotations[registrationOwnershipAnnotation(lease.Address)] != "" {
			t.Fatalf("concurrent unrelated data was not preserved: cm=%#v err=%v", cm, err)
		}
	})
	t.Run("preserved entry withdrawal reconciles applied response loss without delete", func(t *testing.T) {
		instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
			Labels: map[string]string{"example.test/label": "preserve"}},
			Data: map[string]string{"unrelated": "username=other"}}
		client := fake.NewSimpleClientset(instances)
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated unregister response loss")
		})
		deletes := 0
		client.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			deletes++
			return false, nil, nil
		})
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil || deletes != 0 || current.UID != instances.UID ||
			current.Data["unrelated"] != "username=other" || current.Data[lease.Address] != "" ||
			current.Labels["example.test/label"] != "preserve" {
			t.Fatalf("applied entry withdrawal was not reconciled safely: deletes=%d current=%#v err=%v",
				deletes, current, err)
		}
	})
}

func TestUnregisterRequiresOwnershipAndRejectsAvailableConflict(t *testing.T) {
	instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
		Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1"},
		Data: map[string]string{"192.0.2.10": "username=test-user"}}
	client := fake.NewSimpleClientset(instances)
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
	if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}, &byohInstancesState{observed: true, initiallyExisted: true}, false, nil, nil); err == nil {
		t.Fatal("unregister accepted an entry that was not provably owned")
	}
	lease.HostID, lease.LeaseID, lease.ClaimedAliases = "host-a", "lease-a", []string{"192.0.2.10"}
	conflicts := map[string]map[string]string{
		"available":   {"192.0.2.10": testPoolEntry("ip", "host-a", "available", "")},
		"wrong owner": {"192.0.2.10": testPoolEntry("ip", "host-a", "releasing", "other-lease")},
	}
	for name, pool := range conflicts {
		if _, err := transitionBYOHHosts(pool, []*byohHostLease{lease}, "releasing", false, time.Now()); err == nil {
			t.Fatalf("%s ownership conflict was accepted as an owned transition", name)
		}
	}
}

func TestCleanupOrdersIDMSDeletionBeforeReleaseAndAggregatesFailures(t *testing.T) {
	lease := &byohHostLease{HostID: "host-a", LeaseID: "lease-a", Username: "test-user",
		SSHAddress: "ssh.example.test", Platform: "test", ResetPolicy: byohResetPolicy,
		AllocatedAt: "2026-10-04T00:00:00Z", ClaimedAliases: []string{"192.0.2.10"}, ResetVerified: true}
	pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-a")}}
	coreClient := fake.NewSimpleClientset(pool)
	updates := 0
	order := []string{}
	coreClient.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		order = append(order, "pool-update")
		return true, nil, errors.New("pool mutation failure")
	})
	scheme := runtime.NewScheme()
	idms := &unstructured.Unstructured{}
	idms.SetAPIVersion("config.openshift.io/v1")
	idms.SetKind("ImageDigestMirrorSet")
	idms.SetName("winc-82694-lease-a")
	idms.SetUID("idms-uid")
	idms.SetResourceVersion("1")
	idms.SetAnnotations(map[string]string{"windowsmachineconfig.openshift.io/test-id": "lease-a"})
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, idms)
	deletedIDMS := false
	dynamicClient.PrependReactor("delete", "imagedigestmirrorsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		deletedIDMS = true
		order = append(order, "idms-delete")
		return false, nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &byohTestFixture{coreClient: coreClient.CoreV1(), dynamicClient: dynamicClient, ctx: ctx, cancel: cancel,
		leases: []*byohHostLease{lease}, idmsName: idms.GetName(), idmsLeaseID: "lease-a", idmsAttempted: true,
		idmsOwned: true, idmsUID: idms.GetUID()}
	err := fixture.cleanup()
	if err == nil || !deletedIDMS || updates == 0 {
		t.Fatalf("cleanup did not attempt ordered IDMS deletion and release: deleted=%t updates=%d err=%v",
			deletedIDMS, updates, err)
	}
	if len(order) < 2 || order[0] != "idms-delete" || order[1] != "pool-update" {
		t.Fatalf("IDMS cleanup did not precede pool release/quarantine: %v", order)
	}
	if !strings.Contains(err.Error(), "release BYOH host after IDMS cleanup") ||
		!strings.Contains(err.Error(), "quarantine after release failure") {
		t.Fatalf("cleanup did not aggregate release/quarantine failures: %v", err)
	}
}

func TestCleanupFailedDeconfigurationDeletesIDMSBeforeQuarantineWithinAggregateBudget(t *testing.T) {
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", SSHAddress: "ssh.example.test", HostID: "host-a",
		LeaseID: "lease-a", Platform: "test", ResetPolicy: byohResetPolicy, AllocatedAt: "2026-10-04T00:00:00Z",
		ClaimedAliases: []string{"192.0.2.10"}, NodeName: "node-a", NodeUID: "node-uid",
		RegistrationAttempted: true, RegistrationOwned: true, Registered: true, PreserveInstancesConfigMap: true}
	ownershipKey := registrationOwnershipAnnotation(lease.Address)
	instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
		Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
		Annotations: map[string]string{ownershipKey: lease.LeaseID}},
		Data: map[string]string{lease.Address: "username=" + lease.Username}}
	pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: map[string]string{lease.Address: testPoolEntry("ip", lease.HostID, "allocated", lease.LeaseID)}}
	coreClient := fake.NewSimpleClientset(instances, pool)
	order := []string{}
	configMapDeletes := 0
	coreClient.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		configMapDeletes++
		return false, nil, nil
	})
	coreClient.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		switch updated.Name {
		case byohInstancesConfigMap:
			order = append(order, "withdraw-entry")
			return false, nil, nil
		case byohPoolConfigMap:
			order = append(order, "quarantine")
			return true, nil, errors.New("quarantine failure")
		default:
			return false, nil, nil
		}
	})
	scheme := runtime.NewScheme()
	idms := &unstructured.Unstructured{}
	idms.SetAPIVersion("config.openshift.io/v1")
	idms.SetKind("ImageDigestMirrorSet")
	idms.SetName("winc-82694-lease-a")
	idms.SetUID("idms-uid")
	idms.SetResourceVersion("1")
	idms.SetAnnotations(map[string]string{"windowsmachineconfig.openshift.io/test-id": lease.LeaseID})
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, idms)
	dynamicClient.PrependReactor("delete", "imagedigestmirrorsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		order = append(order, "idms-delete")
		return false, nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &byohTestFixture{coreClient: coreClient.CoreV1(), dynamicClient: dynamicClient, ctx: ctx, cancel: cancel,
		leases: []*byohHostLease{lease}, instancesState: byohInstancesState{observed: true, initiallyExisted: true,
			initialUID: instances.UID, ownedUID: instances.UID, originalOwnership: map[string]*string{ownershipKey: nil}},
		idmsName: idms.GetName(), idmsLeaseID: lease.LeaseID, idmsAttempted: true, idmsOwned: true,
		idmsUID: idms.GetUID(), waitForLeaseLog: func(ctx context.Context, _ *byohHostLease, _ string, _ time.Time) error {
			<-ctx.Done()
			return ctx.Err()
		}}
	started := time.Now()
	err := fixture.cleanupWithBudget(80*time.Millisecond, 25*time.Millisecond)
	elapsed := time.Since(started)
	if !reflect.DeepEqual(order, []string{"withdraw-entry", "idms-delete", "quarantine"}) {
		t.Fatalf("actual cleanup orchestration violated deconfigure-IDMS-quarantine order: %v", order)
	}
	if configMapDeletes != 0 {
		t.Fatalf("OCP-82694 entry withdrawal deleted windows-instances %d times", configMapDeletes)
	}
	if err == nil || !strings.Contains(err.Error(), "prove BYOH host reset") ||
		!strings.Contains(err.Error(), "quarantine host after IDMS cleanup") {
		t.Fatalf("actual cleanup did not join deconfiguration and quarantine errors: %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("actual cleanup exceeded its aggregate outer deadline: %v", elapsed)
	}
}

type orderedResetSession struct {
	expectedCommand string
	output          string
	err             error
	marker          string
	order           *[]string
	beforeRun       func() error
}

func (s *orderedResetSession) CombinedOutput(command string) ([]byte, error) {
	if command != s.expectedCommand {
		return nil, errors.New("unexpected reset protocol command")
	}
	if s.beforeRun != nil {
		if err := s.beforeRun(); err != nil {
			return nil, err
		}
	}
	*s.order = append(*s.order, s.marker)
	return []byte(s.output), s.err
}

func (s *orderedResetSession) Close() error { return nil }

func TestActualDeconfigurationResetProofOrdersIDMSBeforeSettlement(t *testing.T) {
	tests := []struct {
		name         string
		serviceErr   error
		directoryErr error
		wantSuccess  bool
	}{
		{name: "success", wantSuccess: true},
		{name: "service failure", serviceErr: errors.New("service probe failed")},
		{name: "directory failure", directoryErr: errors.New("directory probe failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", SSHAddress: "ssh.example.test",
				HostID: "host-a", LeaseID: "lease-a", Platform: "test", ResetPolicy: byohResetPolicy,
				AllocatedAt: "2026-10-04T00:00:00Z", ClaimedAliases: []string{"192.0.2.10"},
				NodeName: "node-a", NodeUID: "node-uid", RegistrationAttempted: true,
				RegistrationOwned: true, Registered: true, PreserveInstancesConfigMap: true}
			ownershipKey := registrationOwnershipAnnotation(lease.Address)
			instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
				Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
				Annotations: map[string]string{ownershipKey: lease.LeaseID}},
				Data: map[string]string{lease.Address: "username=" + lease.Username}}
			pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap,
				Namespace: wmcoNamespace}, Data: map[string]string{
				lease.Address: testPoolEntry("ip", lease.HostID, "allocated", lease.LeaseID)}}
			node := readyBYOHNode(lease.NodeName, lease.NodeUID, lease.Address)
			coreClient := fake.NewSimpleClientset(instances, pool, node)
			order := []string{}
			coreClient.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
				if updated.Name == byohInstancesConfigMap {
					order = append(order, "withdraw-registration")
				} else if updated.Name == byohPoolConfigMap {
					entry, err := parseBYOHPoolEntry(lease.Address, updated.Data[lease.Address])
					if err != nil {
						t.Fatal(err)
					}
					order = append(order, "pool-"+entry.Status)
				}
				return false, nil, nil
			})
			nodeResource := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
			coreClient.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
				if err := coreClient.Tracker().Delete(nodeResource, "", lease.NodeName); err != nil && !apierrors.IsNotFound(err) {
					t.Fatalf("remove retained Node during deconfiguration sequence: %v", err)
				}
				order = append(order, "retained-node-gone")
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, lease.NodeName)
			})
			idms := &unstructured.Unstructured{}
			idms.SetAPIVersion("config.openshift.io/v1")
			idms.SetKind("ImageDigestMirrorSet")
			idms.SetName("winc-82694-lease-a")
			idms.SetUID("idms-uid")
			idms.SetResourceVersion("1")
			idms.SetAnnotations(map[string]string{"windowsmachineconfig.openshift.io/test-id": lease.LeaseID})
			dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), idms)
			dynamicClient.PrependReactor("delete", "imagedigestmirrorsets",
				func(action clienttesting.Action) (bool, runtime.Object, error) {
					options := action.(clienttesting.DeleteAction).GetDeleteOptions()
					if options.Preconditions == nil || options.Preconditions.UID == nil ||
						*options.Preconditions.UID != idms.GetUID() || options.Preconditions.ResourceVersion == nil ||
						*options.Preconditions.ResourceVersion != idms.GetResourceVersion() {
						t.Fatalf("IDMS deletion omitted retained UID/latest-RV preconditions: %#v", options.Preconditions)
					}
					order = append(order, "idms-delete")
					return false, nil, nil
				})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			fixture := &byohTestFixture{coreClient: coreClient.CoreV1(), dynamicClient: dynamicClient,
				ctx: ctx, cancel: cancel, leases: []*byohHostLease{lease}, instancesState: byohInstancesState{
					observed: true, initiallyExisted: true, initialUID: instances.UID, ownedUID: instances.UID,
					originalOwnership: map[string]*string{ownershipKey: nil}}, idmsName: idms.GetName(),
				idmsLeaseID: lease.LeaseID, idmsAttempted: true, idmsOwned: true, idmsUID: idms.GetUID()}
			fixture.waitForLeaseLog = func(_ context.Context, _ *byohHostLease, message string, _ time.Time) error {
				order = append(order, "log-"+message)
				return nil
			}
			sessionIndex := 0
			fixture.sshClientForTest = func(ctx context.Context, _ *byohHostLease) (*windowsSSHClient, error) {
				if _, err := dynamicClient.Resource(byohIDMSGVR).Get(ctx, idms.GetName(), metav1.GetOptions{}); err != nil {
					t.Fatalf("IDMS was removed before the actual reset protocol: %v", err)
				}
				sessions := []*orderedResetSession{
					{expectedCommand: "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " +
						encodePowerShell(managedServicesStoppedScript()), output: managedServicesStoppedProof,
						err: test.serviceErr, marker: "actual-services-stopped", order: &order},
					{expectedCommand: "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " +
						encodePowerShell(managedDirectoriesRemovedScript()), output: managedDirectoriesAbsentProof,
						err: test.directoryErr, marker: "actual-directories-and-containerd-absent", order: &order,
						beforeRun: func() error {
							if !slices.Contains(byohManagedDirectories, `C:\k\containerd`) {
								return errors.New("containerd path missing from actual reset protocol")
							}
							return nil
						}},
				}
				return &windowsSSHClient{newSession: func() (sshCommandSession, error) {
					if sessionIndex >= len(sessions) {
						return nil, errors.New("unexpected extra reset session")
					}
					session := sessions[sessionIndex]
					sessionIndex++
					return session, nil
				}, closeFn: func() error { return nil }}, nil
			}
			err := fixture.cleanupWithBudget(time.Second, 200*time.Millisecond)
			commonPrefix := []string{"withdraw-registration", "log-deconfiguring", "log-removing directories",
				"log-instance has been deconfigured", "retained-node-gone", "actual-services-stopped"}
			if len(order) < len(commonPrefix) || !reflect.DeepEqual(order[:len(commonPrefix)], commonPrefix) {
				t.Fatalf("actual deconfiguration proof order mismatch: %v", order)
			}
			if test.wantSuccess {
				want := append(append([]string(nil), commonPrefix...), "actual-directories-and-containerd-absent",
					"idms-delete", "pool-releasing", "pool-available")
				if err != nil || !lease.ResetVerified || !reflect.DeepEqual(order, want) {
					t.Fatalf("successful actual reset protocol did not release after IDMS deletion: reset=%t order=%v err=%v",
						lease.ResetVerified, order, err)
				}
			} else if err == nil || lease.ResetVerified || !lease.QuarantineRequired ||
				!slices.Contains(order, "idms-delete") || !slices.Contains(order, "pool-unavailable") ||
				slices.Contains(order, "pool-available") {
				t.Fatalf("failed actual reset protocol was not quarantined after IDMS deletion: reset=%t quarantine=%t order=%v err=%v",
					lease.ResetVerified, lease.QuarantineRequired, order, err)
			}
		})
	}
}

func TestCleanupFallbackUsesFiniteContext(t *testing.T) {
	var hadDeadline bool
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runCleanupFallback(ctx, func(ctx context.Context) error {
		_, hadDeadline = ctx.Deadline()
		return errors.New("fallback failure")
	})
	if err == nil || !hadDeadline {
		t.Fatalf("cleanup fallback was not finite: deadline=%t err=%v", hadDeadline, err)
	}
}

func TestAmbiguousRegistrationRejectsConcurrentIdenticalWriter(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		attempted := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap)
		competitor := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: attempted.Name,
			Namespace: attempted.Namespace, UID: "competitor-uid", ResourceVersion: "7"},
			Data: copyStringMap(attempted.Data)}
		if err := client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			competitor, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("simulated failed request before apply")
	})
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
	err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}, &byohInstancesState{})
	if err == nil || lease.RegistrationOwned {
		t.Fatalf("concurrent identical writer established false ownership: owned=%t err=%v",
			lease.RegistrationOwned, err)
	}
	current, getErr := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
		metav1.GetOptions{})
	if getErr != nil || current.UID != "competitor-uid" ||
		current.Annotations[registrationOwnershipAnnotation(lease.Address)] != "" {
		t.Fatalf("concurrent writer was changed: current=%#v err=%v", current, getErr)
	}
}

func TestRegistrationConflictRetriesPreserveUnownedConfigMaps(t *testing.T) {
	resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	lease := func() *byohHostLease {
		return &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
	}
	t.Run("replacement after retained object update conflict", func(t *testing.T) {
		original := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "original-uid", ResourceVersion: "1",
			Labels: map[string]string{"example.test/original": "true"}},
			Data: map[string]string{"original": "username=original"}}
		replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "replacement-uid", ResourceVersion: "7",
			Labels:      map[string]string{"example.test/replacement": "preserve"},
			Annotations: map[string]string{"example.test/annotation": "preserve"}},
			Data: map[string]string{"replacement": "username=replacement"}}
		client := fake.NewSimpleClientset(original)
		updates := 0
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			if err := client.Tracker().Delete(resource, wmcoNamespace, byohInstancesConfigMap); err != nil {
				t.Fatal(err)
			}
			if err := client.Tracker().Create(resource, replacement.DeepCopy(), wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"},
				byohInstancesConfigMap, errors.New("replacement won the update race"))
		})
		selected := lease()
		state := &byohInstancesState{}
		err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{selected}, state)
		if err == nil || selected.RegistrationOwned || state.ownedUID != "" {
			t.Fatalf("replacement ConfigMap established registration ownership: state=%#v lease=%#v err=%v",
				state, selected, err)
		}
		current, getErr := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if getErr != nil || updates != 1 || !reflect.DeepEqual(current, replacement) {
			t.Fatalf("replacement ConfigMap was mutated: updates=%d current=%#v want=%#v err=%v",
				updates, current, replacement, getErr)
		}
	})

	t.Run("concurrent create conflict without ownership evidence", func(t *testing.T) {
		concurrent := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "concurrent-uid", ResourceVersion: "4",
			Labels:      map[string]string{"example.test/concurrent": "preserve"},
			Annotations: map[string]string{"example.test/annotation": "preserve"}},
			Data: map[string]string{"concurrent": "username=concurrent"}}
		client := fake.NewSimpleClientset()
		creates, updates := 0, 0
		client.PrependReactor("create", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			creates++
			if err := client.Tracker().Create(resource, concurrent.DeepCopy(), wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"},
				byohInstancesConfigMap, errors.New("concurrent create won"))
		})
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			return false, nil, nil
		})
		selected := lease()
		state := &byohInstancesState{}
		err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{selected}, state)
		if err == nil || selected.RegistrationOwned || state.ownedUID != "" {
			t.Fatalf("concurrently created ConfigMap established registration ownership: state=%#v lease=%#v err=%v",
				state, selected, err)
		}
		current, getErr := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if getErr != nil || creates != 1 || updates != 0 || !reflect.DeepEqual(current, concurrent) {
			t.Fatalf("concurrently created ConfigMap was mutated: creates=%d updates=%d current=%#v want=%#v err=%v",
				creates, updates, current, concurrent, getErr)
		}
	})

	t.Run("create conflict with exact applied ownership evidence", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		creates, updates := 0, 0
		client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			creates++
			applied := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			applied.UID, applied.ResourceVersion = "applied-uid", "1"
			if err := client.Tracker().Create(resource, applied, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"},
				byohInstancesConfigMap, errors.New("create response reported a conflict after apply"))
		})
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			return false, nil, nil
		})
		selected := lease()
		state := &byohInstancesState{}
		err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{selected}, state)
		if err != nil || !selected.RegistrationOwned || state.ownedUID != "applied-uid" ||
			creates != 1 || updates != 0 {
			t.Fatalf("exact applied create ownership was not reconciled: creates=%d updates=%d state=%#v lease=%#v err=%v",
				creates, updates, state, selected, err)
		}
	})
}

func TestRegistrationNeverOverwritesPreexistingOwnershipMetadata(t *testing.T) {
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
	key := registrationOwnershipAnnotation(lease.Address)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
		Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
		Annotations: map[string]string{key: "preexisting-value"}}, Data: map[string]string{}}
	client := fake.NewSimpleClientset(cm)
	err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}, &byohInstancesState{})
	if err == nil || lease.RegistrationOwned {
		t.Fatalf("preexisting metadata was accepted as writable: owned=%t err=%v", lease.RegistrationOwned, err)
	}
	current, getErr := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
		metav1.GetOptions{})
	if getErr != nil || current.Annotations[key] != "preexisting-value" || current.Data[lease.Address] != "" {
		t.Fatalf("preexisting metadata/data was overwritten: current=%#v err=%v", current, getErr)
	}
}

func TestWindowsInstancesRetainedUIDAndDeletePreconditions(t *testing.T) {
	t.Run("generic delete submits retained UID and latest resourceVersion", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		var submitted metav1.DeleteOptions
		client.PrependReactor("delete", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			submitted = action.(clienttesting.DeleteAction).GetDeleteOptions()
			return false, nil, nil
		})
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, false, nil, nil); err != nil {
			t.Fatal(err)
		}
		if submitted.Preconditions == nil || submitted.Preconditions.UID == nil ||
			*submitted.Preconditions.UID != state.ownedUID || submitted.Preconditions.ResourceVersion == nil ||
			*submitted.Preconditions.ResourceVersion != "1" {
			t.Fatalf("generic delete did not submit retained UID/latest RV: %#v", submitted.Preconditions)
		}
	})

	t.Run("generic unregister preserves replacement ConfigMap", func(t *testing.T) {
		client := fakeClientAssigningConfigMapUID(t)
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		owned, err := client.Tracker().Get(resource, wmcoNamespace, byohInstancesConfigMap)
		if err != nil {
			t.Fatalf("read tracked windows-instances: %v", err)
		}
		ownedConfigMap, ok := owned.(*corev1.ConfigMap)
		if !ok {
			t.Fatalf("tracked windows-instances has unexpected type %T", owned)
		}
		replacement := ownedConfigMap.DeepCopy()
		replacement.UID, replacement.ResourceVersion = "replacement-uid", "9"
		if err := client.Tracker().Delete(resource, wmcoNamespace, byohInstancesConfigMap); err != nil {
			t.Fatal(err)
		}
		if err := client.Tracker().Create(resource, replacement, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, false, nil, nil); err == nil {
			t.Fatal("replacement ConfigMap was accepted as the owned object")
		}
		current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil || current.UID != "replacement-uid" || current.Data[lease.Address] == "" {
			t.Fatalf("replacement ConfigMap was mutated: current=%#v err=%v", current, err)
		}
	})

	t.Run("entry withdrawal preserves empty object identity and arbitrary metadata", func(t *testing.T) {
		instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
			Labels:      map[string]string{"example.test/label": "preserve"},
			Annotations: map[string]string{"example.test/annotation": "preserve"}}, Data: map[string]string{}}
		client := fake.NewSimpleClientset(instances)
		lease := &byohHostLease{Address: "host.example.test", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		deletes := 0
		client.PrependReactor("delete", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			deletes++
			return false, nil, nil
		})
		if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, true, nil, nil); err != nil {
			t.Fatal(err)
		}
		current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(),
			byohInstancesConfigMap, metav1.GetOptions{})
		if err != nil || deletes != 0 || current.UID != instances.UID || len(current.Data) != 0 ||
			current.Labels["example.test/label"] != "preserve" ||
			current.Annotations["example.test/annotation"] != "preserve" {
			t.Fatalf("entry withdrawal changed the ConfigMap object or metadata: deletes=%d current=%#v err=%v",
				deletes, current, err)
		}
	})
}

func TestAppliedIDMSUnknownOwnershipIsReconciledDuringCleanup(t *testing.T) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClient(scheme)
	resource := client.Resource(byohIDMSGVR)
	created := false
	allowGet := false
	client.PrependReactor("get", "imagedigestmirrorsets", func(clienttesting.Action) (bool, runtime.Object, error) {
		if created && !allowGet {
			return true, nil, errors.New("transient IDMS GET failure")
		}
		return false, nil, nil
	})
	client.PrependReactor("create", "imagedigestmirrorsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		object.SetUID("idms-uid")
		object.SetResourceVersion("3")
		if err := client.Tracker().Create(byohIDMSGVR, object, ""); err != nil {
			t.Fatal(err)
		}
		created = true
		return true, nil, errors.New("simulated create response loss")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	ownership, err := createOwnedIDMS(ctx, resource, "winc-82694-test", "lease-a")
	cancel()
	if err == nil || !ownership.attempted || ownership.owned {
		t.Fatalf("failed first reconciliation did not retain unknown attempt: ownership=%#v err=%v", ownership, err)
	}
	allowGet = true
	var submitted metav1.DeleteOptions
	client.PrependReactor("delete", "imagedigestmirrorsets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		submitted = action.(clienttesting.DeleteAction).GetDeleteOptions()
		return false, nil, nil
	})
	fixture := &byohTestFixture{dynamicClient: client, idmsName: "winc-82694-test", idmsLeaseID: "lease-a",
		idmsAttempted: ownership.attempted, idmsOwned: ownership.owned, idmsUID: ownership.uid}
	if err := fixture.deleteSyntheticIDMSWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if submitted.Preconditions == nil || submitted.Preconditions.UID == nil ||
		*submitted.Preconditions.UID != "idms-uid" || submitted.Preconditions.ResourceVersion == nil ||
		*submitted.Preconditions.ResourceVersion != "3" {
		t.Fatalf("cleanup did not submit reconciled IDMS UID/RV: %#v", submitted.Preconditions)
	}
}

func TestNodeDeleteRefreshesResourceVersionAndPreservesReplacement(t *testing.T) {
	retained := readyBYOHNode("node-a", "old-uid", "192.0.2.10")
	retained.ResourceVersion = "4"
	current := retained.DeepCopy()
	current.ResourceVersion = "9"
	client := fake.NewSimpleClientset(current)
	resource := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	var submitted metav1.DeleteOptions
	client.PrependReactor("delete", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		submitted = action.(clienttesting.DeleteAction).GetDeleteOptions()
		if err := client.Tracker().Delete(resource, "", retained.Name); err != nil {
			t.Fatal(err)
		}
		replacement := current.DeepCopy()
		replacement.UID, replacement.ResourceVersion = "replacement-uid", "10"
		if err := client.Tracker().Create(resource, replacement, ""); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, retained.Name,
			errors.New("UID precondition failed"))
	})
	err := deleteNodeByRetainedIdentity(context.Background(), client.CoreV1().Nodes(), retained.Name, retained.UID)
	if err == nil {
		t.Fatal("replacement race did not fail the retained-identity delete")
	}
	if submitted.Preconditions == nil || submitted.Preconditions.UID == nil ||
		*submitted.Preconditions.UID != retained.UID || submitted.Preconditions.ResourceVersion == nil ||
		*submitted.Preconditions.ResourceVersion != current.ResourceVersion {
		t.Fatalf("Node delete omitted retained UID/RV: %#v", submitted.Preconditions)
	}
	replacement, getErr := client.CoreV1().Nodes().Get(context.Background(), retained.Name, metav1.GetOptions{})
	if getErr != nil || replacement.UID != "replacement-uid" {
		t.Fatalf("same-name replacement Node was not preserved: current=%#v err=%v", replacement, getErr)
	}
}

func TestCleanupBudgetIsAggregateOrderedAndErrorJoining(t *testing.T) {
	started := time.Now()
	mainCtx, fallbackCtx, cancel := newBYOHCleanupContexts(80*time.Millisecond, 25*time.Millisecond)
	defer cancel()
	order := []string{}
	operation := func(name string, waitForExpiry bool) func(context.Context) error {
		return func(ctx context.Context) error {
			order = append(order, name)
			if waitForExpiry {
				<-ctx.Done()
			}
			return fmt.Errorf("%s failed: %w", name, ctx.Err())
		}
	}
	var cleanupErrors []error
	for _, step := range []struct {
		name          string
		ctx           context.Context
		waitForExpiry bool
	}{
		{name: "restore", ctx: mainCtx, waitForExpiry: true},
		{name: "deconfigure", ctx: mainCtx},
		{name: "IDMS", ctx: fallbackCtx, waitForExpiry: true},
		{name: "quarantine", ctx: fallbackCtx},
	} {
		if err := operation(step.name, step.waitForExpiry)(step.ctx); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	err := errors.Join(cleanupErrors...)
	elapsed := time.Since(started)
	if !reflect.DeepEqual(order, []string{"restore", "deconfigure", "IDMS", "quarantine"}) {
		t.Fatalf("cleanup attempts were out of order or skipped: %v", order)
	}
	if err == nil || !strings.Contains(err.Error(), "restore") || !strings.Contains(err.Error(), "deconfigure") ||
		!strings.Contains(err.Error(), "IDMS") || !strings.Contains(err.Error(), "quarantine") {
		t.Fatalf("cleanup errors were not aggregated: %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("shared cleanup budget exceeded bounded tolerance: %v", elapsed)
	}
}

func TestWMCOLogMatcherRejectsIdentityPrefixCollisions(t *testing.T) {
	since := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	lease := &byohHostLease{Address: "host-a.example.test", NodeName: "node-a"}
	collisions := []string{
		`2026-10-04T12:00:01Z deconfiguring node-aa`,
		`2026-10-04T12:00:01Z deconfiguring other-node-a`,
		`2026-10-04T12:00:01Z deconfiguring node-a-other`,
		`2026-10-04T12:00:01Z {"logger":"nc other-host-a.example.test","msg":"deconfiguring"}`,
		`2026-10-04T12:00:01Z {"logger":"nc host-a.example.test-other","msg":"deconfiguring"}`,
	}
	for _, line := range collisions {
		if wmcoLogMatchesLeaseSince(line, lease, "deconfiguring", since) {
			t.Fatalf("prefix-colliding identity was accepted: %s", line)
		}
	}
	for _, line := range []string{
		`2026-10-04T12:00:01Z deconfiguring "node-a"`,
		`2026-10-04T12:00:01Z {"logger":"nc host-a.example.test","msg":"deconfiguring"}`,
	} {
		if !wmcoLogMatchesLeaseSince(line, lease, "deconfiguring", since) {
			t.Fatalf("exact quoted/console identity was rejected: %s", line)
		}
	}
}

func TestWMCOLogReadHonorsContextCancellation(t *testing.T) {
	logRequestCanceled := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/pods") {
			if _, err := response.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"wmco-operator","namespace":"` +
				wmcoNamespace + `","labels":{"name":"windows-machine-config-operator"}}}]}`)); err != nil {
				t.Errorf("write fake PodList response: %v", err)
			}
			return
		}
		if strings.HasSuffix(request.URL.Path, "/pods/wmco-operator/log") {
			<-request.Context().Done()
			once.Do(func() { close(logRequestCanceled) })
			return
		}
		http.NotFound(response, request)
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL,
		ContentConfig: rest.ContentConfig{ContentType: runtime.ContentTypeJSON}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = readWMCOLogsSince(ctx, client.CoreV1(), time.Now().UTC())
	if err == nil || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("typed Pod log request ignored cancellation: elapsed=%v err=%v", time.Since(started), err)
	}
	select {
	case <-logRequestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("server-side Pod log request context was not canceled")
	}
}

func TestDeconfigurationRetryRetainsOriginalMutationBoundary(t *testing.T) {
	lease := &byohHostLease{RegistrationOwned: true, Registered: true}
	order := []string{}
	mutations, verifications := 0, 0
	mutate := func() error {
		mutations++
		order = append(order, "unregister")
		captureDeconfigurationBoundary(lease)
		return nil
	}
	verify := func() error {
		verifications++
		order = append(order, "verify")
		if verifications == 1 {
			return errors.New("transient verification failure")
		}
		return nil
	}
	if err := runLeaseDeconfiguration(lease, mutate, verify); err == nil {
		t.Fatal("first transient verification failure was ignored")
	}
	originalBoundary := lease.DeconfigurationStarted
	time.Sleep(time.Millisecond)
	if err := runLeaseDeconfiguration(lease, mutate, verify); err != nil {
		t.Fatal(err)
	}
	if mutations != 1 || verifications != 2 || !lease.DeconfigurationStarted.Equal(originalBoundary) ||
		!reflect.DeepEqual(order, []string{"unregister", "verify", "verify"}) {
		t.Fatalf("retry moved boundary or repeated mutation: mutations=%d verifications=%d order=%v original=%v current=%v",
			mutations, verifications, order, originalBoundary, lease.DeconfigurationStarted)
	}
}

func TestDeconfigurationBoundaryAdvancesOnlyAfterProvenUnappliedMutation(t *testing.T) {
	t.Run("exact owned readback clears boundary for the next mutation", func(t *testing.T) {
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a",
			RegistrationOwned: true, Registered: true, PreserveInstancesConfigMap: true}
		key := registrationOwnershipAnnotation(lease.Address)
		instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
			Annotations: map[string]string{key: lease.LeaseID}},
			Data: map[string]string{lease.Address: "username=" + lease.Username}}
		client := fake.NewSimpleClientset(instances)
		updates := 0
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			if updates == 1 {
				return true, nil, errors.New("request failed before apply")
			}
			return false, nil, nil
		})
		state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: instances.UID,
			ownedUID: instances.UID, originalOwnership: map[string]*string{key: nil}}
		order := []string{}
		mutate := func() error {
			return unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
				[]*byohHostLease{lease}, state, true,
				func() {
					order = append(order, "mutation")
					captureDeconfigurationBoundary(lease)
				}, func() {
					order = append(order, "proven-unapplied")
					clearDeconfigurationBoundary(lease)
				})
		}
		verify := func() error {
			order = append(order, "verify")
			return nil
		}
		if err := runLeaseDeconfiguration(lease, mutate, verify); err == nil {
			t.Fatal("proven-unapplied mutation unexpectedly succeeded")
		}
		if !lease.DeconfigurationStarted.IsZero() || lease.Unregistered {
			t.Fatalf("proven-unapplied mutation retained stale evidence: boundary=%v unregistered=%t",
				lease.DeconfigurationStarted, lease.Unregistered)
		}
		if err := runLeaseDeconfiguration(lease, mutate, verify); err != nil {
			t.Fatal(err)
		}
		if updates != 2 || lease.DeconfigurationStarted.IsZero() || !lease.Unregistered ||
			!reflect.DeepEqual(order, []string{"mutation", "proven-unapplied", "mutation", "verify"}) {
			t.Fatalf("retry did not advance the mutation boundary in production order: updates=%d order=%v lease=%#v",
				updates, order, lease)
		}
	})

	t.Run("observation error retains ambiguous boundary", func(t *testing.T) {
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a",
			RegistrationOwned: true, Registered: true, PreserveInstancesConfigMap: true}
		key := registrationOwnershipAnnotation(lease.Address)
		instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap,
			Namespace: wmcoNamespace, UID: "instances-uid", ResourceVersion: "1",
			Annotations: map[string]string{key: lease.LeaseID}},
			Data: map[string]string{lease.Address: "username=" + lease.Username}}
		client := fake.NewSimpleClientset(instances)
		writeAttempted := false
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			writeAttempted = true
			return true, nil, errors.New("ambiguous update failure")
		})
		client.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			if writeAttempted {
				return true, nil, errors.New("observation failed")
			}
			return false, nil, nil
		})
		state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: instances.UID,
			ownedUID: instances.UID, originalOwnership: map[string]*string{key: nil}}
		provenUnapplied := false
		err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state, true, func() { captureDeconfigurationBoundary(lease) },
			func() { provenUnapplied = true; clearDeconfigurationBoundary(lease) })
		if err == nil || lease.DeconfigurationStarted.IsZero() || provenUnapplied {
			t.Fatalf("observation error was mistaken for proven-unapplied mutation: boundary=%v proven=%t err=%v",
				lease.DeconfigurationStarted, provenUnapplied, err)
		}
	})
}

func TestOwnedHostProcessProbeGuardsNodeAndPodIdentity(t *testing.T) {
	newClient := func(t *testing.T, phase corev1.PodPhase, createErr error) (*fake.Clientset, *byohHostLease) {
		t.Helper()
		node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
		client := fake.NewSimpleClientset(node)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			pod.UID, pod.ResourceVersion, pod.Status.Phase = "probe-uid", "1", phase
			if err := client.Tracker().Create(resource, pod, wmcoNamespace); err != nil {
				t.Fatalf("track created HostProcess Pod: %v", err)
			}
			return true, pod, createErr
		})
		return client, &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
	}

	t.Run("applied create response loss is reconciled and UID-deleted", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodSucceeded, errors.New("simulated create response loss"))
		var deleted metav1.DeleteOptions
		client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			deleted = action.(clienttesting.DeleteAction).GetDeleteOptions()
			return false, nil, nil
		})
		output, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease,
			"Write-Output 'safe'", func(_ context.Context, namespace, name string, uid types.UID) ([]byte, error) {
				if namespace != wmcoNamespace || name == "" || uid != "probe-uid" {
					t.Fatalf("log read omitted retained Pod identity: namespace=%q name=%q uid=%q", namespace, name, uid)
				}
				return []byte("safe"), nil
			})
		if err != nil || output != "safe" {
			t.Fatalf("reconciled HostProcess probe failed: output=%q err=%v", output, err)
		}
		if deleted.Preconditions == nil || deleted.Preconditions.UID == nil || *deleted.Preconditions.UID != "probe-uid" ||
			deleted.Preconditions.ResourceVersion == nil || *deleted.Preconditions.ResourceVersion != "1" {
			t.Fatalf("HostProcess cleanup omitted UID/latest-RV preconditions: %#v", deleted.Preconditions)
		}
	})

	t.Run("replacement Node aborts before logs", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodSucceeded, nil)
		gets := 0
		client.PrependReactor("get", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
			gets++
			node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
			if gets > 1 {
				node.UID = "replacement-uid"
			}
			return true, node, nil
		})
		logsRead := false
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, "safe",
			func(context.Context, string, string, types.UID) ([]byte, error) {
				logsRead = true
				return nil, nil
			})
		if err == nil || logsRead || !lease.QuarantineRequired {
			t.Fatalf("replacement Node did not abort and quarantine before logs: logsRead=%t quarantine=%t err=%v",
				logsRead, lease.QuarantineRequired, err)
		}
	})

	t.Run("post-probe Node GET uncertainty requires quarantine", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodSucceeded, nil)
		gets := 0
		client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets > 1 {
				return true, nil, errors.New("post-probe Node GET unavailable")
			}
			return false, nil, nil
		})
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, "safe",
			func(context.Context, string, string, types.UID) ([]byte, error) { return []byte("safe"), nil })
		if err == nil || !lease.QuarantineRequired {
			t.Fatalf("post-probe Node GET uncertainty was not quarantined: quarantine=%t err=%v",
				lease.QuarantineRequired, err)
		}
	})

	t.Run("wrong Pod UID and name abort before logs and delete", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodSucceeded, nil)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		gets := 0
		client.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			gets++
			if gets == 1 {
				return false, nil, nil
			}
			name := action.(clienttesting.GetAction).GetName()
			object, err := client.Tracker().Get(resource, wmcoNamespace, name)
			if err != nil {
				return true, nil, err
			}
			pod := object.(*corev1.Pod).DeepCopy()
			pod.Name, pod.UID = "replacement-name", "replacement-uid"
			return true, pod, nil
		})
		deleted := false
		client.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			deleted = true
			return false, nil, nil
		})
		logsRead := false
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, "safe",
			func(context.Context, string, string, types.UID) ([]byte, error) {
				logsRead = true
				return nil, nil
			})
		if err == nil || logsRead || deleted {
			t.Fatalf("wrong Pod identity was not fail-closed: logsRead=%t deleted=%t err=%v", logsRead, deleted, err)
		}
	})

	t.Run("cancellation deletes retained Pod and joins bounded operation", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodPending, nil)
		deleted := false
		client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			deleted = true
			return false, nil, nil
		})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := runBYOHHostProcessPS(ctx, client.CoreV1(), lease, "safe",
			func(context.Context, string, string, types.UID) ([]byte, error) { return nil, nil })
		if err == nil || !deleted || !lease.QuarantineRequired {
			t.Fatalf("canceled HostProcess probe did not clean and quarantine retained Pod: deleted=%t quarantine=%t err=%v",
				deleted, lease.QuarantineRequired, err)
		}
	})

	t.Run("failed probe never exposes script material", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodFailed, nil)
		const secretScript = "sensitive-key-material"
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, secretScript,
			func(context.Context, string, string, types.UID) ([]byte, error) { return []byte(secretScript), nil })
		if err == nil || strings.Contains(err.Error(), secretScript) {
			t.Fatalf("failed HostProcess error exposed script material: %v", err)
		}
	})

	t.Run("Create error echoing script and encoded file is sanitized across joined failures", func(t *testing.T) {
		node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
		client := fake.NewSimpleClientset(node)
		const script = "private-script-material"
		encodedFile := base64.StdEncoding.EncodeToString([]byte("entire-authorized-keys-file"))
		client.PrependReactor("create", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, fmt.Errorf("admission echoed %s and %s", script, encodedFile)
		})
		lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, script+encodedFile,
			func(context.Context, string, string, types.UID) ([]byte, error) { return nil, nil })
		joined := errors.Join(err, errors.New("safe cleanup classification"))
		if err == nil || !lease.QuarantineRequired || strings.Contains(err.Error(), script) ||
			strings.Contains(err.Error(), encodedFile) || strings.Contains(joined.Error(), script) ||
			strings.Contains(joined.Error(), encodedFile) {
			t.Fatalf("script-bearing Create error was not sanitized and quarantined: quarantine=%t err=%v",
				lease.QuarantineRequired, err)
		}
	})

	for _, operation := range []string{"cleanup GET", "cleanup DELETE"} {
		t.Run(operation+" uncertainty is sanitized and quarantined", func(t *testing.T) {
			client, lease := newClient(t, corev1.PodSucceeded, nil)
			const sensitive = "encoded-authorized-key-snapshot"
			if operation == "cleanup GET" {
				gets := 0
				client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					gets++
					if gets == 4 {
						return true, nil, fmt.Errorf("server echoed %s", sensitive)
					}
					return false, nil, nil
				})
			} else {
				client.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("server echoed %s", sensitive)
				})
			}
			_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, sensitive,
				func(context.Context, string, string, types.UID) ([]byte, error) { return []byte("safe"), nil })
			if err == nil || !lease.QuarantineRequired || strings.Contains(err.Error(), sensitive) {
				t.Fatalf("%s uncertainty was not sanitized and quarantined: quarantine=%t err=%v",
					operation, lease.QuarantineRequired, err)
			}
		})
	}

	t.Run("log helper rejects wrong Pod UID before requesting logs", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: wmcoNamespace, UID: "actual"}}
		fixture := &byohTestFixture{coreClient: fake.NewSimpleClientset(pod).CoreV1()}
		if _, err := fixture.hostProcessLogs(context.Background(), wmcoNamespace, pod.Name, "wrong"); err == nil {
			t.Fatal("HostProcess log helper accepted a wrong retained Pod UID")
		}
	})

	t.Run("ambiguous create validates nonce and every privileged critical field", func(t *testing.T) {
		mutations := map[string]struct {
			mutate      func(*corev1.Pod)
			deleteOwned bool
		}{
			"command": {mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].Command = []string{"changed"} }, deleteOwned: true},
			"args":    {mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].Args = []string{"changed"} }, deleteOwned: true},
			"environment": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ProgramData", Value: `C:\redirected`}}
			}, deleteOwned: true},
			"environment source": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "injected"}}}}
			}, deleteOwned: true},
			"working directory": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].WorkingDir = `C:\redirected`
			}, deleteOwned: true},
			"init container": {mutate: func(pod *corev1.Pod) {
				pod.Spec.InitContainers = []corev1.Container{{Name: "injected", Image: windowsDebugImage}}
			}, deleteOwned: true},
			"ephemeral container": {mutate: func(pod *corev1.Pod) {
				pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "injected", Image: windowsDebugImage}}}
			}, deleteOwned: true},
			"volume": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Volumes = []corev1.Volume{{Name: "injected", VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
			}, deleteOwned: true},
			"volume mount": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "injected", MountPath: `C:\redirected`}}
			}, deleteOwned: true},
			"volume device": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].VolumeDevices = []corev1.VolumeDevice{{Name: "injected", DevicePath: `C:\device`}}
			}, deleteOwned: true},
			"lifecycle": {mutate: func(pod *corev1.Pod) {
				pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{
					Exec: &corev1.ExecAction{Command: []string{"injected"}}}}
			}, deleteOwned: true},
			"image": {mutate: func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "changed.invalid/image" }, deleteOwned: true},
			"node":  {mutate: func(pod *corev1.Pod) { pod.Spec.NodeName = "other-node" }, deleteOwned: true},
			"security": {mutate: func(pod *corev1.Pod) {
				value := false
				pod.Spec.Containers[0].SecurityContext.WindowsOptions.HostProcess = &value
			}, deleteOwned: true},
			"nonce": {mutate: func(pod *corev1.Pod) {
				pod.Annotations["windowsmachineconfig.openshift.io/ownership-nonce"] = "not-the-attempted-nonce"
			}, deleteOwned: false},
		}
		for name, test := range mutations {
			t.Run(name, func(t *testing.T) {
				node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
				client := fake.NewSimpleClientset(node)
				resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
				client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
					pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
					pod.UID, pod.ResourceVersion, pod.Status.Phase = "owned-uid", "1", corev1.PodSucceeded
					test.mutate(pod)
					if err := client.Tracker().Create(resource, pod, wmcoNamespace); err != nil {
						t.Fatal(err)
					}
					return true, nil, errors.New("create response lost")
				})
				deleted := false
				client.PrependReactor("delete", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					deleted = true
					return false, nil, nil
				})
				lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
				_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, "safe",
					func(context.Context, string, string, types.UID) ([]byte, error) { return []byte("forged"), nil })
				if err == nil || deleted != test.deleteOwned || !lease.QuarantineRequired {
					t.Fatalf("critical mutation was not fail-closed: deleted=%t want=%t quarantine=%t err=%v",
						deleted, test.deleteOwned, lease.QuarantineRequired, err)
				}
			})
		}
	})

	t.Run("documented API defaults do not create a false critical-spec mismatch", func(t *testing.T) {
		attempted := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "probe"}}}}
		current := attempted.DeepCopy()
		thirty, enabled := int64(30), true
		current.Spec.ServiceAccountName = "default"
		current.Spec.DeprecatedServiceAccount = "default"
		current.Spec.DNSPolicy = corev1.DNSClusterFirst
		current.Spec.SchedulerName = corev1.DefaultSchedulerName
		current.Spec.TerminationGracePeriodSeconds = &thirty
		current.Spec.EnableServiceLinks = &enabled
		seconds := int64(300)
		current.Spec.Tolerations = append(current.Spec.Tolerations,
			corev1.Toleration{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
			corev1.Toleration{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists,
				Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds})
		current.Spec.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
		current.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		if !hostProcessPodCriticalSpecMatches(current, attempted) {
			t.Fatal("documented Pod API defaults were rejected as privileged execution drift")
		}
		current.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ProgramData", Value: `C:\redirected`}}
		if hostProcessPodCriticalSpecMatches(current, attempted) {
			t.Fatal("non-defaulted privileged execution input was normalized away")
		}
	})

	t.Run("applied create followed by request cancellation uses bounded owned cleanup", func(t *testing.T) {
		node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
		client := fake.NewSimpleClientset(node)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			pod.UID, pod.ResourceVersion, pod.Status.Phase = "owned-uid", "1", corev1.PodPending
			if err := client.Tracker().Create(resource, pod, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			cancel()
			return true, nil, context.Canceled
		})
		deleted := false
		client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			options := action.(clienttesting.DeleteAction).GetDeleteOptions()
			deleted = options.Preconditions != nil && options.Preconditions.UID != nil &&
				*options.Preconditions.UID == "owned-uid"
			return false, nil, nil
		})
		lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
		started := time.Now()
		_, err := runBYOHHostProcessPS(ctx, client.CoreV1(), lease, "private-material",
			func(context.Context, string, string, types.UID) ([]byte, error) { return nil, nil })
		if err == nil || !deleted || !lease.QuarantineRequired || time.Since(started) > 250*time.Millisecond ||
			strings.Contains(err.Error(), "private-material") {
			t.Fatalf("canceled response-loss cleanup was unsafe: deleted=%t quarantine=%t elapsed=%v err=%v",
				deleted, lease.QuarantineRequired, time.Since(started), err)
		}
	})

	t.Run("same-name replacement during log retrieval is rejected", func(t *testing.T) {
		client, lease := newClient(t, corev1.PodSucceeded, nil)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		_, err := runBYOHHostProcessPS(context.Background(), client.CoreV1(), lease, "safe",
			func(_ context.Context, namespace, name string, _ types.UID) ([]byte, error) {
				if err := client.Tracker().Delete(resource, namespace, name); err != nil {
					t.Fatal(err)
				}
				replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
					UID: "replacement-uid", ResourceVersion: "2"}}
				if err := client.Tracker().Create(resource, replacement, namespace); err != nil {
					t.Fatal(err)
				}
				return []byte("replacement-output"), nil
			})
		if err == nil || !lease.QuarantineRequired {
			t.Fatalf("replacement Pod output was accepted: quarantine=%t err=%v", lease.QuarantineRequired, err)
		}
	})
}

func TestHostProcessCleanupCannotCrossOuterDeadline(t *testing.T) {
	for _, blockOperation := range []string{"cleanup GET", "cleanup DELETE"} {
		t.Run(blockOperation, func(t *testing.T) {
			node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
			var pod *corev1.Pod
			var mu sync.Mutex
			podGets := 0
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				switch {
				case request.Method == http.MethodGet && request.URL.Path == "/api/v1/nodes/node-a":
					_ = json.NewEncoder(writer).Encode(node)
				case request.Method == http.MethodPost && request.URL.Path == "/api/v1/namespaces/openshift-windows-machine-config-operator/pods":
					body, readErr := io.ReadAll(request.Body)
					if readErr != nil {
						t.Errorf("read Pod create: %v", readErr)
					}
					object, _, decodeErr := decoder.Decode(body, nil, nil)
					candidate, ok := object.(*corev1.Pod)
					if decodeErr != nil || !ok {
						t.Errorf("decode Pod create: object=%T err=%v", object, decodeErr)
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					candidate = candidate.DeepCopy()
					candidate.UID, candidate.ResourceVersion, candidate.Status.Phase = "pod-uid", "1", corev1.PodSucceeded
					mu.Lock()
					pod = candidate
					mu.Unlock()
					_ = json.NewEncoder(writer).Encode(candidate)
				case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path,
					"/api/v1/namespaces/openshift-windows-machine-config-operator/pods/"):
					mu.Lock()
					podGets++
					current, currentGet := pod, podGets
					mu.Unlock()
					if blockOperation == "cleanup GET" && currentGet == 4 {
						select {
						case <-request.Context().Done():
						case <-time.After(300 * time.Millisecond):
						}
					}
					if current == nil {
						writer.WriteHeader(http.StatusNotFound)
						_, _ = writer.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
						return
					}
					_ = json.NewEncoder(writer).Encode(current)
				case request.Method == http.MethodDelete:
					if blockOperation == "cleanup DELETE" {
						select {
						case <-request.Context().Done():
						case <-time.After(300 * time.Millisecond):
						}
					}
				default:
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer func() {
				server.CloseClientConnections()
				server.Close()
			}()
			client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err = runBYOHHostProcessPS(ctx, client.CoreV1(), lease, "safe",
				func(context.Context, string, string, types.UID) ([]byte, error) { return []byte("safe"), nil })
			elapsed := time.Since(started)
			if err == nil || elapsed > 250*time.Millisecond || !lease.QuarantineRequired {
				t.Fatalf("HostProcess %s crossed aggregate deadline or omitted quarantine: elapsed=%v quarantine=%t err=%v",
					blockOperation, elapsed, lease.QuarantineRequired, err)
			}
		})
	}
}

func TestAuthorizedKeyMutationIsExactCaseSensitiveAndIdempotent(t *testing.T) {
	owned := testHostPublicKey(t)
	other := testHostPublicKey(t)
	caseVariant := "SSH" + owned[3:]
	tests := map[string]struct {
		content, placed string
	}{
		"empty":           {content: "", placed: owned + "\r\n"},
		"exact":           {content: owned, placed: owned},
		"comment prefix":  {content: "# " + owned + "\r\n", placed: "# " + owned + "\r\n" + owned + "\r\n"},
		"case variant":    {content: caseVariant + "\n", placed: caseVariant + "\n" + owned + "\n"},
		"other key":       {content: other + "\n", placed: other + "\n" + owned + "\n"},
		"CRLF":            {content: other + "\r\n", placed: other + "\r\n" + owned + "\r\n"},
		"duplicate owned": {content: owned + "\r\n" + owned + "\r\n", placed: owned + "\r\n"},
		"multiline":       {content: "header\n" + other, placed: "header\n" + other + "\n" + owned},
		"quote content":   {content: "'unrelated'\r\n", placed: "'unrelated'\r\n" + owned + "\r\n"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			placed, err := mutateAuthorizedKeyContent([]byte(test.content), owned, false)
			if err != nil || string(placed) != test.placed {
				t.Fatalf("exact placement mismatch: got=%q want=%q err=%v", placed, test.placed, err)
			}
			placedAgain, err := mutateAuthorizedKeyContent(placed, owned, false)
			if err != nil || !bytes.Equal(placedAgain, placed) {
				t.Fatalf("double placement was not idempotent: first=%q second=%q err=%v", placed, placedAgain, err)
			}
			removed, err := mutateAuthorizedKeyContent(placedAgain, owned, true)
			removedLines := strings.Split(strings.ReplaceAll(string(removed), "\r\n", "\n"), "\n")
			if err != nil || slices.Contains(removedLines, owned) {
				t.Fatalf("owned exact line was not removed: content=%q err=%v", removed, err)
			}
			removedAgain, err := mutateAuthorizedKeyContent(removed, owned, true)
			if err != nil || !bytes.Equal(removedAgain, removed) {
				t.Fatalf("double removal was not idempotent: first=%q second=%q err=%v", removed, removedAgain, err)
			}
			for _, unrelated := range []string{other, caseVariant, "# " + owned, "'unrelated'"} {
				if strings.Contains(test.content, unrelated) && !bytes.Contains(removed, []byte(unrelated)) {
					t.Fatalf("removal deleted unrelated content %q from %q", unrelated, removed)
				}
			}
		})
	}
	bomContent := append([]byte{0xef, 0xbb, 0xbf}, []byte(other+"\r\n")...)
	bomPlaced, err := mutateAuthorizedKeyContent(bomContent, owned, false)
	if err != nil || !bytes.HasPrefix(bomPlaced, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatalf("UTF-8 BOM was not preserved: %x err=%v", bomPlaced, err)
	}
	for _, invalid := range []string{"", owned + "\n" + other, owned + " comment", "'" + owned + "'"} {
		if _, err := normalizeAuthorizedKeyLine(invalid); err == nil {
			t.Fatalf("malformed or multiline authorized key was accepted: %q", invalid)
		}
	}
}

func TestAuthorizedKeyProductionHostProcessProtocol(t *testing.T) {
	type behavior struct {
		outputs         map[int]string
		phases          map[int]corev1.PodPhase
		createErrors    map[int]error
		unappliedCreate int
		logErrors       map[int]error
		cancelOnCreate  int
	}
	type protocolObservation struct {
		scripts []string
		pods    []*corev1.Pod
		deletes []metav1.DeleteOptions
	}
	newFixture := func(t *testing.T, ctx context.Context, cancel context.CancelFunc,
		b behavior) (*byohTestFixture, *byohHostLease, *protocolObservation) {
		t.Helper()
		node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
		client := fake.NewSimpleClientset(node)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
		observation := &protocolObservation{}
		byName := map[string]int{}
		creates := 0
		client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			creates++
			pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
			if len(pod.Spec.Containers) != 1 || len(pod.Spec.Containers[0].Command) == 0 {
				t.Fatal("production authorized-key method created an incomplete HostProcess Pod")
			}
			observation.scripts = append(observation.scripts,
				pod.Spec.Containers[0].Command[len(pod.Spec.Containers[0].Command)-1])
			observation.pods = append(observation.pods, pod.DeepCopy())
			byName[pod.Name] = creates
			if creates == b.unappliedCreate {
				return true, nil, b.createErrors[creates]
			}
			pod.UID = types.UID(fmt.Sprintf("authorized-key-pod-%d", creates))
			pod.ResourceVersion = fmt.Sprintf("initial-rv-%d", creates)
			pod.Status.Phase = corev1.PodSucceeded
			if phase := b.phases[creates]; phase != "" {
				pod.Status.Phase = phase
			}
			if err := client.Tracker().Create(resource, pod, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			if creates == b.cancelOnCreate && cancel != nil {
				cancel()
			}
			if err := b.createErrors[creates]; err != nil {
				return true, nil, err
			}
			return true, pod, nil
		})
		client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
			observation.deletes = append(observation.deletes,
				action.(clienttesting.DeleteAction).GetDeleteOptions())
			return false, nil, nil
		})
		fixture := &byohTestFixture{coreClient: client.CoreV1(), ctx: ctx}
		fixture.hostProcessLogsForTest = func(_ context.Context, namespace, name string, uid types.UID) ([]byte, error) {
			index := byName[name]
			object, err := client.Tracker().Get(resource, namespace, name)
			if err != nil {
				return nil, err
			}
			pod := object.(*corev1.Pod).DeepCopy()
			if pod.UID != uid {
				return nil, errors.New("retained Pod UID mismatch in test transport")
			}
			pod.ResourceVersion = fmt.Sprintf("latest-rv-%d", index)
			if err := client.Tracker().Update(resource, pod, namespace); err != nil {
				return nil, err
			}
			if err := b.logErrors[index]; err != nil {
				return nil, err
			}
			return []byte(b.outputs[index]), nil
		}
		lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "lease-a", HostID: "host-a"}
		return fixture, lease, observation
	}

	ownedKey := testHostPublicKey(t)
	current := append([]byte{0xef, 0xbb, 0xbf}, []byte("unrelated-key\r\n")...)
	desired, err := mutateAuthorizedKeyContent(current, ownedKey, false)
	if err != nil {
		t.Fatal(err)
	}
	encodedCurrent := base64.StdEncoding.EncodeToString(current)
	encodedDesired := base64.StdEncoding.EncodeToString(desired)

	t.Run("actual read CAS write verify scripts use two independently owned Pods", func(t *testing.T) {
		fixture, lease, observed := newFixture(t, context.Background(), nil, behavior{outputs: map[int]string{
			1: encodedCurrent, 2: ""}, phases: map[int]corev1.PodPhase{}, createErrors: map[int]error{},
			logErrors: map[int]error{}})
		if err := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false); err != nil {
			t.Fatalf("production authorized-key HostProcess method failed: %v", err)
		}
		if len(observed.pods) != 2 || len(observed.scripts) != 2 || len(observed.deletes) != 2 ||
			observed.pods[0].Name == observed.pods[1].Name {
			t.Fatalf("production protocol did not use two independently owned Pods: pods=%d scripts=%d deletes=%d",
				len(observed.pods), len(observed.scripts), len(observed.deletes))
		}
		pathAssignment := `$path="$env:ProgramData\ssh\administrators_authorized_keys";`
		for index, script := range observed.scripts {
			if !strings.Contains(script, pathAssignment) ||
				bytes.Contains([]byte(script), []byte{0x5c, 0x22}) ||
				observed.pods[index].Spec.NodeName != lease.NodeName {
				t.Fatalf("production script %d has unsafe path quoting or wrong retained Node identity", index)
			}
		}
		if !strings.Contains(observed.scripts[0], "[IO.File]::ReadAllBytes") ||
			!strings.Contains(observed.scripts[0], "[Convert]::ToBase64String") ||
			!strings.Contains(observed.scripts[1], "-cne '"+encodedCurrent+"'") ||
			!strings.Contains(observed.scripts[1], "[IO.File]::WriteAllBytes") ||
			!strings.Contains(observed.scripts[1], "-cne '"+encodedDesired+"'") {
			t.Fatal("production scripts omitted read/compare/write/post-write verification semantics")
		}
		for index, options := range observed.deletes {
			wantUID := types.UID(fmt.Sprintf("authorized-key-pod-%d", index+1))
			wantRV := fmt.Sprintf("latest-rv-%d", index+1)
			if options.Preconditions == nil || options.Preconditions.UID == nil ||
				*options.Preconditions.UID != wantUID || options.Preconditions.ResourceVersion == nil ||
				*options.Preconditions.ResourceVersion != wantRV {
				t.Fatalf("production Pod %d cleanup omitted retained UID/fresh-RV proof: %#v", index, options.Preconditions)
			}
		}
	})

	t.Run("concurrent file snapshot drift refuses CAS and quarantines", func(t *testing.T) {
		fixture, lease, observed := newFixture(t, context.Background(), nil, behavior{
			outputs: map[int]string{1: encodedCurrent}, phases: map[int]corev1.PodPhase{2: corev1.PodFailed},
			createErrors: map[int]error{}, logErrors: map[int]error{}})
		err := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false)
		if err == nil || !lease.QuarantineRequired || len(observed.scripts) != 2 ||
			!strings.Contains(observed.scripts[1], "-cne '"+encodedCurrent+"'") ||
			strings.Contains(err.Error(), encodedCurrent) || strings.Contains(err.Error(), encodedDesired) ||
			strings.Contains(err.Error(), ownedKey) {
			t.Fatalf("concurrent snapshot drift was not refused without data exposure: quarantine=%t err=%v",
				lease.QuarantineRequired, err)
		}
	})

	t.Run("applied and unapplied Create response loss are distinguished", func(t *testing.T) {
		const echoed = "script-and-authorized-key-file-echo"
		fixture, lease, _ := newFixture(t, context.Background(), nil, behavior{outputs: map[int]string{
			1: encodedCurrent, 2: ""}, phases: map[int]corev1.PodPhase{}, createErrors: map[int]error{
			2: errors.New(echoed)}, logErrors: map[int]error{}})
		if err := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false); err != nil {
			t.Fatalf("applied Create response loss was not reconciled: %v", err)
		}

		fixture, lease, _ = newFixture(t, context.Background(), nil, behavior{outputs: map[int]string{
			1: encodedCurrent}, phases: map[int]corev1.PodPhase{}, createErrors: map[int]error{
			2: errors.New(echoed)}, unappliedCreate: 2, logErrors: map[int]error{}})
		err := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false)
		if err == nil || !lease.QuarantineRequired || strings.Contains(err.Error(), echoed) ||
			strings.Contains(err.Error(), ownedKey) {
			t.Fatalf("unapplied Create response loss was not sanitized and quarantined: quarantine=%t err=%v",
				lease.QuarantineRequired, err)
		}
	})

	t.Run("command response loss remains quarantined but idempotent retry succeeds", func(t *testing.T) {
		const echoed = "command-response-echoed-whole-file"
		logErrors := map[int]error{2: errors.New(echoed)}
		fixture, lease, observed := newFixture(t, context.Background(), nil, behavior{outputs: map[int]string{
			1: encodedCurrent, 2: "", 3: encodedDesired, 4: ""}, phases: map[int]corev1.PodPhase{},
			createErrors: map[int]error{}, logErrors: logErrors})
		firstErr := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false)
		if firstErr == nil || !lease.QuarantineRequired || strings.Contains(firstErr.Error(), echoed) {
			t.Fatalf("command response loss was not sanitized and quarantined: quarantine=%t err=%v",
				lease.QuarantineRequired, firstErr)
		}
		delete(logErrors, 2)
		if err := fixture.mutateAuthorizedKeyWithHostProcess(context.Background(), lease, ownedKey, false); err != nil ||
			len(observed.scripts) != 4 {
			t.Fatalf("idempotent retry after applied command response loss failed: scripts=%d err=%v",
				len(observed.scripts), err)
		}
	})

	t.Run("cancellation after applied Create uses owned cleanup and quarantine", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		fixture, lease, observed := newFixture(t, ctx, cancel, behavior{outputs: map[int]string{},
			phases: map[int]corev1.PodPhase{1: corev1.PodPending}, createErrors: map[int]error{1: context.Canceled},
			logErrors: map[int]error{}, cancelOnCreate: 1})
		err := fixture.mutateAuthorizedKeyWithHostProcess(ctx, lease, ownedKey, false)
		if err == nil || !lease.QuarantineRequired || len(observed.deletes) != 1 ||
			strings.Contains(err.Error(), ownedKey) {
			t.Fatalf("canceled production method did not use owned cleanup and quarantine: deletes=%d quarantine=%t err=%v",
				len(observed.deletes), lease.QuarantineRequired, err)
		}
	})
}

func TestAuthorizedKeyCancellationQuarantinesWithoutExposingKey(t *testing.T) {
	owned := testHostPublicKey(t)
	lease := &byohHostLease{}
	fixture := &byohTestFixture{placeAuthorizedKeyForTest: func(context.Context, *byohHostLease, string) error {
		return context.Canceled
	}}
	err := fixture.placeAuthorizedKeyWithHostProcess(context.Background(), lease, owned)
	if err == nil || !lease.QuarantineRequired || strings.Contains(err.Error(), owned) {
		t.Fatalf("ambiguous key placement was not sanitized and quarantined: quarantine=%t err=%v",
			lease.QuarantineRequired, err)
	}
}
