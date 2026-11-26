package winc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

const (
	privateKeySecret      = "cloud-private-key"
	privateKeySecretKey   = "private-key.pem"
	usernameAnno          = "windowsmachineconfig.openshift.io/username"
	operatorConditionEnv  = "OPERATOR_CONDITION_NAME"
	usernameMarker        = "<wmcoMarker>"
	usernameArmorTag      = "ENCRYPTED DATA"
	usernameArmorStartTag = "-----BEGIN " + usernameArmorTag + "-----"
	usernameArmorEndTag   = "-----END " + usernameArmorTag + "-----"
	keyRotationTimeout    = 10 * time.Minute
)

var operatorConditionsGVR = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v2", Resource: "operatorconditions"}

// byohKeyRotation replaces the WMCO private key for one reserved BYOH host and restores it. It never
// mutates any host other than the one reserved by the supplied lifecycle.
type byohKeyRotation struct {
	core        corev1client.CoreV1Interface
	lifecycle   *suppliedBYOHLifecycle
	original    []byte
	originalUID types.UID
	replacement []byte
	// rotated is set before the replacement write is attempted, so an uncertain write is still restored.
	rotated bool
	// restored is set only after the original key is confirmed on every BYOH Node.
	restored      bool
	upgradeable   func(context.Context) (string, string, error)
	registrations func(context.Context) (map[string]string, error)
}

// dispose clears the key material this rotation owns. It must run after the last restore attempt.
func (r *byohKeyRotation) dispose() {
	clear(r.original)
	clear(r.replacement)
}

func newBYOHKeyRotation(ctx context.Context, core corev1client.CoreV1Interface, apps appsv1client.AppsV1Interface,
	dyn dynamic.Interface, lifecycle *suppliedBYOHLifecycle) (*byohKeyRotation, error) {
	secret, err := core.Secrets(wmcoNamespace).Get(ctx, privateKeySecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", privateKeySecret, err)
	}
	original := secret.Data[privateKeySecretKey]
	if secret.UID == "" || len(original) == 0 {
		return nil, fmt.Errorf("%s has no UID or no %s data", privateKeySecret, privateKeySecretKey)
	}
	replacement, err := newRSAPrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	r := &byohKeyRotation{core: core, lifecycle: lifecycle, original: bytes.Clone(original), originalUID: secret.UID, replacement: replacement}
	r.upgradeable = func(ctx context.Context) (string, string, error) {
		return operatorUpgradeable(ctx, apps, dyn)
	}
	r.registrations = r.registeredUsernames
	return r, nil
}

// newRSAPrivateKeyPEM returns a PEM-encoded 2048-bit RSA key in the format WMCO accepts.
func newRSAPrivateKeyPEM() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
}

// authorizedKeyFromPrivateKey returns the single-line authorized_keys entry for a private key.
func authorizedKeyFromPrivateKey(privateKey []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return "", fmt.Errorf("parse private key: %w", err)
	}
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(signer.PublicKey())), "\n"), nil
}

// publicKeyHashFromPrivateKeyBytes returns the value WMCO places in the public key hash node annotation.
func publicKeyHashFromPrivateKeyBytes(privateKey []byte) (string, error) {
	authorizedKey, err := authorizedKeyFromPrivateKey(privateKey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(authorizedKey))), nil
}

// secretEqual compares key material without leaking the first differing byte.
func secretEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// decryptUsernameAnnotation mirrors pkg/crypto.DecryptFromJSONString, which the OTE module cannot import.
func decryptUsernameAnnotation(annotation string, privateKey []byte) (string, error) {
	if annotation == "" || len(privateKey) == 0 {
		return "", fmt.Errorf("username annotation and private key are required")
	}
	encrypted := strings.ReplaceAll(annotation, usernameMarker, "\n")
	if !strings.HasPrefix(encrypted, usernameArmorStartTag) {
		encrypted = usernameArmorStartTag + "\n\n" + encrypted + "\n" + usernameArmorEndTag
	}
	block, err := armor.Decode(strings.NewReader(encrypted))
	if err != nil {
		return "", fmt.Errorf("decode username annotation: %w", err)
	}
	attempted := false
	prompt := func([]openpgp.Key, bool) ([]byte, error) {
		if attempted {
			return nil, fmt.Errorf("invalid passphrase supplied")
		}
		attempted = true
		return privateKey, nil
	}
	message, err := openpgp.ReadMessage(block.Body, nil, prompt, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt username annotation: %w", err)
	}
	plaintext, err := io.ReadAll(message.UnverifiedBody)
	if err != nil {
		return "", fmt.Errorf("read decrypted username annotation: %w", err)
	}
	return string(plaintext), nil
}

// authorizeKeyCommand appends an authorized key for the administrators group and keeps every existing key, so
// WMCO can reach the host with either the original or the replacement key.
func authorizeKeyCommand(authorizedKey string) (string, error) {
	fields := strings.Fields(authorizedKey)
	if len(fields) != 2 || fields[0] != "ssh-rsa" || strings.ContainsAny(authorizedKey, "'\"`$\r\n") {
		return "", fmt.Errorf("refusing to authorize a malformed public key")
	}
	return `$ProgressPreference='SilentlyContinue';
$path=Join-Path $env:ProgramData 'ssh\administrators_authorized_keys';
$key='` + authorizedKey + `';
if(!(Test-Path -LiteralPath $path -PathType Leaf)){Write-Output 'BYOH_AUTHORIZED_KEYS_MISSING';exit 1};
if(!(Get-Content -LiteralPath $path | Where-Object { $_ -ceq $key })){Add-Content -LiteralPath $path -Value $key -Encoding ascii};
if(!(Get-Content -LiteralPath $path | Where-Object { $_ -ceq $key })){Write-Output 'BYOH_KEY_NOT_AUTHORIZED';exit 1};
Write-Output 'BYOH_OK'`, nil
}

// machineBackedWindowsNodes returns Windows Nodes that WMCO would recreate on a private key change.
func machineBackedWindowsNodes(nodes []corev1.Node) []string {
	var names []string
	for _, node := range nodes {
		if node.Labels["kubernetes.io/os"] == "windows" && node.Labels[byohLabel] != "true" {
			names = append(names, node.Name)
		}
	}
	return names
}

// byohNodesKeyMismatches returns a description of every BYOH Node whose public key hash or username annotation does
// not match the given private key. usernames maps registered addresses to their expected usernames.
func byohNodesKeyMismatches(nodes []corev1.Node, privateKey []byte, usernames map[string]string) ([]string, error) {
	hash, err := publicKeyHashFromPrivateKeyBytes(privateKey)
	if err != nil {
		return nil, err
	}
	var mismatches []string
	for _, node := range nodes {
		if node.Labels[byohLabel] != "true" {
			continue
		}
		expected := ""
		for _, address := range node.Status.Addresses {
			if username, ok := usernames[address.Address]; ok {
				expected = username
				break
			}
		}
		switch {
		case expected == "":
			mismatches = append(mismatches, node.Name+": no matching windows-instances registration")
		case node.Annotations[pubKeyHashAnno] != hash:
			mismatches = append(mismatches, node.Name+": public key hash does not match")
		default:
			username, err := decryptUsernameAnnotation(node.Annotations[usernameAnno], privateKey)
			if err != nil {
				mismatches = append(mismatches, node.Name+": username annotation does not decrypt with the key")
			} else if username != expected {
				mismatches = append(mismatches, node.Name+": decrypted username does not match the registration")
			}
		}
	}
	return mismatches, nil
}

// upgradeableConditionStatus returns the Upgradeable status recorded in an OperatorCondition spec and status.
// Missing conditions return an empty status and no error so callers can keep polling.
func upgradeableConditionStatus(obj *unstructured.Unstructured) (string, string, error) {
	find := func(fields ...string) (string, error) {
		conditions, found, err := unstructured.NestedSlice(obj.Object, fields...)
		if err != nil {
			return "", fmt.Errorf("read %v conditions: %w", fields, err)
		}
		if !found {
			return "", nil
		}
		for _, item := range conditions {
			condition, ok := item.(map[string]interface{})
			if ok && condition["type"] == "Upgradeable" {
				status, _ := condition["status"].(string)
				return status, nil
			}
		}
		return "", nil
	}
	spec, err := find("spec", "conditions")
	if err != nil {
		return "", "", err
	}
	status, err := find("status", "conditions")
	if err != nil {
		return "", "", err
	}
	return spec, status, nil
}

func operatorUpgradeable(ctx context.Context, apps appsv1client.AppsV1Interface, dyn dynamic.Interface) (string, string, error) {
	deployment, err := apps.Deployments(wmcoNamespace).Get(ctx, wmcoDeploymentName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("read WMCO deployment: %w", err)
	}
	name := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == operatorConditionEnv {
				name = env.Value
			}
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("WMCO deployment has no %s; OLM installation is required", operatorConditionEnv)
	}
	obj, err := dyn.Resource(operatorConditionsGVR).Namespace(wmcoNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("read OperatorCondition %s: %w", name, err)
	}
	return upgradeableConditionStatus(obj)
}

func (r *byohKeyRotation) registeredUsernames(ctx context.Context) (map[string]string, error) {
	cm, err := r.core.ConfigMaps(wmcoNamespace).Get(ctx, suppliedInstances, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	usernames := make(map[string]string, len(cm.Data))
	for address, value := range cm.Data {
		username, ok := strings.CutPrefix(value, "username=")
		if !ok || username == "" {
			return nil, fmt.Errorf("invalid windows-instances registration for %s", address)
		}
		usernames[address] = username
	}
	return usernames, nil
}

// verifyPreconditions refuses to rotate when a key change would recreate Machine-backed Windows Nodes, which the
// dedicated BYOH lane must not disturb, or when the BYOH Nodes do not already reflect the original key.
func (r *byohKeyRotation) verifyPreconditions(ctx context.Context) error {
	nodes, err := r.core.Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if names := machineBackedWindowsNodes(nodes.Items); len(names) != 0 {
		return fmt.Errorf("refusing key rotation; Machine-backed Windows Nodes would be recreated: %v", names)
	}
	if r.lifecycle.node == nil {
		return fmt.Errorf("key rotation requires a configured selected Node")
	}
	usernames, err := r.registrations(ctx)
	if err != nil {
		return err
	}
	mismatches, err := byohNodesKeyMismatches(nodes.Items, r.original, usernames)
	if err != nil {
		return err
	}
	if len(mismatches) != 0 {
		return fmt.Errorf("BYOH Nodes do not reflect the original key before rotation: %v", mismatches)
	}
	return nil
}

// authorizeReplacement adds the replacement public key on the selected host only.
func (r *byohKeyRotation) authorizeReplacement(ctx context.Context) error {
	authorizedKey, err := authorizedKeyFromPrivateKey(r.replacement)
	if err != nil {
		return err
	}
	command, err := authorizeKeyCommand(authorizedKey)
	if err != nil {
		return err
	}
	if _, err := r.lifecycle.checkHost(ctx, command); err != nil {
		return fmt.Errorf("authorize replacement key on selected host: %w", err)
	}
	return nil
}

func (r *byohKeyRotation) setSecret(ctx context.Context, key []byte) error {
	secret, err := r.core.Secrets(wmcoNamespace).Get(ctx, privateKeySecret, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if secret.UID != r.originalUID {
		return fmt.Errorf("%s was replaced by another actor; refusing update", privateKeySecret)
	}
	if !secretEqual(secret.Data[privateKeySecretKey], r.original) && !secretEqual(secret.Data[privateKeySecretKey], r.replacement) {
		return fmt.Errorf("%s holds an unexpected key; refusing update", privateKeySecret)
	}
	if secretEqual(secret.Data[privateKeySecretKey], key) {
		return nil
	}
	wanted := secret.DeepCopy()
	wanted.Data[privateKeySecretKey] = bytes.Clone(key)
	_, updateErr := r.core.Secrets(wmcoNamespace).Update(ctx, wanted, metav1.UpdateOptions{})
	// Read back even an error: a lost response is not proof that the update did not happen.
	current, readErr := r.core.Secrets(wmcoNamespace).Get(ctx, privateKeySecret, metav1.GetOptions{})
	if readErr == nil && current.UID == r.originalUID && secretEqual(current.Data[privateKeySecretKey], key) {
		return nil
	}
	return errors.Join(fmt.Errorf("%s update not confirmed", privateKeySecret), updateErr, readErr)
}

// waitForKey waits until every BYOH Node reflects the key and WMCO reports Upgradeable=True again.
func (r *byohKeyRotation) waitForKey(ctx context.Context, key []byte, label string) error {
	var last []string
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, keyRotationTimeout, true, func(ctx context.Context) (bool, error) {
		nodes, err := r.core.Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			last = []string{"list Nodes: " + safeKubernetesAPIError(err)}
			return false, nil
		}
		if names := machineBackedWindowsNodes(nodes.Items); len(names) != 0 {
			return false, fmt.Errorf("Machine-backed Windows Nodes appeared during key rotation: %v", names)
		}
		usernames, err := r.registrations(ctx)
		if err != nil {
			last = []string{"read registrations: " + safeKubernetesAPIError(err)}
			return false, nil
		}
		if last, err = byohNodesKeyMismatches(nodes.Items, key, usernames); err != nil || len(last) != 0 {
			return false, err
		}
		spec, status, err := r.upgradeable(ctx)
		if err != nil {
			last = []string{err.Error()}
			return false, nil
		}
		if spec != string(metav1.ConditionTrue) || status != string(metav1.ConditionTrue) {
			last = []string{fmt.Sprintf("Upgradeable spec=%q status=%q", spec, status)}
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("BYOH Nodes did not reflect the %s key: %v: %w", label, last, err)
	}
	e2e.Logf("BYOH key rotation: all BYOH Nodes reflect the %s key and WMCO is Upgradeable", label)
	return nil
}

func (r *byohKeyRotation) rotate(ctx context.Context) error {
	r.rotated = true
	if err := r.setSecret(ctx, r.replacement); err != nil {
		return err
	}
	return r.waitForKey(ctx, r.replacement, "replacement")
}

func (r *byohKeyRotation) restore(ctx context.Context) error {
	if !r.rotated {
		r.restored = true
		return nil
	}
	if err := r.setSecret(ctx, r.original); err != nil {
		return fmt.Errorf("restore %s: %w", privateKeySecret, err)
	}
	if err := r.waitForKey(ctx, r.original, "original"); err != nil {
		return err
	}
	r.rotated = false
	r.restored = true
	return nil
}
