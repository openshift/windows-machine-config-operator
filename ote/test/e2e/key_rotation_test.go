package winc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	restfake "k8s.io/client-go/rest/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestKeyRotationNeutralGenerationAndCanonicalHash(t *testing.T) {
	generated, err := generateTestPrivateKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(generated)
	if block == nil || block.Type != "RSA PRIVATE KEY" || len(rest) != 0 {
		t.Fatal("expected one PKCS#1 RSA block")
	}
	parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil || parsed.N.BitLen() != 2048 {
		t.Fatal("expected RSA-2048")
	}
	edKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize))
	der, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	edPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for _, key := range [][]byte{generated, edPEM} {
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		canonical := strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n")
		expected := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
		actual, err := publicKeyHash(key)
		if err != nil || actual != expected {
			t.Fatal("noncanonical authorized-key hash")
		}
	}
	if actual, err := publicKeyHash(edPEM); err != nil || actual != "13e92e91a8938a3d10a228e38f943d340faf2cf552586d0d88ff21baebabe261" {
		t.Fatal("fixed Ed25519 hash changed")
	}
	if _, err := publicKeyHash([]byte("malformed unit key")); err == nil {
		t.Fatal("accepted malformed key")
	}
}

func TestDecryptWMCOUsernameFormatsAndSafeErrors(t *testing.T) {
	key := []byte("unit-only passphrase")
	ciphertext := encryptWMCOUsernameForTest(t, "unit-user", key)
	body := strings.ReplaceAll(ciphertext, "<wmcoMarker>", "\n")
	for _, encoded := range []string{ciphertext, "-----BEGIN ENCRYPTED DATA-----\n\n" + body + "\n-----END ENCRYPTED DATA-----"} {
		decoded, err := decryptWMCOUsername(encoded, key)
		if err != nil || decoded != "unit-user" {
			t.Fatal("WMCO armor compatibility changed")
		}
	}
	for _, test := range []struct {
		ciphertext string
		key        []byte
	}{{ciphertext, nil}, {ciphertext, []byte("wrong unit passphrase")}, {"malformed-unit-ciphertext", key}} {
		_, err := decryptWMCOUsername(test.ciphertext, test.key)
		if err == nil {
			t.Fatal("invalid ciphertext/passphrase accepted")
		}
		for _, sensitive := range []string{ciphertext, "unit-user", string(key), "wrong unit passphrase", "malformed-unit-ciphertext"} {
			if strings.Contains(err.Error(), sensitive) {
				t.Fatal("decryption error exposed input or plaintext")
			}
		}
	}
}

type secretUpdateSpy struct {
	corev1client.SecretInterface
	submitted *corev1.Secret
}

func (spy *secretUpdateSpy) Update(ctx context.Context, secret *corev1.Secret, options metav1.UpdateOptions) (*corev1.Secret, error) {
	spy.submitted = secret
	return spy.SecretInterface.Update(ctx, secret, options)
}

func TestPrivateKeySecretFreshVersionAndPreservation(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName, Namespace: rotationTestNamespace, UID: "original-uid", ResourceVersion: "1", Labels: map[string]string{"preserve": "label"}, Annotations: map[string]string{"preserve": "annotation"}, Finalizers: []string{"example.test/preserve"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{rotationTestDataKey: []byte("old unit key"), "unrelated": []byte("original unrelated data")}}
	client := fake.NewSimpleClientset(secret)
	resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	key := []byte("replacement unit key")
	attempts := 0
	spy := &secretUpdateSpy{SecretInterface: client.CoreV1().Secrets(rotationTestNamespace)}
	client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		attempts++
		updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret)
		if attempts == 1 {
			if updated.ResourceVersion != "1" {
				t.Fatal("first update did not use original version")
			}
			concurrent := secret.DeepCopy()
			concurrent.ResourceVersion = "2"
			concurrent.Annotations["concurrent"] = "preserve"
			concurrent.Data["unrelated"] = []byte("concurrent unrelated data")
			if err := client.Tracker().Update(resource, concurrent, rotationTestNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, apierrors.NewConflict(resource.GroupResource(), secret.Name, errors.New("unit conflict"))
		}
		if attempts != 2 || updated.ResourceVersion != "2" || updated.UID != secret.UID {
			t.Fatal("retry did not fetch fresh version with original UID")
		}
		if !reflect.DeepEqual(updated.Labels, secret.Labels) || updated.Annotations["preserve"] != "annotation" || updated.Annotations["concurrent"] != "preserve" || !reflect.DeepEqual(updated.Finalizers, secret.Finalizers) || updated.Type != secret.Type || string(updated.Data["unrelated"]) != "concurrent unrelated data" {
			t.Fatal("update dropped unrelated metadata or concurrent data")
		}
		if err := client.Tracker().Update(resource, updated, rotationTestNamespace); err != nil {
			t.Fatal(err)
		}
		return true, updated, nil
	})
	if err := updatePrivateKeySecret(context.Background(), spy, secret.Name, rotationTestDataKey, secret.UID, key); err != nil {
		t.Fatal(err)
	}
	key[0] = 'X'
	if attempts != 2 || spy.submitted == nil || string(spy.submitted.Data[rotationTestDataKey]) != "replacement unit key" {
		t.Fatal("submitted key aliases caller bytes or retry not exercised")
	}
}

func TestPrivateKeySecretAmbiguityAndIdentityFailures(t *testing.T) {
	for _, mode := range []string{"empty captured UID", "unapplied response loss", "UID drift on conflict", "UID drift after write", "wrong key after success", "nil data"} {
		t.Run(mode, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName, Namespace: rotationTestNamespace, UID: "original-uid", ResourceVersion: "1"}, Data: map[string][]byte{rotationTestDataKey: []byte("old unit bytes")}}
			if mode == "nil data" {
				secret.Data = nil
			}
			client := fake.NewSimpleClientset(secret)
			resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
			writes := 0
			responseLoss := errors.New("unit response lost")
			client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
				writes++
				if writes > 1 {
					t.Fatal("foreign UID updated after drift")
				}
				updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
				if mode == "unapplied response loss" {
					return true, nil, responseLoss
				}
				if strings.HasPrefix(mode, "UID drift") {
					updated.UID = "foreign-uid"
				}
				if mode == "wrong key after success" {
					updated.Data[rotationTestDataKey] = []byte("wrong unit bytes")
				}
				if err := client.Tracker().Update(resource, updated, rotationTestNamespace); err != nil {
					t.Fatal(err)
				}
				if mode == "UID drift on conflict" {
					return true, nil, apierrors.NewConflict(resource.GroupResource(), secret.Name, errors.New("unit conflict"))
				}
				if mode == "UID drift after write" {
					return true, nil, responseLoss
				}
				return true, updated, nil
			})
			uid := secret.UID
			if mode == "empty captured UID" {
				uid = ""
			}
			err := updatePrivateKeySecret(context.Background(), client.CoreV1().Secrets(rotationTestNamespace), secret.Name, rotationTestDataKey, uid, []byte("replacement unit bytes"))
			if mode == "nil data" {
				if err != nil || writes != 1 {
					t.Fatal("nil data not initialized")
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe or unverified update accepted")
			}
			if mode == "empty captured UID" && writes != 0 {
				t.Fatal("empty UID permitted a write")
			}
			if mode == "unapplied response loss" && (!errors.Is(err, responseLoss) || !strings.Contains(err.Error(), "reconcile ambiguous")) {
				t.Fatal("unapplied ambiguity lost write or verification failure")
			}
			for _, sensitive := range []string{"replacement unit bytes", "old unit bytes", "wrong unit bytes"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatal("update error exposed key bytes")
				}
			}
		})
	}
}

func TestPrivateKeySecretForwardsRequestContext(t *testing.T) {
	secret := &corev1.Secret{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName, Namespace: rotationTestNamespace, UID: "original-uid", ResourceVersion: "1"}, Data: map[string][]byte{rotationTestDataKey: []byte("old unit key")}}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), t, "caller-context"))
	defer cancel()
	gets, puts := 0, 0
	restClient := &restfake.RESTClient{GroupVersion: corev1.SchemeGroupVersion, NegotiatedSerializer: scheme.Codecs.WithoutConversion(), VersionedAPIPath: "/api/v1", Client: restfake.CreateHTTPClient(func(request *http.Request) (*http.Response, error) {
		if request.Context().Value(t) != "caller-context" {
			t.Fatal("Secret request did not forward caller context")
		}
		if request.URL.Path != "/api/v1/namespaces/"+rotationTestNamespace+"/secrets/"+rotationTestSecretName {
			t.Fatal("request ignored explicit namespace or name")
		}
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		if request.Method == http.MethodGet {
			gets++
		}
		if request.Method == http.MethodPut {
			puts++
			if err := json.NewDecoder(request.Body).Decode(secret); err != nil {
				t.Fatal(err)
			}
		}
		body, err := json.Marshal(secret)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	secrets := corev1client.New(restClient).Secrets(rotationTestNamespace)
	if err := updatePrivateKeySecret(ctx, secrets, secret.Name, rotationTestDataKey, secret.UID, []byte("replacement unit key")); err != nil {
		t.Fatal(err)
	}
	if gets != 2 || puts != 1 {
		t.Fatalf("requests GET=%d PUT=%d", gets, puts)
	}
	cancel()
	if err := privateKeySecretMatches(ctx, secrets, secret.Name, rotationTestDataKey, secret.UID, []byte("replacement unit key")); !errors.Is(err, context.Canceled) {
		t.Fatal("verifier did not propagate cancellation")
	}
	if err := updatePrivateKeySecret(ctx, secrets, secret.Name, rotationTestDataKey, secret.UID, []byte("replacement unit key")); !errors.Is(err, context.Canceled) {
		t.Fatal("updater did not propagate cancellation")
	}
	if gets != 2 || puts != 1 {
		t.Fatal("cancelled context permitted successful I/O")
	}
}

const (
	rotationTestSecretName = "independent-key-secret"
	rotationTestDataKey    = "alternate-private-key"
	rotationTestNamespace  = "independent-key-namespace"
)

func encryptWMCOUsernameForTest(t *testing.T, plaintext string, key []byte) string {
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

func TestDecryptWMCOUsernameUsesCurrentKeyNotCiphertextEquality(t *testing.T) {
	key := []byte("non-secret deterministic unit fixture")
	first := encryptWMCOUsernameForTest(t, "fixture-user", key)
	second := encryptWMCOUsernameForTest(t, "fixture-user", key)
	if first == second {
		t.Fatal("test encryption unexpectedly produced equal ciphertext")
	}
	for _, ciphertext := range []string{first, second} {
		username, err := decryptWMCOUsername(ciphertext, key)
		if err != nil || username != "fixture-user" {
			t.Fatalf("decryptWMCOUsername() = %q, %v", username, err)
		}
	}
	if _, err := decryptWMCOUsername(first, []byte("different fixture key")); err == nil {
		t.Fatal("decryptWMCOUsername() accepted the wrong key")
	}
}

func TestPrivateKeySecretUIDGuardsAndAmbiguousUpdateReconciliation(t *testing.T) {
	t.Run("constant-time key comparison covers equal mismatched and different lengths", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName,
			Namespace: rotationTestNamespace, UID: "secret-uid"},
			Data: map[string][]byte{rotationTestDataKey: []byte("fixture-private-key")}}
		secrets := fake.NewSimpleClientset(secret).CoreV1().Secrets(rotationTestNamespace)
		if err := privateKeySecretMatches(context.Background(), secrets, rotationTestSecretName, rotationTestDataKey, secret.UID,
			[]byte("fixture-private-key")); err != nil {
			t.Fatalf("equal key bytes did not match: %v", err)
		}
		for _, mismatched := range [][]byte{[]byte("fixture-private-kez"), []byte("short")} {
			err := privateKeySecretMatches(context.Background(), secrets, rotationTestSecretName, rotationTestDataKey, secret.UID, mismatched)
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
			replacement := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName,
				Namespace: rotationTestNamespace, UID: "replacement-uid", ResourceVersion: "7"},
				Data: map[string][]byte{rotationTestDataKey: []byte("replacement key")}}
			client := fake.NewSimpleClientset(replacement)
			err := updatePrivateKeySecret(context.Background(), client.CoreV1().Secrets(rotationTestNamespace), rotationTestSecretName, rotationTestDataKey,
				"original-uid", []byte("test key"))
			if err == nil {
				t.Fatal("same-name replacement Secret was overwritten")
			}
			current, getErr := client.CoreV1().Secrets(rotationTestNamespace).Get(context.Background(),
				rotationTestSecretName, metav1.GetOptions{})
			if getErr != nil || current.UID != replacement.UID ||
				!bytes.Equal(current.Data[rotationTestDataKey], replacement.Data[rotationTestDataKey]) {
				t.Fatalf("replacement Secret was changed: current=%#v err=%v", current, getErr)
			}
		})
	}

	t.Run("applied update response loss is reconciled by UID and key", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: rotationTestSecretName,
			Namespace: rotationTestNamespace, UID: "original-uid", ResourceVersion: "3",
			Labels: map[string]string{"example.test/preserve": "true"}},
			Data: map[string][]byte{rotationTestDataKey: []byte("old key"), "unrelated": []byte("preserve")}}
		client := fake.NewSimpleClientset(secret)
		resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
		client.PrependReactor("update", "secrets", func(action clienttesting.Action) (bool, runtime.Object, error) {
			updated := action.(clienttesting.UpdateAction).GetObject().(*corev1.Secret).DeepCopy()
			if err := client.Tracker().Update(resource, updated, rotationTestNamespace); err != nil {
				t.Fatal(err)
			}
			return true, nil, errors.New("simulated update response loss")
		})
		newKey := []byte("new key")
		if err := updatePrivateKeySecret(context.Background(), client.CoreV1().Secrets(rotationTestNamespace), rotationTestSecretName, rotationTestDataKey,
			secret.UID, newKey); err != nil {
			t.Fatal(err)
		}
		current, err := client.CoreV1().Secrets(rotationTestNamespace).Get(context.Background(),
			rotationTestSecretName, metav1.GetOptions{})
		if err != nil || current.UID != secret.UID || !bytes.Equal(current.Data[rotationTestDataKey], newKey) ||
			string(current.Data["unrelated"]) != "preserve" || current.Labels["example.test/preserve"] != "true" {
			t.Fatalf("ambiguous update reconciliation lost identity or unrelated state: current=%#v err=%v",
				current, err)
		}
	})
}

func TestNodeReadyWithKeyHash(t *testing.T) {
	const expectedHash = "replacement-key-hash"

	tests := []struct {
		name         string
		node         *corev1.Node
		expectedHash string
		want         bool
	}{
		{
			name: "Ready with replacement key hash",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
			want:         true,
		},
		{
			name: "not Ready",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}),
			expectedHash: expectedHash,
		},
		{
			name: "wrong key hash",
			node: newNodeWithKeyHash("old-key-hash",
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
		},
		{
			name:         "missing Ready condition",
			node:         newNodeWithKeyHash(expectedHash),
			expectedHash: expectedHash,
		},
		{
			name: "ignores other conditions",
			node: newNodeWithKeyHash(expectedHash,
				corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
				corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}),
			expectedHash: expectedHash,
			want:         true,
		},
		{
			name:         "empty expected hash",
			node:         newNodeWithKeyHash(expectedHash),
			expectedHash: "",
		},
		{
			name:         "nil node",
			expectedHash: expectedHash,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nodeReadyWithKeyHash(test.node, test.expectedHash); got != test.want {
				t.Fatalf("nodeReadyWithKeyHash() = %t, want %t", got, test.want)
			}
		})
	}
}

func newNodeWithKeyHash(hash string, conditions ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{pubKeyHashAnno: hash}},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}
