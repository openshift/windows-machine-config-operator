package tracker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// storeName is the name of the ConfigMap used to track Windows worker node information
const storeName = "wmco-node-tracker"

var log = logf.Log.WithName("tracker")

// nodeRecord holds information about a single node in the cluster. This is the information that will be placed in the
// ConfigMap with the node's cloud ID as the key to get the record.
type nodeRecord struct {
	CredSecret string `json:"credSecret"`
	IPAddress  string `json:"ipAddress"`
	Drain      bool   `json:"drain"`
}

// tracker is used to track the instance information
type tracker struct {
	windowsVMs   map[types.WindowsVM]bool
	operatorNS   string
	k8sclientset *kubernetes.Clientset
}

// NewTracker initializes and returns a tracker object
// NOT TESTED
func NewTracker(k8sclientset *kubernetes.Clientset, windowsVMs map[types.WindowsVM]bool) (*tracker, error) {
	if k8sclientset == nil {
		return nil, fmt.Errorf("cannot instantiate tracker without k8s client")
	}
	if windowsVMs == nil {
		return nil, fmt.Errorf("cannot instantiate tracker with a nil windowsVMs slice")
	}

	// Get the namespace the operator is currently deployed in.
	operatorNS, err := k8sutil.GetOperatorNamespace()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get operator namespace")
	}

	return &tracker{
		windowsVMs:   windowsVMs,
		operatorNS:   operatorNS,
		k8sclientset: k8sclientset,
	}, nil
}

// newNodeRecord initializes and returns a nodeRecord object
func newNodeRecord(ipAddress string, secretName string) nodeRecord {
	return nodeRecord{
		IPAddress:  ipAddress,
		CredSecret: secretName,
		Drain:      false,
	}
}

// Reconcile ensures that the ConfigMap used by the tracker to store VM -> node information is present on the cluster
// and is populated with nodeRecords.
// NOT TESTED
func (t *tracker) Reconcile() error {
	store, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Get(storeName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(err, fmt.Sprintf("unable to query %s/%s ConfigMap", t.operatorNS, storeName))
	}

	// The ConfigMap does not exist, so we need to create it based on the WindowsVM slice
	if err != nil && k8serrors.IsNotFound(err) {
		if store, err = t.createStoreConfigMap(); err != nil {
			return errors.Wrap(err, fmt.Sprintf("unable to create %s/%s ConfigMap", t.operatorNS, storeName))
		}
	}

	if err = t.updateStoreConfigMap(store, t.createNodeRecords()); err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to update %s/%s ConfigMap", t.operatorNS, storeName))
	}
	return nil
}

// createStoreConfigMap creates the store ConfigMap without any data
// NOT TESTED
func (t *tracker) createStoreConfigMap() (*v1.ConfigMap, error) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      storeName,
			Namespace: t.operatorNS,
		},
	}
	cm, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Create(cm)
	if err != nil {
		return nil, err
	}
	return cm, nil
}

// updateStoreConfigMap updates the store ConfigMap with the nodeRecords
// NOT TESTED
func (t *tracker) updateStoreConfigMap(cm *v1.ConfigMap, nodeRecords map[string][]byte) error {
	cm.BinaryData = nodeRecords
	cm, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Update(cm)
	if err != nil {
		return err
	}
	return nil
}

// createNodeRecords creates a map of marshaled nodeRecords that can be stored in the ConfigMap data
// TODO: We might want to think about creating the marshaled data separately to allow for unit testing
// NOT TESTED
func (t *tracker) createNodeRecords() map[string][]byte {
	nodeRecords := make(map[string][]byte, len(t.windowsVMs))
	for windowsVM, _ := range t.windowsVMs {
		if windowsVM == nil {
			log.Info("ignoring nil entry in windowsVMs")
			continue
		}
		credentials := windowsVM.GetCredentials()
		if credentials == nil {
			log.Info("ignoring VM with nil credentials")
			continue
		}
		if credentials.GetInstanceId() == "" || credentials.GetIPAddress() == "" || credentials.GetPassword() == "" ||
			credentials.GetUserName() == "" {
			log.Info("ignoring VM with incomplete credentials: %v", credentials)
			continue
		}

		secretName, err := t.createSecret(credentials)
		if err != nil {
			log.Info("unable to create secret for VM %s: %v", credentials.GetInstanceId(), err)
			continue
		}

		nodeRecord, err := json.Marshal(newNodeRecord(credentials.GetIPAddress(), secretName))
		if err != nil {
			log.Info("unable to marshall node record for VM %s: %v", credentials.GetInstanceId(), err)
		}
		nodeRecords[credentials.GetInstanceId()] = nodeRecord
	}
	return nodeRecords
}

// createSecret creates the secret that stores the username and password from the credentials as base64 encoded data
// NOT TESTED
func (t *tracker) createSecret(credentials *types.Credentials) (string, error) {
	creds := newCredentials(credentials.GetUserName(), credentials.GetPassword())
	secretCreds, err := creds.encode()
	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("error encoding credentials %v", credentials))
	}

	credData := make(map[string][]byte, 1)
	credData[credentials.GetInstanceId()] = secretCreds.data

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentials.GetInstanceId(),
			Namespace: t.operatorNS,
		},
		Data: credData,
		Type: v1.SecretTypeOpaque,
	}

	// TODO: Get and delete the secret if it already exists.
	secret, err = t.k8sclientset.CoreV1().Secrets(t.operatorNS).Create(secret)
	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("error creating secret %s/%s", t.operatorNS, secret))
	}
	return secret.GetName(), nil
}

// Note: please add all credentials related methods in this section

// credentials encapsulates the username and password of a VM
type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// newCredentials returns a credentials object
func newCredentials(username, password string) credentials {
	return credentials{
		Username: username,
		Password: password,
	}
}

// encode marshals the credentials into JSON and base64 encodes it
func (c credentials) encode() (encodedCredentials, error) {
	creds, err := json.Marshal(c)
	if err != nil {
		return encodedCredentials{}, errors.Wrap(err, fmt.Sprintf("error marshaling credentials: %v", err))
	}

	encodedCreds := make([]byte, base64.StdEncoding.EncodedLen(len(creds)))
	// If we want to GA with this approach, we would need to figure out a way to encrypt it.
	base64.StdEncoding.Encode(encodedCreds, creds)
	return newEncodedCredentials(encodedCreds), nil
}

// Note: please add all encodedCredentials related methods in this section

// encodedCredentials holds base64 encoded data
type encodedCredentials struct {
	data []byte
}

// newEncodedCredentials returns a encodedCredentials object
func newEncodedCredentials(data []byte) encodedCredentials {
	return encodedCredentials{
		data: data,
	}
}

// decode decodes the base64 encoded data and unmarshals the JSON into a credentials object
func (e encodedCredentials) decode() (credentials, error) {
	if e.data == nil {
		return credentials{}, fmt.Errorf("encoded data was nil")
	}

	decodedData := make([]byte, base64.StdEncoding.DecodedLen(len(e.data)))
	_, err := base64.StdEncoding.Decode(decodedData, e.data)
	if err != nil {
		return credentials{}, errors.Wrap(err, fmt.Sprintf("error decoding encoded credentials"))
	}

	var creds credentials
	err = json.Unmarshal(decodedData, &creds)
	if err != nil {
		return credentials{}, errors.Wrap(err, fmt.Sprintf("error unmarshaling encoded credentials"))
	}
	return creds, nil
}
