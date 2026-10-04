package winc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestBYOHPoolEntryParsing(t *testing.T) {
	valid := func(addressType, hostID string, disposable bool) string {
		return fmt.Sprintf(`status: available
username: test-user
address-type: %s
platform: test
test-id:
allocated-at:
last-updated: 2026-10-04T00:00:00Z
host-id: %s
reset-policy: wmco-deconfigure-v1
disposable: %t
ssh-address: ssh.example.test
`, addressType, hostID, disposable)
	}
	tests := []struct {
		name, address, raw string
		wantErr            bool
	}{
		{name: "IP", address: "192.0.2.10", raw: valid("ip", "host-a", false)},
		{name: "DNS", address: "host.example.test", raw: valid("dns", "host-a", false)},
		{name: "DNS declared for IP key", address: "192.0.2.10", raw: valid("dns", "host-a", false), wantErr: true},
		{name: "missing host identity", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), "host-id: host-a\n", ""), wantErr: true},
		{name: "unknown status", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), "status: available", "status: idle"), wantErr: true},
		{name: "bad boolean", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), "disposable: false", "disposable: perhaps"), wantErr: true},
		{name: "bad timestamp", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), "2026-10-04T00:00:00Z", "yesterday"), wantErr: true},
		{name: "reusable without reset contract", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), byohResetPolicy, "none"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseBYOHPoolEntry(test.address, test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseBYOHPoolEntry() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestClaimBYOHHostsUsesDistinctPhysicalHostsAndClaimsAliases(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	entry := func(addressType, hostID, sshAddress string) string {
		return fmt.Sprintf(`status: available
username: test-user
address-type: %s
platform: test
test-id:
allocated-at:
last-updated: 2026-10-04T00:00:00Z
host-id: %s
reset-policy: wmco-deconfigure-v1
disposable: false
ssh-address: %s
`, addressType, hostID, sshAddress)
	}
	data := map[string]string{
		"192.0.2.10":          entry("ip", "host-a", "ssh-a.example.test"),
		"host-a.example.test": entry("dns", "host-a", "ssh-a.example.test"),
		"host-b.example.test": entry("dns", "host-b", "ssh-b.example.test"),
	}
	updated, leases, err := claimBYOHHosts(data, []byohHostRequest{
		{AddressType: byohAddressIP}, {AddressType: byohAddressDNS},
	}, "lease-a", now)
	if err != nil {
		t.Fatalf("claimBYOHHosts() returned an error: %v", err)
	}
	if len(leases) != 2 || leases[0].HostID == leases[1].HostID {
		t.Fatalf("claimBYOHHosts() did not choose two physical hosts: %#v", leases)
	}
	for _, address := range []string{"192.0.2.10", "host-a.example.test", "host-b.example.test"} {
		parsed, err := parseBYOHPoolEntry(address, updated[address])
		if err != nil {
			t.Fatalf("updated pool entry did not parse: %v", err)
		}
		if parsed.Status != "allocated" || parsed.TestID != "lease-a" {
			t.Fatalf("alias %q was not atomically claimed", address)
		}
	}
}

func TestClaimBYOHHostsInsufficientInventoryDoesNotMutateInput(t *testing.T) {
	raw := `status: available
username: test-user
address-type: ip
platform: test
test-id:
allocated-at:
last-updated: 2026-10-04T00:00:00Z
host-id: host-a
reset-policy: wmco-deconfigure-v1
disposable: false
ssh-address: ssh-a.example.test
`
	data := map[string]string{"192.0.2.10": raw}
	original := map[string]string{"192.0.2.10": raw}
	_, _, err := claimBYOHHosts(data, []byohHostRequest{{AddressType: byohAddressDNS}}, "lease-a", time.Now())
	if err == nil {
		t.Fatal("claimBYOHHosts() returned no error for insufficient exact-type inventory")
	}
	if !reflect.DeepEqual(data, original) {
		t.Fatal("claimBYOHHosts() mutated inventory after failed selection")
	}
}

func TestUpdateBYOHPoolLeasesRetriesConflictAndClaimsAliasGroup(t *testing.T) {
	raw := func(addressType string) string {
		return fmt.Sprintf(`status: available
username: test-user
address-type: %s
platform: test
test-id:
allocated-at:
last-updated: 2026-10-04T00:00:00Z
host-id: host-a
reset-policy: wmco-deconfigure-v1
disposable: false
ssh-address: ssh-a.example.test
`, addressType)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: map[string]string{"192.0.2.10": raw("ip"), "host-a.example.test": raw("dns")}}
	client := fake.NewSimpleClientset(cm)
	updates := 0
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"},
				byohPoolConfigMap, errors.New("test conflict"))
		}
		return false, nil, nil
	})
	leases, err := updateBYOHPoolLeases(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]byohHostRequest{{AddressType: byohAddressIP}})
	if err != nil {
		t.Fatalf("updateBYOHPoolLeases() returned an error: %v", err)
	}
	if updates != 2 || len(leases) != 1 || len(leases[0].ClaimedAliases) != 2 {
		t.Fatalf("conflict retry or alias claim was incomplete: updates=%d leases=%#v", updates, leases)
	}
}

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

func TestPublicKeyHashAndNodeReadiness(t *testing.T) {
	keyPath, err := generateTestPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(keyPath)
	hash, err := publicKeyHashFromPrivateKey(keyPath)
	if err != nil || len(hash) != 64 {
		t.Fatalf("publicKeyHashFromPrivateKey() = %q, %v", hash, err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "selected", UID: "uid-selected",
		Labels: map[string]string{"windowsmachineconfig.openshift.io/byoh": "true"}, Annotations: map[string]string{
			byohPublicKeyHashAnno: hash, byohVersionAnno: "v1", byohDesiredVersionAnno: "v1"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	nodes := fake.NewSimpleClientset(node).CoreV1().Nodes()
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	ready, err := targetLeaseNodesReadyWithKeyHash(context.Background(), nodes, []*byohHostLease{lease}, hash)
	if err != nil || !ready {
		t.Fatalf("targetLeaseNodesReadyWithKeyHash() = %t, %v", ready, err)
	}
	ready, err = targetLeaseNodesReadyWithKeyHash(context.Background(), nodes,
		[]*byohHostLease{lease, lease}, hash)
	if err != nil || ready {
		t.Fatalf("duplicate selection was accepted: ready=%t err=%v", ready, err)
	}
}

func encryptBYOHUsernameForTest(t *testing.T, plaintext string, key []byte) string {
	t.Helper()
	var buffer bytes.Buffer
	armored, err := armor.Encode(&buffer, "ENCRYPTED DATA", nil)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := openpgp.SymmetricallyEncrypt(armored, key, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := armored.Close(); err != nil {
		t.Fatal(err)
	}
	value := strings.TrimPrefix(buffer.String(), "-----BEGIN ENCRYPTED DATA-----")
	value = strings.TrimSuffix(value, "-----END ENCRYPTED DATA-----\n")
	return strings.ReplaceAll(strings.Trim(value, "\n"), "\n", "<wmcoMarker>")
}

func TestDecryptBYOHUsernameUsesCurrentKeyNotCiphertextEquality(t *testing.T) {
	key := []byte("non-secret deterministic unit fixture")
	first := encryptBYOHUsernameForTest(t, "fixture-user", key)
	second := encryptBYOHUsernameForTest(t, "fixture-user", key)
	if first == second {
		t.Fatal("test encryption unexpectedly produced equal ciphertext")
	}
	for _, ciphertext := range []string{first, second} {
		username, err := decryptBYOHUsername(ciphertext, key)
		if err != nil || username != "fixture-user" {
			t.Fatalf("decryptBYOHUsername() = %q, %v", username, err)
		}
	}
	if _, err := decryptBYOHUsername(first, []byte("different fixture key")); err == nil {
		t.Fatal("decryptBYOHUsername() accepted the wrong key")
	}
}

func testPoolEntry(addressType, hostID, status, testID string) string {
	allocatedAt := ""
	if testID != "" {
		allocatedAt = "2026-10-04T00:00:00Z"
	}
	return fmt.Sprintf(`status: %s
username: test-user
address-type: %s
platform: test
test-id: %s
allocated-at: %s
last-updated: 2026-10-04T00:00:00Z
host-id: %s
reset-policy: wmco-deconfigure-v1
disposable: false
ssh-address: ssh.example.test
`, status, addressType, testID, allocatedAt, hostID)
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

func TestPoolClaimResponseLossRetainsOwnership(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "available", "")}}
	client := fake.NewSimpleClientset(cm)
	updates := 0
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
			updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated applied response error")
		}
		return true, nil, errors.New("simulated quarantine failure")
	})
	leases, err := updateBYOHPoolLeases(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]byohHostRequest{{AddressType: byohAddressIP}})
	if err == nil || len(leases) != 1 {
		t.Fatalf("ambiguous claim did not return retained ownership: leases=%#v err=%v", leases, err)
	}
	if !strings.Contains(err.Error(), "claim BYOH physical hosts") {
		t.Fatalf("original claim error was not preserved: %v", err)
	}
}

func TestPoolPostWriteFailuresReturnLeaseAndQuarantineErrors(t *testing.T) {
	tests := []struct {
		name string
		get  func(*corev1.ConfigMap) (*corev1.ConfigMap, error)
	}{
		{name: "verification GET failure", get: func(*corev1.ConfigMap) (*corev1.ConfigMap, error) {
			return nil, errors.New("verification unavailable")
		}},
		{name: "malformed post-write data", get: func(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
			cm.Data["192.0.2.10"] = "malformed"
			return cm, nil
		}},
		{name: "ownership mismatch", get: func(cm *corev1.ConfigMap) (*corev1.ConfigMap, error) {
			cm.Data["192.0.2.10"] = testPoolEntry("ip", "host-a", "allocated", "other-lease")
			return cm, nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
				Data: map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "available", "")}}
			client := fake.NewSimpleClientset(cm)
			gets := 0
			client.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				gets++
				if gets == 1 {
					return false, nil, nil
				}
				live, err := client.Tracker().Get(schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
					wmcoNamespace, byohPoolConfigMap)
				if err != nil {
					return true, nil, err
				}
				returnValue, getErr := test.get(live.(*corev1.ConfigMap).DeepCopy())
				return true, returnValue, getErr
			})
			leases, err := updateBYOHPoolLeases(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
				[]byohHostRequest{{AddressType: byohAddressIP}})
			if err == nil || len(leases) != 1 {
				t.Fatalf("post-write failure lost cleanup ownership: leases=%#v err=%v", leases, err)
			}
			if !leases[0].QuarantineRequired {
				t.Fatal("ambiguous post-write ownership was not marked for quarantine")
			}
			if !strings.Contains(err.Error(), "quarantine") {
				t.Fatalf("quarantine failure was not aggregated: %v", err)
			}
		})
	}
}

func TestPoolTransitionsRejectWrongOwnershipAndUnverifiedRelease(t *testing.T) {
	lease := &byohHostLease{HostID: "host-a", LeaseID: "lease-a", ClaimedAliases: []string{"192.0.2.10"}}
	data := map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "other-lease")}
	if _, err := transitionBYOHHosts(data, []*byohHostLease{lease}, "releasing", false, time.Now()); err == nil {
		t.Fatal("wrong-owner transition was accepted")
	}
	client := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: byohPoolConfigMap, Namespace: wmcoNamespace}, Data: data})
	if err := releaseBYOHHosts(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}); err == nil {
		t.Fatal("host without reset proof was released")
	}
	current, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
		metav1.GetOptions{})
	entry, _ := parseBYOHPoolEntry("192.0.2.10", current.Data["192.0.2.10"])
	if entry.Status == "available" {
		t.Fatal("failed reset became available")
	}
}

func TestExactNodeDiscoveryAndDistinctIdentity(t *testing.T) {
	lease := &byohHostLease{Address: "host.example.test", AddressType: byohAddressDNS}
	lookup := func(string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("192.0.2.11")}, nil
	}
	nodeA := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	nodeB := readyBYOHNode("node-b", "uid-b", "192.0.2.11")
	if _, _, err := exactLeaseNode([]corev1.Node{*nodeA, *nodeB}, lease, lookup, ""); err == nil {
		t.Fatal("multi-A DNS matching multiple Nodes was accepted")
	}
	node, ready, err := exactLeaseNode([]corev1.Node{*nodeA}, lease, lookup, "")
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

func TestNodeUIDReplacementSequences(t *testing.T) {
	old := readyBYOHNode("node-a", "old-uid", "192.0.2.10")
	t.Run("stale old UID times out", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
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

func TestKeyHashReadinessRequiresExactConvergedUID(t *testing.T) {
	hash := strings.Repeat("a", 64)
	base := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	base.Annotations[byohPublicKeyHashAnno] = hash
	tests := []struct {
		name   string
		mutate func(*corev1.Node, *byohHostLease)
	}{
		{name: "wrong hash", mutate: func(node *corev1.Node, _ *byohHostLease) { node.Annotations[byohPublicKeyHashAnno] = "wrong" }},
		{name: "missing Ready", mutate: func(node *corev1.Node, _ *byohHostLease) { node.Status.Conditions = nil }},
		{name: "cordoned", mutate: func(node *corev1.Node, _ *byohHostLease) { node.Spec.Unschedulable = true }},
		{name: "missing version", mutate: func(node *corev1.Node, _ *byohHostLease) { delete(node.Annotations, byohVersionAnno) }},
		{name: "mismatched version", mutate: func(node *corev1.Node, _ *byohHostLease) { node.Annotations[byohVersionAnno] = "old" }},
		{name: "replaced UID", mutate: func(_ *corev1.Node, lease *byohHostLease) { lease.NodeUID = "different" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			node := base.DeepCopy()
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
			test.mutate(node, lease)
			ready, err := targetLeaseNodesReadyWithKeyHash(context.Background(),
				fake.NewSimpleClientset(node).CoreV1().Nodes(), []*byohHostLease{lease}, hash)
			if err != nil || ready {
				t.Fatalf("non-converged node accepted: ready=%t err=%v", ready, err)
			}
		})
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("node API unavailable")
	})
	if _, err := targetLeaseNodesReadyWithKeyHash(context.Background(), client.CoreV1().Nodes(),
		[]*byohHostLease{{NodeName: "node-a", NodeUID: "uid-a"}}, hash); err == nil {
		t.Fatal("Node API error was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := targetLeaseNodesReadyWithKeyHash(ctx, client.CoreV1().Nodes(),
		[]*byohHostLease{{NodeName: "node-a", NodeUID: "uid-a"}}, hash); err == nil {
		t.Fatal("canceled context was ignored")
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

func TestManagedResetContractScripts(t *testing.T) {
	for _, required := range []string{`C:\Temp`, `C:\k\etc\kubernetes\manifests`, `C:\k`} {
		if !containsString(byohManagedDirectories, required) {
			t.Fatalf("required managed directory %q is missing", required)
		}
	}
	if containsString(byohManagedDirectories, `C:\k\pod-manifests`) {
		t.Fatal("obsolete pod-manifests path remains in reset contract")
	}
	if !strings.Contains(managedServicesStoppedScript(), "$svc.Status -ne 'Stopped'") {
		t.Fatal("service predicate does not reject non-Stopped transitional states")
	}
	if !strings.Contains(managedDirectoriesRemovedScript(), "Test-Path -LiteralPath") {
		t.Fatal("directory predicate does not check every literal managed path")
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
		client := fake.NewSimpleClientset()
		lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", LeaseID: "lease-a"}
		state := &byohInstancesState{}
		if err := registerBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}, state); err != nil {
			t.Fatal(err)
		}
		cm, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
			metav1.GetOptions{})
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
		cm, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap,
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

func TestCurrentBYOHWorkloadPlacementUsesOwnersReadinessAndUIDs(t *testing.T) {
	controller := true
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	readyPod := func(name string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
			UID: "rs-current", Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
				Status: corev1.ConditionTrue}}}}
	}
	pods := []corev1.Pod{}
	for i := 0; i < 5; i++ {
		pods = append(pods, readyPod(fmt.Sprintf("current-%d", i)))
	}
	terminating := readyPod("terminating-old")
	deletion := metav1.Now()
	terminating.DeletionTimestamp = &deletion
	pods = append(pods, terminating, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign",
		OwnerReferences: []metav1.OwnerReference{{UID: "rs-old", Controller: &controller}}}})
	ready, err := currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(node).CoreV1().Nodes(),
		pods, "rs-current", []*byohHostLease{lease}, 5)
	if err != nil || !ready {
		t.Fatalf("five current Ready owned Pods were not accepted: ready=%t err=%v", ready, err)
	}
	replaced := node.DeepCopy()
	replaced.UID = "uid-replaced"
	ready, err = currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(replaced).CoreV1().Nodes(),
		pods, "rs-current", []*byohHostLease{lease}, 5)
	if err == nil || ready {
		t.Fatal("Pods on a replaced Node UID were accepted")
	}
}

func TestCanceledContextsAndSanitizedSSHClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease := &byohHostLease{Address: "192.0.2.10", AddressType: byohAddressIP}
	if err := waitForLeaseNodeWithOptions(ctx, fake.NewSimpleClientset().CoreV1().Nodes(), lease,
		net.LookupIP, "", time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("canceled discovery context was ignored")
	}
	classified := sshErrorClass(ctx, context.Canceled)
	if classified != "timeout" || strings.Contains(classified, lease.Address) {
		t.Fatalf("SSH cancellation classification was not sanitized: %q", classified)
	}
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

func TestMalformedCiphertextAndRejectedOldNodeUID(t *testing.T) {
	if _, err := decryptBYOHUsername("not armored ciphertext", []byte("fixture key")); err == nil {
		t.Fatal("malformed encrypted username was accepted")
	}
	lease := &byohHostLease{Address: "192.0.2.10", AddressType: byohAddressIP}
	old := readyBYOHNode("node-a", "old-uid", lease.Address)
	node, ready, err := exactLeaseNode([]corev1.Node{*old}, lease, net.LookupIP, old.UID)
	if err != nil || ready || node != nil {
		t.Fatalf("old Node UID was accepted as its own replacement: node=%#v ready=%t err=%v", node, ready, err)
	}
}

func TestCleanupOrdersIDMSDeletionBeforeReleaseAndAggregatesFailures(t *testing.T) {
	lease := &byohHostLease{HostID: "host-a", LeaseID: "lease-a", ClaimedAliases: []string{"192.0.2.10"},
		ResetVerified: true}
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
	lease := &byohHostLease{Address: "192.0.2.10", Username: "test-user", HostID: "host-a",
		LeaseID: "lease-a", ClaimedAliases: []string{"192.0.2.10"}, NodeName: "node-a", NodeUID: "node-uid",
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
		owned, _ := client.Tracker().Get(resource, wmcoNamespace, byohInstancesConfigMap)
		replacement := owned.(*corev1.ConfigMap).DeepCopy()
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

func TestKeyUsernameConvergenceUsesOneRetainedNodeRead(t *testing.T) {
	key := []byte("non-secret convergence fixture")
	hash := strings.Repeat("a", 64)
	ciphertext := encryptBYOHUsernameForTest(t, "test-user", key)
	for _, test := range []struct {
		name          string
		previous      string
		requireChange bool
	}{
		{name: "replacement key", previous: "old-ciphertext", requireChange: true},
		{name: "restored key remains decryptability based", requireChange: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			retained := readyBYOHNode("node-a", "retained-uid", "192.0.2.10")
			retained.Annotations[byohPublicKeyHashAnno] = hash
			retained.Annotations[byohUsernameAnno] = "not encrypted"
			replacement := retained.DeepCopy()
			replacement.UID = "replacement-uid"
			replacement.Annotations[byohUsernameAnno] = ciphertext
			client := fake.NewSimpleClientset()
			gets := 0
			client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
				gets++
				if gets == 1 {
					return true, retained.DeepCopy(), nil
				}
				return true, replacement.DeepCopy(), nil
			})
			lease := &byohHostLease{NodeName: retained.Name, NodeUID: retained.UID, Username: "test-user"}
			ready, err := leaseNodeKeyAndUsernameConverged(context.Background(), client.CoreV1().Nodes(), lease,
				hash, key, test.previous, test.requireChange)
			if err != nil || ready || gets != 1 {
				t.Fatalf("convergence mixed Node identities: ready=%t gets=%d err=%v", ready, gets, err)
			}
		})
	}
}

func TestPrivateKeySecretUIDGuardsAndAmbiguousUpdateReconciliation(t *testing.T) {
	for _, phase := range []string{"replacement before rotation", "replacement before restore"} {
		t.Run(phase, func(t *testing.T) {
			replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
				Namespace: wmcoNamespace, UID: "replacement-uid", ResourceVersion: "7"},
				Data: map[string][]byte{byohPrivateKeyDataKey: []byte("replacement key")}}
			client := fake.NewSimpleClientset(replacement)
			err := updatePrivateKeySecret(context.Background(), client.CoreV1().Secrets(wmcoNamespace),
				"original-uid", []byte("test key"))
			if err == nil {
				t.Fatal("same-name replacement Secret was overwritten")
			}
			current, getErr := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(),
				byohPrivateKeySecret, metav1.GetOptions{})
			if getErr != nil || current.UID != replacement.UID ||
				!bytes.Equal(current.Data[byohPrivateKeyDataKey], replacement.Data[byohPrivateKeyDataKey]) {
				t.Fatalf("replacement Secret was changed: current=%#v err=%v", current, getErr)
			}
		})
	}

	t.Run("applied update response loss is reconciled by UID and key", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
			Namespace: wmcoNamespace, UID: "original-uid", ResourceVersion: "3",
			Labels: map[string]string{"example.test/preserve": "true"}},
			Data: map[string][]byte{byohPrivateKeyDataKey: []byte("old key"), "unrelated": []byte("preserve")}}
		client := fake.NewSimpleClientset(secret)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
		client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
			if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated update response loss")
		})
		newKey := []byte("new key")
		if err := updatePrivateKeySecret(context.Background(), client.CoreV1().Secrets(wmcoNamespace),
			secret.UID, newKey); err != nil {
			t.Fatal(err)
		}
		current, err := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(),
			byohPrivateKeySecret, metav1.GetOptions{})
		if err != nil || current.UID != secret.UID || !bytes.Equal(current.Data[byohPrivateKeyDataKey], newKey) ||
			string(current.Data["unrelated"]) != "preserve" || current.Labels["example.test/preserve"] != "true" {
			t.Fatalf("ambiguous update reconciliation lost identity or unrelated state: current=%#v err=%v",
				current, err)
		}
	})
}

type closeTrackingConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

type blockingCommandSession struct {
	transportClosed chan struct{}
	workerExited    chan struct{}
	once            sync.Once
}

func (s *blockingCommandSession) CombinedOutput(string) ([]byte, error) {
	<-s.transportClosed
	s.once.Do(func() { close(s.workerExited) })
	return nil, errors.New("transport closed")
}

func (s *blockingCommandSession) Close() error {
	select {
	case <-s.workerExited:
		return nil
	case <-time.After(200 * time.Millisecond):
		return errors.New("command worker was not joined before session close")
	}
}

func (c *closeTrackingConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestSSHHandshakeAndSessionCreationHonorCancellation(t *testing.T) {
	t.Run("stalled handshake closes raw connection", func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		tracked := &closeTrackingConn{Conn: clientSide, closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := newWindowsSSHClientFromConnection(ctx, tracked, "fixture:22", &ssh.ClientConfig{
				User: "fixture", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: time.Second})
			result <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("stalled handshake cancellation was not classified: %v", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("stalled SSH handshake did not return promptly")
		}
		select {
		case <-tracked.closed:
		default:
			t.Fatal("stalled SSH handshake did not close the raw connection")
		}
	})

	t.Run("stalled session open closes client and joins opener", func(t *testing.T) {
		closed := make(chan struct{})
		openerExited := make(chan struct{})
		var once sync.Once
		client := &windowsSSHClient{
			newSession: func() (sshCommandSession, error) {
				<-closed
				close(openerExited)
				return nil, errors.New("client closed")
			},
			closeFn: func() error {
				once.Do(func() { close(closed) })
				return nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := runPowerShellOverSSH(ctx, client, "Write-Output 'never runs'")
		if err == nil || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("stalled session cancellation was not prompt: elapsed=%v err=%v", time.Since(started), err)
		}
		select {
		case <-openerExited:
		default:
			t.Fatal("session opener goroutine remained blocked after cancellation")
		}
	})

	t.Run("stalled command closes transport and joins worker", func(t *testing.T) {
		transportClosed := make(chan struct{})
		workerExited := make(chan struct{})
		var once sync.Once
		session := &blockingCommandSession{transportClosed: transportClosed, workerExited: workerExited}
		client := &windowsSSHClient{
			newSession: func() (sshCommandSession, error) { return session, nil },
			closeFn: func() error {
				once.Do(func() { close(transportClosed) })
				return nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := runPowerShellOverSSH(ctx, client, "Write-Output 'blocked'")
		if err == nil || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("stalled command cancellation was not prompt: elapsed=%v err=%v", time.Since(started), err)
		}
		select {
		case <-transportClosed:
		default:
			t.Fatal("stalled command cancellation did not close the transport")
		}
		select {
		case <-workerExited:
		default:
			t.Fatal("command worker remained blocked after cancellation returned")
		}
	})
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
	err := runBYOHCleanupOperations([]byohCleanupOperation{
		{name: "restore", ctx: mainCtx, run: operation("restore", true)},
		{name: "deconfigure", ctx: mainCtx, run: operation("deconfigure", false)},
		{name: "IDMS", ctx: fallbackCtx, run: operation("IDMS", true)},
		{name: "quarantine", ctx: fallbackCtx, run: operation("quarantine", false)},
	})
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
			_, _ = response.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"wmco-operator","namespace":"` +
				wmcoNamespace + `","labels":{"name":"windows-machine-config-operator"}}}]}`))
			return
		}
		if strings.HasSuffix(request.URL.Path, "/pods/wmco-operator/log") {
			<-request.Context().Done()
			once.Do(func() { close(logRequestCanceled) })
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
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
	case <-time.After(250 * time.Millisecond):
		t.Fatal("server-side Pod log request context was not canceled")
	}
}

func TestCurrentReplicaSetAndReadyOldPodsAreExcluded(t *testing.T) {
	controller := true
	template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "win-webserver"}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "win-webserver", UID: "deployment-uid",
		Annotations: map[string]string{"deployment.kubernetes.io/revision": "2"}},
		Spec: appsv1.DeploymentSpec{Template: template}}
	currentTemplate := template.DeepCopy()
	currentTemplate.Labels["pod-template-hash"] = "current"
	replicaSets := []appsv1.ReplicaSet{
		{ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "rs-old",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "1"},
			OwnerReferences: []metav1.OwnerReference{{UID: deployment.UID, Controller: &controller}}},
			Spec: appsv1.ReplicaSetSpec{Template: *currentTemplate}},
		{ObjectMeta: metav1.ObjectMeta{Name: "current", UID: "rs-current",
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": "2"},
			OwnerReferences: []metav1.OwnerReference{{UID: deployment.UID, Controller: &controller}}},
			Spec: appsv1.ReplicaSetSpec{Template: *currentTemplate}},
	}
	uid, found := currentDeploymentReplicaSetUID(deployment, replicaSets)
	if !found || uid != "rs-current" {
		t.Fatalf("current ReplicaSet was not uniquely verified: uid=%q found=%t", uid, found)
	}
	node := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID}
	readyPod := func(name string, owner types.UID) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{
			UID: owner, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady,
				Status: corev1.ConditionTrue}}}}
	}
	pods := []corev1.Pod{readyPod("current-0", uid), readyPod("current-1", uid),
		readyPod("current-2", uid), readyPod("current-3", uid), readyPod("ready-old", "rs-old")}
	ready, err := currentBYOHWorkloadReady(context.Background(), fake.NewSimpleClientset(node).CoreV1().Nodes(),
		pods, uid, []*byohHostLease{lease}, 5)
	if err != nil || ready {
		t.Fatalf("Ready old-ReplicaSet Pod satisfied current replica count: ready=%t err=%v", ready, err)
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
