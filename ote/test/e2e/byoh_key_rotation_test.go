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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestPublicKeyHashAndNodeReadiness(t *testing.T) {
	key, err := generateTestPrivateKeyPEM()
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
	if !nodeReadyWithKeyHash(node, hash) || !nodeReadyAndConverged(node) {
		t.Fatal("Ready, schedulable, labelled, version-converged Node did not satisfy its exact key hash")
	}
}

func TestKeyHashReadinessRequiresExactConvergedUID(t *testing.T) {
	key := []byte("non-secret readiness fixture")
	hash := strings.Repeat("a", 64)
	base := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	base.Annotations[byohPublicKeyHashAnno] = hash
	base.Annotations[byohUsernameAnno] = encryptWMCOUsernameForTest(t, "test-user", key)
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
	if _, err := decryptWMCOUsername("not armored ciphertext", []byte("fixture key")); err == nil {
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

func TestBYOHKeyReadinessComposesSharedPredicate(t *testing.T) {
	key := []byte("non-secret composition fixture")
	hash := strings.Repeat("a", 64)
	base := readyBYOHNode("node-a", "uid-a", "192.0.2.10")
	base.Annotations[byohPublicKeyHashAnno] = hash
	base.Annotations[byohUsernameAnno] = encryptWMCOUsernameForTest(t, "test-user", key)
	for _, test := range []struct {
		name   string
		mutate func(*corev1.Node)
	}{
		{name: "exact retained UID", mutate: func(node *corev1.Node) { node.UID = "replacement" }},
		{name: "BYOH label", mutate: func(node *corev1.Node) { delete(node.Labels, "windowsmachineconfig.openshift.io/byoh") }},
		{name: "schedulability", mutate: func(node *corev1.Node) { node.Spec.Unschedulable = true }},
		{name: "version convergence", mutate: func(node *corev1.Node) { node.Annotations[byohVersionAnno] = "old" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := base.DeepCopy()
			test.mutate(node)
			if !nodeReadyWithKeyHash(node, hash) {
				t.Fatal("shared Ready/hash predicate unexpectedly rejected the fixture")
			}
			lease := &byohHostLease{NodeName: base.Name, NodeUID: base.UID, Username: "test-user"}
			ready, err := leaseNodeKeyAndUsernameConverged(context.Background(),
				fake.NewSimpleClientset(node).CoreV1().Nodes(), lease, hash, key, "", false)
			if err != nil || ready {
				t.Fatalf("BYOH accepted weakened composition: ready=%t err=%v", ready, err)
			}
		})
	}
}

func TestKeyUsernameConvergenceUsesOneRetainedNodeRead(t *testing.T) {
	key := []byte("non-secret convergence fixture")
	hash := strings.Repeat("a", 64)
	ciphertext := encryptWMCOUsernameForTest(t, "test-user", key)
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

func TestKeyRotationPreflightRejectsWrongOriginalUsername(t *testing.T) {
	key, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := publicKeyHash(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, ciphertext := range []string{"malformed ciphertext", encryptWMCOUsernameForTest(t, "different-user", key)} {
		node := readyBYOHNode("node-a", "node-uid", "192.0.2.10")
		node.Annotations[byohPublicKeyHashAnno] = hash
		node.Annotations[byohUsernameAnno] = ciphertext
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret,
			Namespace: wmcoNamespace, UID: "secret-uid"}, Data: map[string][]byte{byohPrivateKeyDataKey: key}}
		client := fake.NewSimpleClientset(node, secret)
		fixture := &byohTestFixture{ctx: context.Background(), coreClient: client.CoreV1()}
		lease := &byohHostLease{NodeName: node.Name, NodeUID: node.UID, Username: "test-user"}
		if err := fixture.rotateBYOHPrivateKey(lease); err == nil {
			t.Fatal("rotation accepted an invalid original username")
		}
		if fixture.secretSnapshot != nil || fixture.replacementAuthorizedKey != "" {
			t.Fatal("rotation retained mutation state before validating the original username")
		}
		for _, action := range client.Actions() {
			if action.GetVerb() != "get" {
				t.Fatal("rotation mutated resources before validating the original username")
			}
		}
	}
}

func TestKeyRotationPreflightRejectsUnsafeOriginalState(t *testing.T) {
	key, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := publicKeyHash(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"empty Secret UID", "empty key", "wrong Node UID", "wrong hash", "not Ready", "missing BYOH label", "version mismatch", "unschedulable", "missing ciphertext"} {
		t.Run(mode, func(t *testing.T) {
			node := readyBYOHNode("selected", "node-uid", "192.0.2.10")
			node.Annotations[byohPublicKeyHashAnno] = hash
			node.Annotations[byohUsernameAnno] = encryptWMCOUsernameForTest(t, "unit-user", key)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: byohPrivateKeySecret, Namespace: wmcoNamespace, UID: "secret-uid"}, Data: map[string][]byte{byohPrivateKeyDataKey: key}}
			switch mode {
			case "empty Secret UID":
				secret.UID = ""
			case "empty key":
				secret.Data = nil
			case "wrong Node UID":
				node.UID = "foreign-node"
			case "wrong hash":
				node.Annotations[byohPublicKeyHashAnno] = "wrong-hash"
			case "not Ready":
				node.Status.Conditions = nil
			case "missing BYOH label":
				node.Labels = nil
			case "version mismatch":
				node.Annotations[byohDesiredVersionAnno] = "different-version"
			case "unschedulable":
				node.Spec.Unschedulable = true
			case "missing ciphertext":
				delete(node.Annotations, byohUsernameAnno)
			}
			client := fake.NewSimpleClientset(node, secret)
			fixture := &byohTestFixture{ctx: context.Background(), coreClient: client.CoreV1()}
			fixture.placeAuthorizedKeyForTest = func(context.Context, *byohHostLease, string) error {
				t.Fatal("preflight failure attempted authorized-key mutation")
				return nil
			}
			lease := &byohHostLease{NodeName: node.Name, NodeUID: "node-uid", Username: "unit-user"}
			if err := fixture.rotateBYOHPrivateKey(lease); err == nil {
				t.Fatal("unsafe original state passed preflight")
			}
			if fixture.secretSnapshot != nil || fixture.replacementAuthorizedKey != "" {
				t.Fatal("preflight retained mutation state")
			}
			for _, action := range client.Actions() {
				if action.GetVerb() != "get" {
					t.Fatal("preflight failure mutated resources")
				}
			}
		})
	}
}

func TestKeyRotationUsesHostProcessThenSSHAndRestoresAfterLaterFailure(t *testing.T) {
	originalKey, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	originalHash, err := publicKeyHash(originalKey)
	if err != nil {
		t.Fatal(err)
	}
	const username = "test-user"
	originalCiphertext := encryptWMCOUsernameForTest(t, username, originalKey)
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
		current.Annotations[byohUsernameAnno] = encryptWMCOUsernameForTest(t, username, key)
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
	originalConvergenceError := errors.New("injected original convergence failure")
	originalAuthenticationError := errors.New("injected original authentication failure")
	replacementRemovalError := errors.New("injected replacement removal failure")
	for _, testCase := range []struct {
		mode          string
		rotationError string
		cleanupError  error
		rotationOrder []string
		cleanupOrder  []string
	}{
		{"secret update failure", "injected Secret update failure", nil,
			[]string{"hostprocess-place", "secret-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original", "hostprocess-remove"}},
		{"secret applied response loss then replacement SSH failure", "injected replacement authentication failure", nil,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement", "ssh-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original", "hostprocess-remove"}},
		{"annotation timeout", "replacement key annotations did not converge", nil,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original", "hostprocess-remove"}},
		{"replacement SSH cancellation", "context canceled", nil,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement", "ssh-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original", "hostprocess-remove"}},
		{"original convergence failure", "force cleanup after successful later phases", originalConvergenceError,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement", "ssh-replacement"},
			[]string{"secret-original", "convergence-original"}},
		{"original authentication failure", "force cleanup after successful later phases", originalAuthenticationError,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement", "ssh-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original"}},
		{"replacement removal failure", "force cleanup after successful later phases", replacementRemovalError,
			[]string{"hostprocess-place", "secret-replacement", "convergence-replacement", "ssh-replacement"},
			[]string{"secret-original", "convergence-original", "converged-original", "ssh-original", "hostprocess-remove"}},
	} {
		mode := testCase.mode
		t.Run(mode, func(t *testing.T) {
			originalKey, err := generateTestPrivateKeyPEM()
			if err != nil {
				t.Fatal(err)
			}
			originalHash, err := publicKeyHash(originalKey)
			if err != nil {
				t.Fatal(err)
			}
			const username = "test-user"
			originalCiphertext := encryptWMCOUsernameForTest(t, username, originalKey)
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
			var restoreContexts []context.Context
			checkRestoreContext := func(ctx context.Context) {
				t.Helper()
				if len(restoreContexts) == 0 || ctx != restoreContexts[len(restoreContexts)-1] {
					t.Fatal("restoration phase did not use its convergence context")
				}
			}
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
			fixture.removeAuthorizedKeyForTest = func(ctx context.Context, _ *byohHostLease, _ string) error {
				checkRestoreContext(ctx)
				order = append(order, "hostprocess-remove")
				if mode == "replacement removal failure" {
					return replacementRemovalError
				}
				return nil
			}
			fixture.keyConvergenceForTest = func(ctx context.Context, _ *byohHostLease, _ string, key []byte,
				_ string, requireChange bool) error {
				if requireChange {
					order = append(order, "convergence-replacement")
					if ctx != fixture.ctx {
						t.Fatal("replacement convergence lost rotation context")
					}
				} else {
					order = append(order, "convergence-original")
					restoreContexts = append(restoreContexts, ctx)
					if _, bounded := ctx.Deadline(); !bounded || ctx == fixture.ctx || ctx.Err() != nil {
						t.Fatal("original convergence did not receive a live bounded cleanup context")
					}
				}
				if requireChange && mode == "annotation timeout" {
					return context.DeadlineExceeded
				}
				if !requireChange && !bytes.Equal(key, originalKey) {
					t.Fatal("original convergence used unexpected key bytes")
				}
				if !requireChange && mode == "original convergence failure" {
					return originalConvergenceError
				}
				if !requireChange {
					order = append(order, "converged-original")
				}
				return nil
			}
			fixture.verifySSHForTest = func(ctx context.Context, _ *byohHostLease, key []byte) error {
				if bytes.Equal(key, originalKey) {
					checkRestoreContext(ctx)
					order = append(order, "ssh-original")
					if mode == "original authentication failure" {
						return originalAuthenticationError
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
			if testCase.rotationError == "" || !strings.Contains(rotationErr.Error(), testCase.rotationError) {
				t.Fatalf("failure fixture missed intended phase: %v", rotationErr)
			}
			if !slices.Equal(order, testCase.rotationOrder) {
				t.Fatalf("unexpected rotation phase order: %v", order)
			}
			cleanupStart := len(order)
			cleanupErr := fixture.cleanupWithBudget(time.Second, 200*time.Millisecond)
			expectedCleanupOrder := append([]string(nil), testCase.cleanupOrder...)
			expectedAttempts := 1
			if testCase.cleanupError != nil {
				if fixture.replacementAuthorizedKey == "" {
					t.Fatal("failed restoration discarded replacement-key protection")
				}
				expectedAttempts = 2
				expectedCleanupOrder = append(expectedCleanupOrder, testCase.cleanupOrder...)
				if !errors.Is(cleanupErr, testCase.cleanupError) || !strings.Contains(cleanupErr.Error(), "bounded key-restoration fallback") {
					t.Fatalf("cleanup did not aggregate its injected primary/fallback failure: %v", cleanupErr)
				}
			} else if !fixture.secretRestored || fixture.replacementAuthorizedKey != "" {
				t.Fatal("successful restoration did not release replacement-key protection")
			}
			if !slices.Equal(order[cleanupStart:], expectedCleanupOrder) || restoreAttempts != expectedAttempts || len(restoreContexts) != expectedAttempts {
				t.Fatalf("unexpected cleanup phases or attempts: order=%v writes=%d contexts=%d", order[cleanupStart:], restoreAttempts, len(restoreContexts))
			}
			if expectedAttempts == 2 {
				primaryDeadline, _ := restoreContexts[0].Deadline()
				fallbackDeadline, _ := restoreContexts[1].Deadline()
				if restoreContexts[0] == restoreContexts[1] || !primaryDeadline.Before(fallbackDeadline) {
					t.Fatal("cleanup did not use a separate reserved bounded fallback context")
				}
			}
			t.Logf("verified cleanup order=%v restoration attempts=%d bounded contexts=%d injected cleanup cause=%t", order[cleanupStart:], restoreAttempts, len(restoreContexts), testCase.cleanupError != nil)
			if mode == "original convergence failure" || mode == "original authentication failure" {
				if cleanupErr == nil || slices.Contains(order, "hostprocess-remove") || fixture.secretRestored {
					t.Fatal("failed original convergence/authentication did not retain replacement-key protection")
				}
			}
			if mode == "replacement removal failure" && (cleanupErr == nil || !slices.Contains(order, "hostprocess-remove") || fixture.secretRestored) {
				t.Fatal("replacement removal failure was not reached or was marked restored")
			}
			combined := errors.Join(rotationErr, cleanupErr)
			if !errors.Is(combined, rotationErr) || (testCase.cleanupError != nil && !errors.Is(combined, testCase.cleanupError)) {
				t.Fatal("combined rotation/cleanup error lost an injected cause")
			}
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
