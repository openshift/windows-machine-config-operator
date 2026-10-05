package winc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func testHostPublicKey(t *testing.T) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate unit-test host key: %v", err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("convert unit-test host key: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func mustParseTestHostKey(t *testing.T, value string) ssh.PublicKey {
	t.Helper()
	key, err := parseTrustedHostKey(value)
	if err != nil || key == nil {
		t.Fatalf("parse unit-test host key: key=%v err=%v", key, err)
	}
	return key
}

func TestBYOHPoolEntryParsing(t *testing.T) {
	trustedKey := testHostPublicKey(t)
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
ssh-host-public-key: %s
`, addressType, hostID, disposable, trustedKey)
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
		{name: "legacy absent trust parses", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), "ssh-host-public-key: "+trustedKey+"\n", "")},
		{name: "malformed host key", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), trustedKey, "ssh-ed25519 not-base64"), wantErr: true},
		{name: "commented host key", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), trustedKey, trustedKey+" producer-comment"), wantErr: true},
		{name: "multiple host keys", address: "192.0.2.10", raw: strings.ReplaceAll(valid("ip", "host-a", false), trustedKey, trustedKey+"\n"+trustedKey), wantErr: true},
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

func TestTrustedHostKeyRequiredBeforeClaimAndAliasesMustAgree(t *testing.T) {
	firstKey, secondKey := testHostPublicKey(t), testHostPublicKey(t)
	entry := func(addressType, key string) string {
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
ssh-address: ssh.example.test
ssh-host-public-key: %s
`, addressType, key)
	}
	t.Run("missing trust fails before input mutation", func(t *testing.T) {
		data := map[string]string{"192.0.2.10": entry("ip", "")}
		original := copyStringMap(data)
		if _, _, err := claimBYOHHosts(data, []byohHostRequest{{AddressType: byohAddressIP,
			RequireTrustedSSH: true}}, "lease-a", time.Now()); err == nil {
			t.Fatal("trusted SSH request accepted inventory without ssh-host-public-key")
		}
		if !reflect.DeepEqual(data, original) {
			t.Fatal("rejected trust validation mutated inventory")
		}
	})
	t.Run("physical-host aliases require byte-identical trust", func(t *testing.T) {
		data := map[string]string{
			"192.0.2.10":        entry("ip", firstKey),
			"host.example.test": entry("dns", secondKey),
		}
		if _, err := validateBYOHPoolInventoryWithTrust(data, true); err == nil {
			t.Fatal("different trusted keys for aliases of one physical host were accepted")
		}
	})
}

func TestClaimBYOHHostsUsesDistinctPhysicalHostsAndClaimsAliases(t *testing.T) {
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	trustedKey := testHostPublicKey(t)
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
ssh-host-public-key: %s
`, addressType, hostID, sshAddress, trustedKey)
	}
	data := map[string]string{
		"192.0.2.10":          entry("ip", "host-a", "ssh-a.example.test"),
		"host-a.example.test": entry("dns", "host-a", "ssh-a.example.test"),
		"host-b.example.test": entry("dns", "host-b", "ssh-b.example.test"),
	}
	updated, leases, err := claimBYOHHosts(data, []byohHostRequest{
		{AddressType: byohAddressIP, RequireTrustedSSH: true},
		{AddressType: byohAddressDNS, RequireTrustedSSH: true},
	}, "lease-a", now)
	if err != nil {
		t.Fatalf("claimBYOHHosts() returned an error: %v", err)
	}
	if len(leases) != 2 || leases[0].HostID == leases[1].HostID {
		t.Fatalf("claimBYOHHosts() did not choose two physical hosts: %#v", leases)
	}
	for _, lease := range leases {
		if lease.TrustedHostKey == nil || !bytes.Equal(lease.TrustedHostKey.Marshal(), mustParseTestHostKey(t, trustedKey).Marshal()) {
			t.Fatal("trusted producer host key was not retained in the physical-host lease")
		}
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
	trustedKey := testHostPublicKey(t)
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
ssh-host-public-key: ` + trustedKey + "\n"
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
	trustedKey := testHostPublicKey(t)
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
ssh-host-public-key: %s
`, addressType, trustedKey)
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
		[]byohHostRequest{{AddressType: byohAddressIP, RequireTrustedSSH: true}})
	if err != nil {
		t.Fatalf("updateBYOHPoolLeases() returned an error: %v", err)
	}
	if updates != 2 || len(leases) != 1 || len(leases[0].ClaimedAliases) != 2 {
		t.Fatalf("conflict retry or alias claim was incomplete: updates=%d leases=%#v", updates, leases)
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
ssh-host-public-key:
`, status, addressType, testID, allocatedAt, hostID)
}

func testAllocatedPoolLease(hostID, leaseID string, aliases ...string) *byohHostLease {
	return &byohHostLease{HostID: hostID, LeaseID: leaseID, Username: "test-user",
		SSHAddress: "ssh.example.test", Platform: "test", ResetPolicy: byohResetPolicy,
		AllocatedAt: "2026-10-04T00:00:00Z", ClaimedAliases: append([]string(nil), aliases...)}
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

func TestPoolClaimResponseLossRejectsWholeHostAliasDriftBeforeRegistration(t *testing.T) {
	tests := map[string]func(map[string]string){
		"late alias with different owner": func(data map[string]string) {
			late := data["192.0.2.10"]
			late = strings.Replace(late, "test-id: ", "test-id: other-lease", 1)
			data["192.0.2.12"] = late
		},
		"removed alias": func(data map[string]string) {
			delete(data, "192.0.2.11")
		},
		"changed immutable alias metadata": func(data map[string]string) {
			for alias, value := range data {
				data[alias] = strings.Replace(value, "platform: test", "platform: other", 1)
			}
		},
	}
	for name, drift := range tests {
		t.Run(name, func(t *testing.T) {
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
				Data: map[string]string{
					"192.0.2.10": testPoolEntry("ip", "host-a", "available", ""),
					"192.0.2.11": testPoolEntry("ip", "host-a", "available", ""),
				}}
			client := fake.NewSimpleClientset(cm)
			resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
			client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
				drift(updated.Data)
				if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				return true, nil, errors.New("simulated applied claim response loss")
			})
			registrationEntered := false
			leases, err := updateBYOHPoolLeases(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
				[]byohHostRequest{{AddressType: byohAddressIP}})
			if err == nil {
				registrationEntered = true
			}
			if len(leases) != 1 || !leases[0].QuarantineRequired || registrationEntered ||
				!strings.Contains(err.Error(), "reconcile ambiguous BYOH claim") {
				t.Fatalf("claim readback did not fail closed before registration: leases=%d quarantine=%t registration=%t err=%v",
					len(leases), len(leases) == 1 && leases[0].QuarantineRequired, registrationEntered, err)
			}
		})
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
	lease := testAllocatedPoolLease("host-a", "lease-a", "192.0.2.10")
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
	current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
		metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read pool state after rejected release: %v", err)
	}
	entry, err := parseBYOHPoolEntry("192.0.2.10", current.Data["192.0.2.10"])
	if err != nil {
		t.Fatalf("parse pool state after rejected release: %v", err)
	}
	if entry.Status == "available" {
		t.Fatal("failed reset became available")
	}
}

func TestPoolTransitionsRejectWholeHostAliasDriftWithoutMutation(t *testing.T) {
	base := map[string]string{
		"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-a"),
		"192.0.2.11": testPoolEntry("ip", "host-a", "allocated", "lease-a"),
	}
	lease := testAllocatedPoolLease("host-a", "lease-a", "192.0.2.10", "192.0.2.11")
	tests := map[string]func(map[string]string){
		"late alias": func(data map[string]string) {
			data["192.0.2.12"] = testPoolEntry("ip", "host-a", "allocated", "lease-a")
		},
		"removed alias": func(data map[string]string) { delete(data, "192.0.2.11") },
		"changed alias metadata": func(data map[string]string) {
			data["192.0.2.11"] = strings.Replace(data["192.0.2.11"], "username: test-user", "username: other", 1)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			data := copyStringMap(base)
			mutate(data)
			before := copyStringMap(data)
			updated, err := transitionBYOHHosts(data, []*byohHostLease{lease}, "releasing", false, time.Now())
			if err == nil || updated != nil {
				t.Fatalf("alias drift produced a partial transition: updated=%#v err=%v", updated, err)
			}
			if !reflect.DeepEqual(data, before) {
				t.Fatalf("rejected transition mutated its input: before=%#v after=%#v", before, data)
			}
		})
	}
}

func TestPoolReleaseReconcilesAppliedResponseLossForEveryAlias(t *testing.T) {
	aliases := []string{"192.0.2.10", "192.0.2.11"}
	data := map[string]string{}
	for _, alias := range aliases {
		data[alias] = testPoolEntry("ip", "host-a", "allocated", "lease-a")
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: data}
	client := fake.NewSimpleClientset(cm)
	resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	updates := 0
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates != 2 {
			return false, nil, nil
		}
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
			t.Fatalf("apply response-lost final release: %v", err)
		}
		return true, nil, errors.New("simulated applied final-update response loss")
	})
	lease := testAllocatedPoolLease("host-a", "lease-a", aliases...)
	lease.ResetVerified = true
	if err := releaseBYOHHosts(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease}); err != nil {
		t.Fatalf("exact applied final release was not reconciled: %v", err)
	}
	if !lease.CleanupDone {
		t.Fatal("cleanup was not settled after exact final-release readback")
	}
	live, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
		metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, alias := range aliases {
		entry, err := parseBYOHPoolEntry(alias, live.Data[alias])
		if err != nil || entry.Status != "available" || entry.TestID != "" || entry.AllocatedAt != "" {
			t.Fatalf("alias %s did not reach exact released state: entry=%#v err=%v", alias, entry, err)
		}
	}
}

func TestPoolReleasingResponseLossAppliedAndUnappliedStates(t *testing.T) {
	t.Run("applied releasing response loss continues to exact available settlement", func(t *testing.T) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
			Data: map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-a")}}
		client := fake.NewSimpleClientset(cm)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
		updates := 0
		client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			if updates != 1 {
				return false, nil, nil
			}
			updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
			if err := client.Tracker().Update(resource, updated, wmcoNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated applied releasing response loss")
		})
		lease := testAllocatedPoolLease("host-a", "lease-a", "192.0.2.10")
		lease.ResetVerified = true
		if err := releaseBYOHHosts(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}); err != nil {
			t.Fatalf("applied releasing response loss was not reconciled: %v", err)
		}
		live, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
			metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := parseBYOHPoolEntry("192.0.2.10", live.Data["192.0.2.10"])
		if err != nil || entry.Status != "available" || entry.TestID != "" || !lease.CleanupDone {
			t.Fatalf("applied releasing response loss did not settle exactly: entry=%#v cleanup=%t err=%v",
				entry, lease.CleanupDone, err)
		}
	})

	t.Run("unapplied releasing response loss never becomes available", func(t *testing.T) {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
			Data: map[string]string{"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-a")}}
		client := fake.NewSimpleClientset(cm)
		updates := 0
		client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
			updates++
			if updates == 1 {
				return true, nil, errors.New("simulated unapplied releasing response loss")
			}
			return false, nil, nil
		})
		lease := testAllocatedPoolLease("host-a", "lease-a", "192.0.2.10")
		lease.ResetVerified = true
		if err := releaseBYOHHosts(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
			[]*byohHostLease{lease}); err == nil {
			t.Fatal("unapplied releasing response loss was accepted")
		}
		live, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
			metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := parseBYOHPoolEntry("192.0.2.10", live.Data["192.0.2.10"])
		if err != nil || entry.Status != "unavailable" {
			t.Fatalf("unapplied releasing response loss did not quarantine: entry=%#v err=%v", entry, err)
		}
	})
}

func TestPoolReleaseFailsClosedOnUnappliedPartialDriftAndLaterOwner(t *testing.T) {
	lease := testAllocatedPoolLease("host-a", "lease-a", "192.0.2.10", "192.0.2.11")
	lease.ResetVerified = true
	unapplied := map[string]string{
		"192.0.2.10": testPoolEntry("ip", "host-a", "releasing", "lease-a"),
		"192.0.2.11": testPoolEntry("ip", "host-a", "releasing", "lease-a"),
	}
	if state, err := inspectBYOHTransition(unapplied, []*byohHostLease{lease}, "available", true); err != nil ||
		state != byohTransitionUnapplied {
		t.Fatalf("exact unapplied transition was not distinguished: state=%q err=%v", state, err)
	}
	for name, data := range map[string]map[string]string{
		"partial": {
			"192.0.2.10": testPoolEntry("ip", "host-a", "available", ""),
			"192.0.2.11": testPoolEntry("ip", "host-a", "releasing", "lease-a"),
		},
		"alias drift": {
			"192.0.2.10": testPoolEntry("ip", "host-a", "releasing", "lease-a"),
		},
		"later owner": {
			"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-b"),
			"192.0.2.11": testPoolEntry("ip", "host-a", "allocated", "lease-b"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectBYOHTransition(data, []*byohHostLease{lease}, "available", true); err == nil {
				t.Fatal("ambiguous final state was accepted")
			}
		})
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace},
		Data: map[string]string{
			"192.0.2.10": testPoolEntry("ip", "host-a", "allocated", "lease-a"),
			"192.0.2.11": testPoolEntry("ip", "host-a", "allocated", "lease-a"),
		}}
	client := fake.NewSimpleClientset(cm)
	resource := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	updates := 0
	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return false, nil, nil
		}
		later := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		for _, alias := range lease.ClaimedAliases {
			later.Data[alias] = testPoolEntry("ip", "host-a", "allocated", "lease-b")
		}
		if err := client.Tracker().Update(resource, later, wmcoNamespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, errors.New("response lost after a later claimant won")
	})
	err := releaseBYOHHosts(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace),
		[]*byohHostLease{lease})
	if err == nil || lease.CleanupDone || updates != 2 || !strings.Contains(err.Error(), "changed owner") {
		t.Fatalf("later owner was not preserved fail-closed: updates=%d cleanup=%t err=%v", updates, lease.CleanupDone, err)
	}
}
