package winc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func legacyBYOHRecord(disposable bool) string {
	return fmt.Sprintf("status: available\nusername: test-user\naddress-type: ip\nplatform: test\ntest-id:\nallocated-at:\nlast-updated: 2026-10-30T00:00:00Z\ndisposable: %t\n", disposable)
}

func TestBYOHProducerCompatibility(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"legacy", legacyBYOHRecord(false), true},
		{"disposable", legacyBYOHRecord(true), true},
		{"alternate-update", strings.ReplaceAll(legacyBYOHRecord(false), "last-updated:", "lastupdated:"), true},
		{"rich", legacyBYOHRecord(false) + "host-id: supplied\nreset-policy: wmco-deconfigure-v1\nssh-address: 192.0.2.1\nssh-host-public-key: " + testHostPublicKey(t) + "\n", true},
		{"missing-disposable", strings.ReplaceAll(legacyBYOHRecord(false), "disposable: false\n", ""), false},
		{"malformed", legacyBYOHRecord(false) + "unsupported: value\n", false},
		{"duplicate", legacyBYOHRecord(false) + "username: other\n", false},
		{"bad-timestamp", strings.ReplaceAll(legacyBYOHRecord(false), "2026-10-30T00:00:00Z", "not-time"), false},
		{"ip-not-dns", strings.ReplaceAll(legacyBYOHRecord(false), "address-type: ip", "address-type: dns"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := decodeBYOHProducerRecord("192.0.2.1", test.raw)
			if (err == nil) != test.valid {
				t.Fatalf("decode error=%v valid=%t", err, test.valid)
			}
			if err != nil {
				return
			}
			reserved := reserveBYOHProducerRecord(record, "run", time.Date(2026, 10, 30, 1, 0, 0, 0, time.UTC))
			after, err := decodeBYOHProducerRecord("192.0.2.1", reserved)
			if err != nil {
				t.Fatal(err)
			}
			if after.Fields["status"] != "allocated" || after.Fields["test-id"] != "run" {
				t.Fatal("producer reservation was not retained")
			}
			for name, value := range record.Fields {
				if name == "status" || name == "test-id" || name == "allocated-at" || name == "last-updated" || name == "lastupdated" {
					continue
				}
				if after.Fields[name] != value {
					t.Fatalf("immutable producer field %s changed", name)
				}
			}
			if _, provided := record.Fields["host-id"]; !provided && strings.Contains(reserved, "host-id:") {
				t.Fatal("invented producer identity")
			}
		})
	}
}

func TestBYOHPreflightInvalidInventoryNoMutation(t *testing.T) {
	for _, raw := range []string{"malformed", strings.ReplaceAll(legacyBYOHRecord(false), "disposable: false\n", "")} {
		client := fake.NewSimpleClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace, UID: "pool", ResourceVersion: "1"}, Data: map[string]string{"192.0.2.1": raw}})
		fixture := &byohTestFixture{coreClient: client.CoreV1()}
		if _, err := fixture.preflightBYOH(context.Background(), "run"); err == nil {
			t.Fatal("invalid inventory accepted")
		}
		for _, action := range client.Actions() {
			if action.GetVerb() != "get" {
				t.Fatalf("mutation before valid inventory: %s", action.GetVerb())
			}
		}
	}
}

func TestBYOHPreflightPublicFacts(t *testing.T) {
	facts := byohHostFacts{UUID: "18d74027-cdc4-4347-8b32-81dd83a83001", GUID: "18d74027-cdc4-4347-8b32-81dd83a83002", Addresses: []string{"192.0.2.1"}, DNS: "host.example.test", PublicKey: testHostPublicKey(t)}
	if _, err := decodeBYOHHostFacts(mustBYOHJSON(facts), true); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*byohHostFacts)
	}{
		{"missing-key", func(value *byohHostFacts) { value.PublicKey = "" }},
		{"missing-guid", func(value *byohHostFacts) { value.GUID = "" }},
		{"bad-interface", func(value *byohHostFacts) { value.Addresses = []string{"not-an-ip"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := facts
			test.mutate(&copy)
			if _, err := decodeBYOHHostFacts(mustBYOHJSON(copy), true); err == nil {
				t.Fatal("incomplete facts accepted")
			}
		})
	}
	if _, err := decodeBYOHHostFacts(mustBYOHJSON(facts)+" {}", true); err == nil {
		t.Fatal("trailing output accepted")
	}
	if _, err := decodeBYOHHostFacts(`{"failure":"hostkey-reparse"}`, true); err == nil || !strings.Contains(err.Error(), "hostkey-reparse") {
		t.Fatal("capability failure class was discarded")
	}
	if _, err := decodeBYOHHostFacts(`{"failure":"untrusted-echo"}`, true); err == nil || strings.Contains(err.Error(), "untrusted-echo") {
		t.Fatal("untrusted failure content was exposed")
	}
	if !sameBYOHPhysicalHost(facts, facts) {
		t.Fatal("same host rejected")
	}
	other := facts
	other.GUID = "18d74027-cdc4-4347-8b32-81dd83a83003"
	if sameBYOHPhysicalHost(facts, other) {
		t.Fatal("different machine accepted")
	}
	if _, err := resolveBYOHAddress(context.Background(), "host.example.test", func(context.Context, string) ([]net.IP, error) { return nil, errors.New("DNS") }); err == nil {
		t.Fatal("failed DNS accepted")
	}
}

func TestBYOHPreflightBootstrapUsesOwnedPublicProbeWithoutSSH(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFailure=%t", cleanupFailure), func(t *testing.T) {
			node := readyBYOHNode("node-a", "node-uid", "192.0.2.1")
			client := fake.NewSimpleClientset(node)
			client.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				pod := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
				script := pod.Spec.Containers[0].Command[len(pod.Spec.Containers[0].Command)-1]
				if script != byohPublicHostKeyScript || !strings.Contains(script, ".pub'") || !strings.Contains(script, "trailing-nonpublic-data") || strings.Contains(script, "ssh-keyscan") {
					t.Fatal("wrong bootstrap/public-key policy")
				}
				pod.UID = "probe"
				pod.ResourceVersion = "2"
				pod.Status.Phase = corev1.PodSucceeded
				if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				return true, pod, nil
			})
			deleted := false
			client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				options := action.(clienttesting.DeleteAction).GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "probe" || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != "2" {
					t.Fatal("probe cleanup lost UID/RV guards")
				}
				deleted = true
				if cleanupFailure {
					return true, nil, errors.New("API cleanup failed")
				}
				return false, nil, nil
			})
			facts := byohHostFacts{UUID: "18d74027-cdc4-4347-8b32-81dd83a83001", GUID: "18d74027-cdc4-4347-8b32-81dd83a83002", Addresses: []string{"192.0.2.1"}, DNS: "host.example.test", PublicKey: testHostPublicKey(t)}
			fixture := &byohTestFixture{coreClient: client.CoreV1(), hostProcessLogsForTest: func(context.Context, string, string, types.UID) ([]byte, error) {
				return []byte(mustBYOHJSON(facts)), nil
			}}
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, LeaseID: "run-probe", HostID: string(node.UID)}
			fixture.preparationDiagnostic = &byohPreparationDiagnostic{Host: 1, Phase: "validation", PodCreate: "not-attempted", PodDelete: "not-attempted"}
			actual, err := fixture.probeBYOHHost(context.Background(), lease)
			if (err != nil) != cleanupFailure || !deleted {
				t.Fatalf("bootstrap err=%v deleted=%t", err, deleted)
			}
			if err == nil && actual.PublicKey != facts.PublicKey {
				t.Fatal("captured public key was not retained")
			}
			expectedDelete := "confirmed"
			if cleanupFailure {
				expectedDelete = "unknown"
			}
			if fixture.preparationDiagnostic.PodCreate != "confirmed" || fixture.preparationDiagnostic.PodDelete != expectedDelete {
				t.Fatalf("owned API side effects lost: %+v", fixture.preparationDiagnostic)
			}
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "secrets" {
					t.Fatal("bootstrap read a Secret or required SSH trust")
				}
			}
		})
	}
}

func TestBYOHSessionInitialAdoptionAndWithdrawal(t *testing.T) {
	for _, mode := range []string{"success", "lost-response", "foreign-owner", "replacement", "username-drift", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			original := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap, Namespace: wmcoNamespace, UID: "instances", ResourceVersion: "1", Labels: map[string]string{"unrelated": "keep"}, Annotations: map[string]string{"unrelated": "bytes"}}, Data: map[string]string{"192.0.2.1": "username=test-user", "host.example.test": "username=test-user"}}
			snapshot := original.DeepCopy()
			if mode == "foreign-owner" {
				original.Annotations[registrationOwnershipAnnotation("192.0.2.1")] = "foreign"
			}
			if mode == "replacement" {
				original.UID = "replacement"
			}
			if mode == "username-drift" {
				original.Data["192.0.2.1"] = "username=other"
			}
			client := fake.NewSimpleClientset(original)
			updates := 0
			client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
				updates++
				object := action.(clienttesting.UpdateAction).GetObject().(*corev1.ConfigMap)
				if mode == "conflict" && updates == 1 {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, object.Name, errors.New("conflict"))
				}
				if mode == "lost-response" && updates == 1 {
					if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), object, wmcoNamespace); err != nil {
						t.Fatal(err)
					}
					return true, nil, errors.New("response lost")
				}
				return false, nil, nil
			})
			leases := []*byohHostLease{{Address: "192.0.2.1", Username: "test-user", LeaseID: "run"}, {Address: "host.example.test", Username: "test-user", LeaseID: "run"}}
			state := &byohInstancesState{observed: true, initiallyExisted: true, initialUID: snapshot.UID}
			err := adoptBYOHInitialRegistrations(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace), snapshot, leases, state)
			if mode == "foreign-owner" || mode == "replacement" || mode == "username-drift" {
				if err == nil || updates != 0 {
					t.Fatalf("unsafe adoption: err=%v updates=%d", err, updates)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !leases[0].RegistrationOwned || state.ownedUID != snapshot.UID {
				t.Fatal("ownership not proven")
			}
			if err := unregisterBYOHInstances(context.Background(), client.CoreV1().ConfigMaps(wmcoNamespace), leases, state, true, nil, nil); err != nil {
				t.Fatal(err)
			}
			current, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if current.UID != snapshot.UID || len(current.Data) != 0 || !reflect.DeepEqual(current.Labels, snapshot.Labels) || !reflect.DeepEqual(current.Annotations, snapshot.Annotations) {
				t.Fatal("empty original registration ConfigMap or metadata was not preserved")
			}
		})
	}
}

func TestBYOHSessionPrivateSettlementRetainsReservation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
			record, _ := decodeBYOHProducerRecord("192.0.2.1", legacyBYOHRecord(false))
			reservation := map[string]string{"192.0.2.1": reserveBYOHProducerRecord(record, "run", time.Now())}
			session := &byohRunSession{Scope: scope, Nonce: "nonce", Phase: "Prepared", ActiveCase: "case", UID: "session", Name: byohSessionName(scope), Reservations: reservation, SecretUID: "secret"}
			ledger := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}, Data: map[string]string{"192.0.2.1": serializeBYOHPoolEntry(byohPoolEntry{Status: "available", Username: "test-user", AddressType: byohAddressIP, Platform: "test", LastUpdated: time.Now().UTC().Format(time.RFC3339), HostID: "physical", ResetPolicy: byohResetPolicy, SSHAddress: "192.0.2.1"})}}
			pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace, UID: "pool"}, Data: copyStringMap(reservation)}
			client := fake.NewSimpleClientset(ledger, pool)
			fixture := &byohTestFixture{coreClient: client.CoreV1(), session: session, caseID: "case"}
			var cleanupErr error
			if failed {
				cleanupErr = errors.New("uncertain host reset")
			}
			err := fixture.settleBYOHCase(context.Background(), cleanupErr)
			if (err != nil) != failed {
				t.Fatalf("unexpected settlement result: %v", err)
			}
			current, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), session.Name, metav1.GetOptions{})
			owner, err := decodeBYOHSession(current, scope)
			if err != nil {
				t.Fatal(err)
			}
			if owner.ActiveCase != "" || (failed && owner.Phase != "Failed") {
				t.Fatal("case did not settle or quarantine")
			}
			producer, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
			if !reflect.DeepEqual(producer, pool) {
				t.Fatal("case settlement changed raw run reservation")
			}
			if _, err := decodeBYOHSession(current, byohRunScope{Run: "foreign", PoolUID: "pool", ClusterUID: "cluster"}); err == nil {
				t.Fatal("foreign run adopted retained proof")
			}
		})
	}
}

func TestBYOHSessionOptOut(t *testing.T) {
	t.Setenv(byohPoolRequiredEnv, "")
	if err := InitializeBYOHSession(); err != nil {
		t.Fatal(err)
	}
}

func TestBYOHSessionSecretUpdatesRejectForeignKey(t *testing.T) {
	fixture, client, _ := byohRotationGuardFixture(t)
	secret, err := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.secretSnapshot = secret.DeepCopy()
	fixture.originalKeyHash, err = publicKeyHash(secret.Data[byohPrivateKeyDataKey])
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(replacement)
	if err != nil {
		t.Fatal(err)
	}
	fixture.replacementAuthorizedKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if err := fixture.updateBYOHPrivateKey(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := fixture.updateBYOHPrivateKey(context.Background(), fixture.secretSnapshot.Data[byohPrivateKeyDataKey]); err != nil {
		t.Fatal(err)
	}
	foreign, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	secret.Data[byohPrivateKeyDataKey] = foreign
	if _, err := client.CoreV1().Secrets(wmcoNamespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.ClearActions()
	if err := fixture.updateBYOHPrivateKey(context.Background(), replacement); err == nil {
		t.Fatal("foreign same-UID key was overwritten")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			t.Fatal("foreign key triggered an update")
		}
	}
}

func byohPreflightFixture(t *testing.T) (*byohTestFixture, *fake.Clientset, map[string]byohHostFacts) {
	t.Helper()
	key, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := publicKeyHash(key)
	if err != nil {
		t.Fatal(err)
	}
	pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, Namespace: wmcoNamespace, UID: "pool", ResourceVersion: "1"}, Data: map[string]string{}}
	instances := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohInstancesConfigMap, Namespace: wmcoNamespace, UID: "instances", ResourceVersion: "1", Annotations: map[string]string{"unrelated": "preserve"}}, Data: map[string]string{}}
	objects := []runtime.Object{pool, instances, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret, Namespace: wmcoNamespace, UID: "secret"}, Data: map[string][]byte{byohPrivateKeyDataKey: key}}}
	facts := map[string]byohHostFacts{}
	for index := 1; index <= 3; index++ {
		address := fmt.Sprintf("192.0.2.%d", index)
		name := fmt.Sprintf("host%d.example.test", index)
		pool.Data[address] = legacyBYOHRecord(index == 3)
		instances.Data[address] = "username=test-user"
		instances.Data[name] = "username=test-user"
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(fmt.Sprintf("node%d", index)), ResourceVersion: "1", Labels: map[string]string{"kubernetes.io/os": "windows", "windowsmachineconfig.openshift.io/byoh": "true"}, Annotations: map[string]string{byohVersionAnno: "expected", byohDesiredVersionAnno: "expected", byohPublicKeyHashAnno: hash, byohUsernameAnno: encryptWMCOUsernameForTest(t, "test-user", key)}}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}, {Type: corev1.NodeInternalDNS, Address: name}}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		objects = append(objects, node)
		facts[name] = byohHostFacts{UUID: fmt.Sprintf("18d74027-cdc4-4347-8b32-81dd83a8300%d", index), GUID: fmt.Sprintf("18d74027-cdc4-4347-8b32-81dd83a8310%d", index), Addresses: []string{address}, DNS: name, PublicKey: testHostPublicKey(t)}
	}
	client := fake.NewSimpleClientset(objects...)
	fixture := &byohTestFixture{ctx: context.Background(), coreClient: client.CoreV1(), dynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}: "MachineList"})}
	fixture.preflightProbeForTest = func(_ context.Context, lease *byohHostLease) (byohHostFacts, error) {
		return facts[lease.NodeName], nil
	}
	fixture.preflightLookupForTest = func(_ context.Context, name string) ([]net.IP, error) {
		for _, host := range facts {
			if name == host.DNS {
				return []net.IP{net.ParseIP(host.Addresses[0])}, nil
			}
		}
		return nil, errors.New("unknown DNS")
	}
	fixture.sshClientForTest = func(_ context.Context, lease *byohHostLease) (*windowsSSHClient, error) {
		var selected byohHostFacts
		for _, host := range facts {
			if containsBYOHAddress(host.Addresses, lease.SSHAddress) {
				selected = host
				break
			}
		}
		return &windowsSSHClient{newSession: func() (sshCommandSession, error) {
			return &byohFixtureSSHSession{facts: selected}, nil
		}}, nil
	}
	return fixture, client, facts
}

type byohFixtureSSHSession struct{ facts byohHostFacts }

func (session *byohFixtureSSHSession) CombinedOutput(command string) ([]byte, error) {
	switch command {
	case "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + encodePowerShell(byohIdentityScript):
		return []byte(mustBYOHJSON(session.facts)), nil
	case "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + encodePowerShell(managedServicesStoppedScript()):
		return []byte(managedServicesStoppedProof), nil
	case "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " + encodePowerShell(managedDirectoriesRemovedScript()):
		return []byte(managedDirectoriesAbsentProof), nil
	default:
		return nil, fmt.Errorf("unexpected test SSH command")
	}
}

func (session *byohFixtureSSHSession) Close() error { return nil }

func TestBYOHPreflightAllHostsBeforeAnyAdoption(t *testing.T) {
	for _, failure := range []string{"none", "matching-key", "wrong-key", "disposable-disagreement", "foreign-workload", "api", "probe-admission", "probe-cleanup", "duplicate-identity", "missing-key", "SSH", "unknown-alias", "foreign-owner", "DNS"} {
		t.Run(failure, func(t *testing.T) {
			fixture, client, facts := byohPreflightFixture(t)
			switch failure {
			case "foreign-workload":
				_, err := client.CoreV1().Pods("other").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "other"}, Spec: corev1.PodSpec{NodeName: "host1.example.test"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			case "disposable-disagreement":
				pool, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
				pool.Data["host3.example.test"] = strings.ReplaceAll(legacyBYOHRecord(false), "address-type: ip", "address-type: dns")
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), pool, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			case "matching-key", "wrong-key":
				pool, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
				key := facts["host1.example.test"].PublicKey
				if failure == "wrong-key" {
					key = testHostPublicKey(t)
				}
				pool.Data["192.0.2.1"] += "ssh-host-public-key: " + key + "\n"
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), pool, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			case "api":
				client.PrependReactor("get", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("API unavailable")
				})
			case "probe-admission", "probe-cleanup":
				fixture.preflightProbeForTest = func(context.Context, *byohHostLease) (byohHostFacts, error) {
					return byohHostFacts{}, errors.New(failure)
				}
			case "duplicate-identity":
				host := facts["host2.example.test"]
				host.UUID = facts["host1.example.test"].UUID
				facts["host2.example.test"] = host
			case "missing-key":
				host := facts["host3.example.test"]
				host.PublicKey = ""
				facts["host3.example.test"] = host
			case "SSH":
				fixture.sshClientForTest = func(context.Context, *byohHostLease) (*windowsSSHClient, error) {
					return nil, errors.New("authentication")
				}
			case "unknown-alias", "foreign-owner":
				cm, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohInstancesConfigMap, metav1.GetOptions{})
				if failure == "unknown-alias" {
					cm.Data["unknown.example.test"] = "username=test-user"
				} else {
					cm.Annotations[registrationOwnershipAnnotation("192.0.2.1")] = "foreign"
				}
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			case "DNS":
				fixture.preflightLookupForTest = func(context.Context, string) ([]net.IP, error) { return nil, errors.New("DNS unavailable") }
			}
			client.ClearActions()
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			if (err == nil) != (failure == "none" || failure == "matching-key") {
				t.Fatalf("preflight failure=%s err=%v", failure, err)
			}
			if proof != nil && len(proof.Hosts) != 3 {
				t.Fatal("not all three hosts proved")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					t.Fatalf("adoption/mutation before complete proof: %s", action.GetVerb())
				}
			}
		})
	}
}

func TestBYOHSessionPreparationResetsUnusedHosts(t *testing.T) {
	for _, failThird := range []bool{false, true} {
		t.Run(fmt.Sprintf("partialFailure=%t", failThird), func(t *testing.T) {
			fixture, client, _ := byohPreflightFixture(t)
			proof, err := fixture.preflightBYOH(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			session := &byohRunSession{Scope: byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}, Nonce: "nonce", Phase: "Initializing", UID: "session"}
			session.Name = byohSessionName(session.Scope)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: session.Name, Namespace: wmcoNamespace, UID: session.UID, ResourceVersion: "1", Annotations: map[string]string{byohSessionAnnotation: mustBYOHJSON(session)}}}
			if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Create(context.Background(), cm, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			fixture.waitForLeaseLog = func(ctx context.Context, lease *byohHostLease, message string, _ time.Time) error {
				registrations, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(ctx, byohInstancesConfigMap, metav1.GetOptions{})
				if len(registrations.Data) != 0 {
					t.Fatal("reset began before all-alias withdrawal")
				}
				seen[lease.NodeName] = true
				if failThird && lease.NodeName == "host3.example.test" {
					return errors.New("unproved deconfiguration")
				}
				if message == "instance has been deconfigured" {
					return client.CoreV1().Nodes().Delete(ctx, lease.NodeName, metav1.DeleteOptions{})
				}
				return nil
			}
			err = fixture.prepareBYOHHosts(context.Background(), proof, session)
			if (err != nil) != failThird {
				t.Fatalf("prepare err=%v", err)
			}
			if len(seen) != 3 {
				t.Fatal("unused initial host was not included in preparation")
			}
			ledger, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), session.Name, metav1.GetOptions{})
			if failThird {
				if len(ledger.Data) != 0 || session.Phase == "Prepared" {
					t.Fatal("partial reset exposed available hosts")
				}
			} else {
				entries, err := validateBYOHPoolInventoryWithTrust(ledger.Data, true)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 6 || session.Phase != "Prepared" {
					t.Fatal("prepared private ledger is incomplete")
				}
				fixture.session, err = decodeBYOHSession(ledger, session.Scope)
				if err != nil {
					t.Fatal(err)
				}
				session = fixture.session
				fixture.caseID = "case"
				if err := fixture.validateBYOHCaseStart(context.Background()); err != nil {
					t.Fatal(err)
				}
				session.ActiveCase = "case"
				ledger.Annotations[byohSessionAnnotation] = mustBYOHJSON(session)
				if _, err := client.CoreV1().ConfigMaps(wmcoNamespace).Update(context.Background(), ledger, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				beforePool, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
				leases, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressDNS, RequireTrustedSSH: true})
				if err != nil {
					t.Fatal(err)
				}
				if leases[0].PhysicalUUID == "" || !leases[0].PreserveInstancesConfigMap {
					t.Fatal("case lost physical trust or CM preservation")
				}
				if err := fixture.verifyBYOHUnusedReset(context.Background(), leases[0]); err != nil {
					t.Fatal(err)
				}
				if err := fixture.releaseVerified(leases...); err != nil {
					t.Fatal(err)
				}
				afterPool, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
				if !reflect.DeepEqual(beforePool, afterPool) {
					t.Fatal("private case release changed producer records")
				}
				afterPool.Data["192.0.2.1"] = legacyBYOHRecord(false)
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), afterPool, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.allocateBYOHHosts(byohHostRequest{AddressType: byohAddressIP}); err == nil {
					t.Fatal("retained prepared evidence overrode changed producer ownership")
				}
				if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), beforePool, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
			}
			producer, _ := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap, metav1.GetOptions{})
			for address, raw := range producer.Data {
				record, err := decodeBYOHProducerRecord(address, raw)
				if err != nil {
					t.Fatal(err)
				}
				if record.Fields["status"] != "allocated" || record.Fields["test-id"] != "run" {
					t.Fatal("initial producer reservation released")
				}
			}
		})
	}
}

func TestBYOHSessionFailureErrorIdentity(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		for _, saveFailed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/saveFailed=%t", cause, saveFailed), func(t *testing.T) {
				fixture, client, _ := byohPreflightFixture(t)
				unsafeCause := errors.New("hostile payload peer key script\nprivate diagnostic")
				saveCause := errors.New("hostile persisted string")
				probeCalls := 0
				fixture.preflightProbeForTest = func(context.Context, *byohHostLease) (byohHostFacts, error) {
					probeCalls++
					return byohHostFacts{}, errors.Join(fmt.Errorf("%s: %w", unsafeCause, cause), unsafeCause)
				}
				client.PrependReactor("create", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
					created := action.(clienttesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
					created.UID, created.ResourceVersion = "session", "1"
					return true, created, client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), created, wmcoNamespace)
				})
				if saveFailed {
					client.PrependReactor("update", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
						return true, nil, saveCause
					})
				}
				scope := byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}
				_, err := fixture.ensureBYOHSession(context.Background(), scope)
				if !errors.Is(err, cause) || !errors.Is(err, unsafeCause) || errors.Is(err, saveCause) != saveFailed {
					t.Fatalf("in-process error identities not retained: %v", err)
				}
				summary := "BYOH preflight/preparation failed: " + fixture.preparationDiagnostic.summary()
				if saveFailed {
					summary += "; failed-session-save=unknown"
				}
				if err.Error() != summary || strings.Contains(err.Error(), "hostile") || strings.Contains(err.Error(), "\n") {
					t.Fatalf("unsafe public error: %q", err.Error())
				}
				if saveFailed {
					return
				}
				maps := fixture.coreClient.ConfigMaps(wmcoNamespace)
				retained, err := maps.Get(context.Background(), byohSessionName(scope), metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				annotation := retained.Annotations[byohSessionAnnotation]
				owner, err := decodeBYOHSession(retained, scope)
				if err != nil || owner.Phase != "Failed" || owner.Failure == nil || strings.Contains(annotation, "hostile") {
					t.Fatalf("unsafe retained diagnostic: %v", err)
				}
				_, err = fixture.ensureBYOHSession(context.Background(), scope)
				if err == nil || errors.Is(err, cause) || errors.Is(err, unsafeCause) || !strings.Contains(err.Error(), "restart prohibited") || !strings.Contains(err.Error(), owner.Failure.summary()) {
					t.Fatalf("retained failure manufactured identity or lost safe cause: %v", err)
				}
				retained, err = maps.Get(context.Background(), byohSessionName(scope), metav1.GetOptions{})
				if err != nil || probeCalls != 1 || retained.Annotations[byohSessionAnnotation] != annotation {
					t.Fatalf("failed initializer restarted or retained cause changed: probes=%d err=%v", probeCalls, err)
				}
			})
		}
	}
}

func TestBYOHSessionCrossProcessFailureDoesNotRestart(t *testing.T) {
	if address := os.Getenv("WMCO_BYOH_SESSION_TEST_SERVER"); address != "" {
		client, err := kubernetes.NewForConfig(&rest.Config{Host: address, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}})
		if err != nil {
			t.Fatal(err)
		}
		fixture := &byohTestFixture{coreClient: client.CoreV1(), dynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}: "MachineList"})}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := fixture.ensureBYOHSession(ctx, byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"}); err == nil {
			t.Fatal("malformed preflight unexpectedly prepared")
		}
		return
	}
	var mutex sync.Mutex
	var session *corev1.ConfigMap
	creates, poolReads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			if session != nil {
				response.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(response).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: "Failure", Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			var created corev1.ConfigMap
			if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
				t.Error(err)
				response.WriteHeader(400)
				return
			}
			created.UID = "session"
			created.ResourceVersion = "1"
			created.APIVersion = "v1"
			created.Kind = "ConfigMap"
			session = &created
			creates++
			response.WriteHeader(201)
			_ = json.NewEncoder(response).Encode(session)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/"+byohPoolConfigMap) {
			poolReads++
			_ = json.NewEncoder(response).Encode(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap, UID: "pool", ResourceVersion: "1"}, Data: map[string]string{"192.0.2.1": "malformed"}})
			return
		}
		if request.Method == http.MethodPut {
			var updated corev1.ConfigMap
			if err := json.NewDecoder(request.Body).Decode(&updated); err != nil {
				t.Error(err)
				return
			}
			session = &updated
		}
		if session == nil {
			response.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(response).Encode(session)
	}))
	defer server.Close()
	commands := []*exec.Cmd{}
	for index := 0; index < 2; index++ {
		command := exec.Command(os.Args[0], "-test.run=^TestBYOHSessionCrossProcessFailureDoesNotRestart$", "-test.timeout=20s")
		command.Env = append(os.Environ(), "WMCO_BYOH_SESSION_TEST_SERVER="+server.URL)
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	mutex.Lock()
	defer mutex.Unlock()
	if creates != 1 || poolReads != 1 {
		t.Fatalf("initializer restarted: creates=%d preflights=%d", creates, poolReads)
	}
	owner, err := decodeBYOHSession(session, byohRunScope{Run: "run", PoolUID: "pool", ClusterUID: "cluster"})
	if err != nil || owner.Phase != "Failed" {
		t.Fatalf("failed evidence was not retained: %v", err)
	}
	t.Log("two independent run-test processes: one initializer, one preflight, retained failed evidence")
}
