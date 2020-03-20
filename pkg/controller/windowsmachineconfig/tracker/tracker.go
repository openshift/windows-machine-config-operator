package tracker

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// StoreName is the name of the ConfigMap used to track Windows worker node information
const StoreName = "wmco-node-tracker"

var log = ctrllog.Log.WithName("tracker")

// nodeRecord holds information about a single node in the cluster. This is the information that will be placed in the
// ConfigMap with the node's cloud ID as the key to get the record.
type nodeRecord struct {
	// CredSecret holds the name of the secret for each Windows node managed by the WMCO
	CredSecret string `json:"credSecret"`
	// IPAddress of the Windows node managed by the WMCO
	IPAddress string `json:"ipAddress"`
	// Drain indicates if the Windows node has to be drained before removing the Windows VMs from cluster
	Drain bool `json:"drain"`
}

// Tracker is used to track the instance information
type Tracker struct {
	// windowsVMs is a map of Windows VMs to be tracked by WMCO
	windowsVMs map[types.WindowsVM]bool
	// operatorNS is the name of namespace where the operator is running.
	operatorNS string
	//k8sclientset is the kubernetes client set
	k8sclientset *kubernetes.Clientset
	// nodeRecords is a map of instanceID as key and marshalled nodeRecord object
	nodeRecords map[string][]byte
}

// NewTracker initializes and returns a tracker object
func NewTracker(k8sclientset *kubernetes.Clientset, windowsVMs map[types.WindowsVM]bool) (*Tracker, error) {
	if k8sclientset == nil {
		return nil, fmt.Errorf("cannot instantiate tracker without k8s client")
	}
	if windowsVMs == nil {
		return nil, fmt.Errorf("cannot instantiate tracker with a nil windowsVMs slice")
	}

	// Get the namespace the operator is currently deployed in.
	operatorNS, err := k8sutil.GetWatchNamespace()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get operator namespace")
	}

	return &Tracker{
		windowsVMs:   windowsVMs,
		operatorNS:   operatorNS,
		k8sclientset: k8sclientset,
	}, nil
}

// WindowsVMs sets WindowsVMs to be tracked by WMCO
func (t *Tracker) WindowsVMs(windowsVMs map[types.WindowsVM]bool) {
	t.windowsVMs = windowsVMs
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
func (t *Tracker) Reconcile() error {
	store, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Get(StoreName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrap(err, fmt.Sprintf("unable to query %s/%s ConfigMap", t.operatorNS, StoreName))
	}

	// The ConfigMap does not exist, so we need to create it based on the WindowsVM slice
	if err != nil && k8serrors.IsNotFound(err) {
		if store, err = t.createStore(); err != nil {
			return errors.Wrap(err, fmt.Sprintf("unable to create %s/%s ConfigMap", t.operatorNS, StoreName))
		}
	}
	// sync the node records
	t.syncNodeRecords()
	// sync the secrets
	t.syncSecrets(store)
	// sync the store
	if err = t.updateStore(store); err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to update %s/%s ConfigMap", t.operatorNS, StoreName))
	}
	return nil
}

// createStoreConfigMap creates the store ConfigMap without any data
func (t *Tracker) createStore() (*v1.ConfigMap, error) {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StoreName,
			Namespace: t.operatorNS,
		},
	}
	cm, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Create(cm)
	if err != nil {
		return nil, err
	}
	return cm, nil
}

// syncSecrets will syncSecrets associated with instances in the cloud provider
func (t *Tracker) syncSecrets(cm *v1.ConfigMap) {
	existingNodes := cm.BinaryData
	// Since the secret is created with the instanceID as it's name, we can use the same key to identify secrets
	// and delete the stale secrets associated with deleted instances.
	for name := range existingNodes {
		if _, ok := t.nodeRecords[name]; !ok {
			deleteOptions := &metav1.DeleteOptions{}
			if err := t.k8sclientset.CoreV1().Secrets(t.operatorNS).Delete(name, deleteOptions); err != nil {
				log.Error(err, "while deleting secret associated with instance")
			}
		}
	}
}

// updateStoreConfigMap updates the store ConfigMap with the nodeRecords
func (t *Tracker) updateStore(cm *v1.ConfigMap) error {
	cm.BinaryData = t.nodeRecords
	cm, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Update(cm)
	if err != nil {
		return err
	}
	return nil
}

// syncNodeRecords syncs the node records to be used by tracker. We construct the credentials struct
// and marshall it so that nodeRecords are updated.
func (t *Tracker) syncNodeRecords() {
	nodeRecords := make(map[string][]byte, len(t.windowsVMs))
	for windowsVM := range t.windowsVMs {
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
		if t.nodeRecords != nil {
			if _, ok := t.nodeRecords[credentials.GetInstanceId()]; ok {
				log.Info("Node records already exist for the given Windows VM")
				continue
			}
		}
		// TODO: See if we can wrap the secret creation in the node record creation.
		//		 This way, we'll just have sync node records which will delete the
		//		 secrets associated with them.
		// 		Jira Story: https://issues.redhat.com/browse/WINC-321
		secretName, err := t.createSecret(credentials)
		if err != nil {
			log.Error(err, "unable to create secret for VM ", "instance",
				credentials.GetInstanceId())
			continue
		}

		nodeRecord, err := json.Marshal(newNodeRecord(credentials.GetIPAddress(), secretName))
		if err != nil {
			log.Error(err, "unable to marshall node record for VM ", "instance",
				credentials.GetInstanceId())
			continue
		}
		nodeRecords[credentials.GetInstanceId()] = nodeRecord
	}
	// We are overwriting the nodeRecords of tracker here. We'll always use configmap as source of truth
	t.nodeRecords = nodeRecords
}

// createSecret creates the secret that stores the username and password from the credentials.
func (t *Tracker) createSecret(credentials *types.Credentials) (string, error) {
	creds := NewCredentials(credentials.GetUserName(), credentials.GetPassword())

	reqBodyBytes := new(bytes.Buffer)
	if err := json.NewEncoder(reqBodyBytes).Encode(creds); err != nil {
		return "", errors.Wrap(err, "error encoding the credentials struct")
	}

	credValue := reqBodyBytes.Bytes()
	credData := make(map[string][]byte, 1)
	credData[credentials.GetInstanceId()] = credValue

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentials.GetInstanceId(),
			Namespace: t.operatorNS,
		},
		Data: credData,
		Type: v1.SecretTypeOpaque,
	}

	// Get and delete the secret if it already exists.
	_, err := t.k8sclientset.CoreV1().Secrets(t.operatorNS).Get(credentials.GetInstanceId(), metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		deleteOptions := &metav1.DeleteOptions{}
		if err := t.k8sclientset.CoreV1().Secrets(t.operatorNS).Delete(credentials.GetInstanceId(), deleteOptions); err != nil {
			return "", errors.Wrap(err, fmt.Sprintf("error deleting existing secret %s/%s", t.operatorNS,
				credentials.GetInstanceId()))
		}
	}

	secret, err = t.k8sclientset.CoreV1().Secrets(t.operatorNS).Create(secret)
	if err != nil {
		return "", errors.Wrap(err, fmt.Sprintf("error creating secret %s/%s", t.operatorNS, credentials.GetInstanceId()))
	}
	return secret.GetName(), nil
}

// Credentials encapsulates the username and password of a VM. This to clearly differentiate between
// the credentials we have in WNI. This ensures that we don't have to marshall the JSON to WNI's
// Credentials struct which doesn't have JSON tags.
type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// NewCredentials returns a credentials object
func NewCredentials(username, password string) Credentials {
	return Credentials{
		Username: username,
		Password: password,
	}
}
