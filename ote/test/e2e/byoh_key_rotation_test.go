package winc

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestPublicKeyHashAndNodeReadiness(t *testing.T) {
	key, err := generateTestPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := publicKeyHash(key)
	if err != nil || len(hash) != 64 {
		t.Fatalf("publicKeyHash() = %q, %v", hash, err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "selected", UID: "uid-selected",
		Labels: map[string]string{"windowsmachineconfig.openshift.io/byoh": "true"}, Annotations: map[string]string{
			byohPublicKeyHashAnno: hash, byohVersionAnno: "v1", byohDesiredVersionAnno: "v1"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if !nodeReadyWithKeyHash(node, hash) {
		t.Fatal("Ready, schedulable, labelled, version-converged Node did not satisfy its exact key hash")
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

func TestKeyHashReadinessRequiresExactConvergedUID(t *testing.T) {
	key := []byte("non-secret readiness fixture")
	hash := strings.Repeat("a", 64)
	base := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	base.Annotations[byohPublicKeyHashAnno] = hash
	base.Annotations[byohUsernameAnno] = encryptBYOHUsernameForTest(t, "test-user", key)
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
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, Username: "test-user"}
			test.mutate(node, lease)
			ready, err := leaseNodeKeyAndUsernameConverged(context.Background(),
				fake.NewSimpleClientset(node).CoreV1().Nodes(), lease, hash, key, "", false)
			if err != nil || ready {
				t.Fatalf("non-converged node accepted: ready=%t err=%v", ready, err)
			}
		})
	}
	client := fake.NewSimpleClientset()
	client.PrependReactor("get", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("node API unavailable")
	})
	lease := &byohHostLease{NodeName: "node-a", NodeUID: "uid-a", Username: "test-user"}
	if _, err := leaseNodeKeyAndUsernameConverged(context.Background(), client.CoreV1().Nodes(),
		lease, hash, key, "", false); err == nil {
		t.Fatal("Node API error was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := leaseNodeKeyAndUsernameConverged(ctx, client.CoreV1().Nodes(),
		lease, hash, key, "", false); err == nil {
		t.Fatal("canceled context was ignored")
	}
}

func TestMalformedCiphertextAndRejectedOldNodeUID(t *testing.T) {
	if _, err := decryptBYOHUsername("not armored ciphertext", []byte("fixture key")); err == nil {
		t.Fatal("malformed encrypted username was accepted")
	}
	lease := &byohHostLease{Address: "192.0.2.10", AddressType: byohAddressIP}
	old := readyBYOHNode("node-a", "old-uid", lease.Address)
	lookup := func(context.Context, string) ([]net.IP, error) { return nil, nil }
	node, ready, err := exactLeaseNode(context.Background(), []corev1.Node{*old}, lease, lookup, old.UID)
	if err != nil || ready || node != nil {
		t.Fatalf("old Node UID was accepted as its own replacement: node=%#v ready=%t err=%v", node, ready, err)
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
	t.Run("constant-time key comparison covers equal mismatched and different lengths", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
			Namespace: wmcoNamespace, UID: "secret-uid"},
			Data: map[string][]byte{byohPrivateKeyDataKey: []byte("fixture-private-key")}}
		secrets := fake.NewSimpleClientset(secret).CoreV1().Secrets(wmcoNamespace)
		if err := privateKeySecretMatches(context.Background(), secrets, secret.UID,
			[]byte("fixture-private-key")); err != nil {
			t.Fatalf("equal key bytes did not match: %v", err)
		}
		for _, mismatched := range [][]byte{[]byte("fixture-private-kez"), []byte("short")} {
			err := privateKeySecretMatches(context.Background(), secrets, secret.UID, mismatched)
			if err == nil {
				t.Fatalf("mismatched key bytes were accepted: %q", mismatched)
			}
			if strings.Contains(err.Error(), string(mismatched)) || strings.Contains(err.Error(), "fixture-private-key") {
				t.Fatalf("key comparison error exposed key bytes: %v", err)
			}
		}
	})

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

func TestKeyRotationUsesHostProcessThenSSHAndRestoresAfterLaterFailure(t *testing.T) {
	originalKey, err := generateTestPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	originalHash, err := publicKeyHash(originalKey)
	if err != nil {
		t.Fatal(err)
	}
	const username = "test-user"
	originalCiphertext := encryptBYOHUsernameForTest(t, username, originalKey)
	node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
	node.Annotations[byohPublicKeyHashAnno] = originalHash
	node.Annotations[byohUsernameAnno] = originalCiphertext
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
		Namespace: wmcoNamespace, UID: "secret-uid", ResourceVersion: "1"},
		Data: map[string][]byte{byohPrivateKeyDataKey: append([]byte(nil), originalKey...)}}
	client := fake.NewSimpleClientset(node, secret)
	secretResource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	nodeResource := schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	order := []string{}
	client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
		key := updated.Data[byohPrivateKeyDataKey]
		phase := "replacement"
		if bytes.Equal(key, originalKey) {
			phase = "original"
		}
		order = append(order, "secret-"+phase)
		if err := client.Tracker().Update(secretResource, updated, wmcoNamespace); err != nil {
			t.Fatalf("update tracked Secret: %v", err)
		}
		currentObject, err := client.Tracker().Get(nodeResource, "", node.Name)
		if err != nil {
			t.Fatalf("get tracked Node: %v", err)
		}
		current, ok := currentObject.(*corev1.Node)
		if !ok {
			t.Fatalf("tracked Node has unexpected type %T", currentObject)
		}
		current = current.DeepCopy()
		hash, err := publicKeyHash(key)
		if err != nil {
			t.Fatalf("hash updated key: %v", err)
		}
		current.Annotations[byohPublicKeyHashAnno] = hash
		current.Annotations[byohUsernameAnno] = encryptBYOHUsernameForTest(t, username, key)
		if err := client.Tracker().Update(nodeResource, current, ""); err != nil {
			t.Fatalf("update tracked Node annotations: %v", err)
		}
		return true, updated, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, Username: username,
		HostID: "host-a", LeaseID: "lease-a"}
	fixture := &byohTestFixture{ctx: ctx, cancel: cancel, coreClient: client.CoreV1(), secretLease: lease}
	fixture.placeAuthorizedKeyForTest = func(_ context.Context, got *byohHostLease, authorizedKey string) error {
		if got != lease || strings.TrimSpace(authorizedKey) == "" {
			t.Fatal("HostProcess key placement did not receive the retained lease/public key")
		}
		order = append(order, "hostprocess-place")
		return nil
	}
	fixture.removeAuthorizedKeyForTest = func(_ context.Context, got *byohHostLease, authorizedKey string) error {
		if got != lease || authorizedKey != fixture.replacementAuthorizedKey {
			t.Fatal("HostProcess key removal did not receive the retained lease/replacement key")
		}
		order = append(order, "hostprocess-remove")
		return nil
	}
	fixture.verifySSHForTest = func(ctx context.Context, got *byohHostLease, privateKey []byte) error {
		if got != lease {
			t.Fatal("SSH authentication used a different lease")
		}
		hash, err := publicKeyHash(privateKey)
		if err != nil {
			return err
		}
		current, err := client.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil || current.Annotations[byohPublicKeyHashAnno] != hash {
			t.Fatalf("direct SSH proof ran before Node key convergence: node=%#v err=%v", current, err)
		}
		order = append(order, "ssh-auth")
		return nil
	}
	fixture.configuredServicesForTest = func(context.Context, *byohHostLease) error {
		order = append(order, "hostprocess-services")
		return errors.New("later configured-service probe failure")
	}
	if err := fixture.rotateBYOHPrivateKey(lease); err == nil {
		t.Fatal("injected later HostProcess service failure was ignored")
	}
	lease.CleanupDone = true
	fixture.leases = []*byohHostLease{lease}
	if err := fixture.cleanupWithBudget(time.Second, 200*time.Millisecond); err != nil {
		t.Fatalf("actual cleanup did not restore original key after later failure: %v", err)
	}
	wantOrder := []string{"hostprocess-place", "secret-replacement", "ssh-auth", "hostprocess-services",
		"secret-original", "ssh-auth", "hostprocess-remove"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("key rotation/restoration transport order = %v, want %v", order, wantOrder)
	}
	current, err := client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), byohPrivateKeySecret,
		metav1.GetOptions{})
	if err != nil || !bytes.Equal(current.Data[byohPrivateKeyDataKey], originalKey) || !fixture.secretRestored {
		t.Fatalf("original Secret key was not restored: restored=%t current=%#v err=%v",
			fixture.secretRestored, current, err)
	}
}

func TestKeyRotationFailureMatrixDrivesActualCleanup(t *testing.T) {
	for _, mode := range []string{
		"secret update failure",
		"secret applied response loss then replacement SSH failure",
		"annotation timeout",
		"replacement SSH cancellation",
		"original convergence failure",
		"original authentication failure",
		"replacement removal failure",
	} {
		t.Run(mode, func(t *testing.T) {
			originalKey, err := generateTestPrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			originalHash, err := publicKeyHash(originalKey)
			if err != nil {
				t.Fatal(err)
			}
			const username = "test-user"
			const originalCiphertext = "original-ciphertext-marker"
			node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
			node.Annotations[byohPublicKeyHashAnno] = originalHash
			node.Annotations[byohUsernameAnno] = originalCiphertext
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
				Namespace: wmcoNamespace, UID: "secret-uid", ResourceVersion: "1"},
				Data: map[string][]byte{byohPrivateKeyDataKey: append([]byte(nil), originalKey...)}}
			poolEntry := strings.Replace(testPoolEntry("ip", "host-a", "allocated", "lease-a"),
				"disposable: false", "disposable: true", 1)
			pool := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: byohPoolConfigMap,
				Namespace: wmcoNamespace}, Data: map[string]string{"192.0.2.10": poolEntry}}
			client := fake.NewSimpleClientset(node, secret, pool)
			secretResource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			order := []string{}
			restoreAttempts := 0
			client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
				updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
				if updated.UID != secret.UID {
					t.Fatalf("Secret restoration did not retain UID: %q", updated.UID)
				}
				isOriginal := bytes.Equal(updated.Data[byohPrivateKeyDataKey], originalKey)
				if isOriginal {
					restoreAttempts++
					order = append(order, "secret-original")
				} else {
					order = append(order, "secret-replacement")
				}
				if !isOriginal && mode == "secret update failure" {
					return true, nil, errors.New("injected Secret update failure")
				}
				if err := client.Tracker().Update(secretResource, updated, wmcoNamespace); err != nil {
					t.Fatal(err)
				}
				if !isOriginal && mode == "secret applied response loss then replacement SSH failure" {
					return true, nil, errors.New("injected applied Secret response loss")
				}
				return true, updated, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, Username: username,
				HostID: "host-a", LeaseID: "lease-a", Address: "192.0.2.10", SSHAddress: "ssh.example.test",
				Platform: "test", ResetPolicy: byohResetPolicy, AllocatedAt: "2026-10-04T00:00:00Z",
				ClaimedAliases: []string{"192.0.2.10"}, Disposable: true}
			fixture := &byohTestFixture{ctx: ctx, cancel: cancel, coreClient: client.CoreV1(), leases: []*byohHostLease{lease}}
			fixture.placeAuthorizedKeyForTest = func(context.Context, *byohHostLease, string) error {
				order = append(order, "hostprocess-place")
				return nil
			}
			fixture.removeAuthorizedKeyForTest = func(context.Context, *byohHostLease, string) error {
				order = append(order, "hostprocess-remove")
				if mode == "replacement removal failure" {
					return errors.New("injected replacement removal failure")
				}
				return nil
			}
			fixture.keyConvergenceForTest = func(_ context.Context, _ *byohHostLease, _ string, key []byte,
				_ string, requireChange bool) error {
				if requireChange && mode == "annotation timeout" {
					return context.DeadlineExceeded
				}
				if !requireChange && mode == "original convergence failure" {
					return context.DeadlineExceeded
				}
				if !requireChange && !bytes.Equal(key, originalKey) {
					t.Fatal("original convergence used unexpected key bytes")
				}
				return nil
			}
			fixture.verifySSHForTest = func(_ context.Context, _ *byohHostLease, key []byte) error {
				if bytes.Equal(key, originalKey) {
					order = append(order, "ssh-original")
					if mode == "original authentication failure" {
						return errors.New("injected original authentication failure")
					}
					return nil
				}
				order = append(order, "ssh-replacement")
				if mode == "replacement SSH cancellation" {
					return context.Canceled
				}
				if mode == "secret applied response loss then replacement SSH failure" {
					return errors.New("injected replacement authentication failure")
				}
				return nil
			}
			fixture.configuredServicesForTest = func(context.Context, *byohHostLease) error {
				return errors.New("force cleanup after successful later phases")
			}
			rotationErr := fixture.rotateBYOHPrivateKey(lease)
			if rotationErr == nil {
				t.Fatal("injected key-rotation failure was ignored")
			}
			cleanupErr := fixture.cleanupWithBudget(time.Second, 200*time.Millisecond)
			if restoreAttempts == 0 {
				t.Fatalf("actual cleanup never attempted original Secret restoration: order=%v err=%v", order, cleanupErr)
			}
			combined := errors.Join(rotationErr, cleanupErr)
			if strings.Contains(combined.Error(), string(originalKey)) || strings.Contains(combined.Error(), originalCiphertext) {
				t.Fatal("failure path exposed original key or ciphertext bytes")
			}
			livePool, err := client.CoreV1().ConfigMaps(wmcoNamespace).Get(context.Background(), byohPoolConfigMap,
				metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			entry, err := parseBYOHPoolEntry(lease.Address, livePool.Data[lease.Address])
			if err != nil || entry.Status != "unavailable" || entry.TestID != "" {
				t.Fatalf("disposable uncertain host was not quarantined: entry=%#v err=%v cleanup=%v",
					entry, err, cleanupErr)
			}
			originalIndex, removeIndex := slices.Index(order, "secret-original"), slices.Index(order, "hostprocess-remove")
			if removeIndex >= 0 && removeIndex < originalIndex {
				t.Fatalf("replacement key removal preceded original Secret restoration: %v", order)
			}
		})
	}
}
