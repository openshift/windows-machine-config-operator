package winc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// encryptUsernameForTest mirrors pkg/crypto.EncryptToJSONString to produce annotations as WMCO writes them.
func encryptUsernameForTest(t *testing.T, plaintext string, key []byte) string {
	t.Helper()
	buffer := bytes.NewBuffer(nil)
	encoder, err := armor.Encode(buffer, usernameArmorTag, nil)
	require.NoError(t, err)
	writer, err := openpgp.SymmetricallyEncrypt(encoder, key, nil, nil)
	require.NoError(t, err)
	_, err = writer.Write([]byte(plaintext))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.NoError(t, encoder.Close())
	encrypted := strings.TrimPrefix(buffer.String(), usernameArmorStartTag)
	encrypted = strings.TrimSuffix(encrypted, usernameArmorEndTag)
	encrypted = strings.Trim(encrypted, "\n")
	return strings.ReplaceAll(encrypted, "\n", usernameMarker)
}

func testPrivateKey(t *testing.T) []byte {
	t.Helper()
	key, err := newRSAPrivateKeyPEM()
	require.NoError(t, err)
	return key
}

// annotateForKey sets the annotations WMCO applies to a BYOH Node configured with the given key.
func annotateForKey(t *testing.T, node *corev1.Node, key []byte, username string) {
	t.Helper()
	hash, err := publicKeyHashFromPrivateKeyBytes(key)
	require.NoError(t, err)
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[pubKeyHashAnno] = hash
	node.Annotations[usernameAnno] = encryptUsernameForTest(t, username, key)
}

func TestDecryptUsernameAnnotation(t *testing.T) {
	key, other := testPrivateKey(t), testPrivateKey(t)
	annotation := encryptUsernameForTest(t, "Administrator", key)

	username, err := decryptUsernameAnnotation(annotation, key)
	require.NoError(t, err)
	require.Equal(t, "Administrator", username)

	legacy := usernameArmorStartTag + "\n\n" + strings.ReplaceAll(annotation, usernameMarker, "\n") + "\n" + usernameArmorEndTag
	username, err = decryptUsernameAnnotation(legacy, key)
	require.NoError(t, err)
	require.Equal(t, "Administrator", username)

	_, err = decryptUsernameAnnotation(annotation, other)
	require.Error(t, err)
	_, err = decryptUsernameAnnotation("", key)
	require.Error(t, err)
}

func TestAuthorizeKeyCommand(t *testing.T) {
	authorizedKey, err := authorizedKeyFromPrivateKey(testPrivateKey(t))
	require.NoError(t, err)
	command, err := authorizeKeyCommand(authorizedKey)
	require.NoError(t, err)
	require.Contains(t, command, "$key='"+authorizedKey+"'")
	require.Contains(t, command, "administrators_authorized_keys")
	require.Contains(t, command, "BYOH_OK")
	require.Contains(t, command, "Get-Content", "authorization must inspect the file line by line")
	require.Contains(t, command, "-ceq", "authorization must match the key as an exact line")
	require.NotContains(t, command, "Set-Content", "existing authorized keys must be kept")

	for _, malformed := range []string{"", "ssh-ed25519 AAAA", "ssh-rsa AAAA comment", "ssh-rsa AAAA'", "ssh-rsa AAAA$(x)", "ssh-rsa AAAA\nssh-rsa BBBB"} {
		_, err := authorizeKeyCommand(malformed)
		require.Error(t, err, "accepted %q", malformed)
	}
}

func TestMachineBackedWindowsNodes(t *testing.T) {
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "byoh", Labels: map[string]string{"kubernetes.io/os": "windows", byohLabel: "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "machine", Labels: map[string]string{"kubernetes.io/os": "windows"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "linux", Labels: map[string]string{"kubernetes.io/os": "linux"}}},
	}
	require.Equal(t, []string{"machine"}, machineBackedWindowsNodes(nodes))
	require.Empty(t, machineBackedWindowsNodes(nodes[:1]))
}

func TestBYOHNodesKeyMismatches(t *testing.T) {
	key, other := testPrivateKey(t), testPrivateKey(t)
	usernames := map[string]string{"192.0.2.1": "Administrator"}
	newNode := func() corev1.Node {
		node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "byoh", Labels: map[string]string{byohLabel: "true"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}}}
		annotateForKey(t, &node, key, "Administrator")
		return node
	}
	for name, tc := range map[string]struct {
		mutate func(*corev1.Node)
		want   string
	}{
		"matching":       {mutate: func(*corev1.Node) {}},
		"non-BYOH":       {mutate: func(n *corev1.Node) { n.Labels = nil; n.Annotations = nil }},
		"unregistered":   {mutate: func(n *corev1.Node) { n.Status.Addresses[0].Address = "192.0.2.9" }, want: "no matching windows-instances registration"},
		"stale hash":     {mutate: func(n *corev1.Node) { annotateForKey(t, n, other, "Administrator") }, want: "public key hash does not match"},
		"stale username": {mutate: func(n *corev1.Node) { n.Annotations[usernameAnno] = encryptUsernameForTest(t, "Administrator", other) }, want: "does not decrypt"},
		"wrong username": {mutate: func(n *corev1.Node) { n.Annotations[usernameAnno] = encryptUsernameForTest(t, "someone", key) }, want: "does not match the registration"},
	} {
		t.Run(name, func(t *testing.T) {
			node := newNode()
			tc.mutate(&node)
			mismatches, err := byohNodesKeyMismatches([]corev1.Node{node}, key, usernames)
			require.NoError(t, err)
			if tc.want == "" {
				require.Empty(t, mismatches)
				return
			}
			require.Len(t, mismatches, 1)
			require.Contains(t, mismatches[0], tc.want)
		})
	}
}

func TestUpgradeableConditionStatus(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Other", "status": "False"},
			map[string]interface{}{"type": "Upgradeable", "status": "True"},
		}},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Upgradeable", "status": "False"},
		}},
	}}
	spec, status, err := upgradeableConditionStatus(obj)
	require.NoError(t, err)
	require.Equal(t, "True", spec)
	require.Equal(t, "False", status)

	spec, status, err = upgradeableConditionStatus(&unstructured.Unstructured{Object: map[string]interface{}{}})
	require.NoError(t, err)
	require.Empty(t, spec)
	require.Empty(t, status)
}

type rotationTestFixture struct {
	rotation *byohKeyRotation
	client   *fake.Clientset
	reusable string
	checks   []string
}

// rotationFixture reserves the disposable host from lifecycleFixture, adds a reusable BYOH Node and simulates WMCO
// re-annotating every BYOH Node whenever the private key Secret changes.
func rotationFixture(t *testing.T) *rotationTestFixture {
	t.Helper()
	f, client := lifecycleFixture(t)
	ctx := context.Background()
	original, replacement := testPrivateKey(t), testPrivateKey(t)

	selected, err := client.CoreV1().Nodes().Get(ctx, f.node.Name, metav1.GetOptions{})
	require.NoError(t, err)
	annotateForKey(t, selected, original, f.username)
	_, err = client.CoreV1().Nodes().Update(ctx, selected, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.node = selected.DeepCopy()

	reusable := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "reusable-node", UID: "reusable", Labels: map[string]string{"kubernetes.io/os": "windows", byohLabel: "true"}},
		Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.1"}}}}
	annotateForKey(t, reusable, original, "Administrator")
	_, err = client.CoreV1().Nodes().Create(ctx, reusable, metav1.CreateOptions{})
	require.NoError(t, err)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: privateKeySecret, Namespace: wmcoNamespace, UID: "secret"},
		Data: map[string][]byte{privateKeySecretKey: original}}
	_, err = client.CoreV1().Secrets(wmcoNamespace).Create(ctx, secret, metav1.CreateOptions{})
	require.NoError(t, err)

	fixture := &rotationTestFixture{client: client, reusable: reusable.Name}
	f.checkHost = func(_ context.Context, command string) (string, error) {
		fixture.checks = append(fixture.checks, command)
		return "BYOH_OK", nil
	}
	fixture.rotation = &byohKeyRotation{core: client.CoreV1(), lifecycle: f, original: original, originalUID: secret.UID, replacement: replacement,
		upgradeable: func(context.Context) (string, string, error) { return "True", "True", nil }}
	fixture.rotation.registrations = fixture.rotation.registeredUsernames

	client.PrependReactor("update", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
		key := action.(ktesting.UpdateAction).GetObject().(*corev1.Secret).Data[privateKeySecretKey]
		for _, name := range []string{selected.Name, reusable.Name} {
			node, err := client.Tracker().Get(corev1.SchemeGroupVersion.WithResource("nodes"), "", name)
			if err != nil {
				continue
			}
			updated := node.(*corev1.Node).DeepCopy()
			annotateForKey(t, updated, key, "Administrator")
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("nodes"), updated, ""))
		}
		return false, nil, nil
	})
	return fixture
}

func (fx *rotationTestFixture) secretKey(t *testing.T) []byte {
	t.Helper()
	secret, err := fx.client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), privateKeySecret, metav1.GetOptions{})
	require.NoError(t, err)
	return secret.Data[privateKeySecretKey]
}

func (fx *rotationTestFixture) requireNodesMatch(t *testing.T, key []byte) {
	t.Helper()
	nodes, err := fx.client.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	usernames, err := fx.rotation.registeredUsernames(context.Background())
	require.NoError(t, err)
	mismatches, err := byohNodesKeyMismatches(nodes.Items, key, usernames)
	require.NoError(t, err)
	require.Empty(t, mismatches)
}

func TestBYOHKeyRotationRotateAndRestore(t *testing.T) {
	fx := rotationFixture(t)
	r, ctx := fx.rotation, context.Background()

	require.NoError(t, r.verifyPreconditions(ctx))
	require.NoError(t, r.authorizeReplacement(ctx))
	require.Len(t, fx.checks, 1)
	authorizedKey, err := authorizedKeyFromPrivateKey(r.replacement)
	require.NoError(t, err)
	require.Contains(t, fx.checks[0], authorizedKey)

	require.NoError(t, r.rotate(ctx))
	require.True(t, r.rotated)
	require.False(t, r.restored)
	require.Equal(t, r.replacement, fx.secretKey(t))
	fx.requireNodesMatch(t, r.replacement)

	require.NoError(t, r.restore(ctx))
	require.False(t, r.rotated)
	require.True(t, r.restored)
	require.Equal(t, r.original, fx.secretKey(t))
	fx.requireNodesMatch(t, r.original)
}

func TestBYOHKeyRotationRestoreIsNoOpBeforeRotation(t *testing.T) {
	fx := rotationFixture(t)
	fx.client.ClearActions()
	require.NoError(t, fx.rotation.restore(context.Background()))
	require.True(t, fx.rotation.restored)
	require.Empty(t, fx.client.Actions())
}

func TestBYOHKeyRotationPreconditions(t *testing.T) {
	t.Run("machine-backed node", func(t *testing.T) {
		fx := rotationFixture(t)
		machine := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "machine-node", Labels: map[string]string{"kubernetes.io/os": "windows"}}}
		_, err := fx.client.CoreV1().Nodes().Create(context.Background(), machine, metav1.CreateOptions{})
		require.NoError(t, err)
		err = fx.rotation.verifyPreconditions(context.Background())
		require.ErrorContains(t, err, "Machine-backed Windows Nodes")
	})
	t.Run("stale BYOH annotations", func(t *testing.T) {
		fx := rotationFixture(t)
		node, err := fx.client.CoreV1().Nodes().Get(context.Background(), fx.reusable, metav1.GetOptions{})
		require.NoError(t, err)
		annotateForKey(t, node, fx.rotation.replacement, "Administrator")
		_, err = fx.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.ErrorContains(t, fx.rotation.verifyPreconditions(context.Background()), "original key before rotation")
	})
	t.Run("no selected node", func(t *testing.T) {
		fx := rotationFixture(t)
		fx.rotation.lifecycle.node = nil
		require.ErrorContains(t, fx.rotation.verifyPreconditions(context.Background()), "configured selected Node")
	})
}

func TestBYOHKeyRotationRefusesForeignSecret(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Secret){
		"replaced secret": func(s *corev1.Secret) { s.UID = "foreign" },
		"unexpected key":  func(s *corev1.Secret) { s.Data[privateKeySecretKey] = []byte("foreign") },
	} {
		t.Run(name, func(t *testing.T) {
			fx := rotationFixture(t)
			secret, err := fx.client.CoreV1().Secrets(wmcoNamespace).Get(context.Background(), privateKeySecret, metav1.GetOptions{})
			require.NoError(t, err)
			mutate(secret)
			require.NoError(t, fx.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), secret, wmcoNamespace))
			fx.client.ClearActions()

			require.ErrorContains(t, fx.rotation.rotate(context.Background()), "refusing update")
			require.True(t, fx.rotation.rotated, "an attempted rotation must still be restored")
			for _, action := range fx.client.Actions() {
				require.NotEqual(t, "update", action.GetVerb(), "foreign Secret was overwritten")
			}
		})
	}
}

func TestBYOHKeyRotationConfirmsUncertainWrites(t *testing.T) {
	t.Run("lost response after applied write", func(t *testing.T) {
		fx := rotationFixture(t)
		fx.client.PrependReactor("update", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
			secret := action.(ktesting.UpdateAction).GetObject().(*corev1.Secret)
			require.NoError(t, fx.client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("secrets"), secret, wmcoNamespace))
			return true, nil, errors.New("connection reset")
		})
		require.NoError(t, fx.rotation.setSecret(context.Background(), fx.rotation.replacement))
		require.Equal(t, fx.rotation.replacement, fx.secretKey(t))
	})
	t.Run("rejected write", func(t *testing.T) {
		fx := rotationFixture(t)
		fx.client.PrependReactor("update", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("conflict")
		})
		err := fx.rotation.rotate(context.Background())
		require.ErrorContains(t, err, "update not confirmed")
		require.True(t, fx.rotation.rotated)
		require.Equal(t, fx.rotation.original, fx.secretKey(t))
	})
}
