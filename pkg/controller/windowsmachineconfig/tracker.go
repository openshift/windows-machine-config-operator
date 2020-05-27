package windowsmachineconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	nc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/nodeconfig"
	"github.com/operator-framework/operator-sdk/pkg/k8sutil"
	"github.com/pkg/errors"
	"k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// StoreName is the name of the ConfigMap used to track Windows worker node information
const StoreName = "wmco-node-tracker"

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

// tracker is used to track the instance information
type tracker struct {
	// windowsVMs is a map of Windows VMs to be tracked by WMCO
	windowsVMs map[types.WindowsVM]bool
	// operatorNS is the name of namespace where the operator is running.
	operatorNS string
	//k8sclientset is the kubernetes client set
	k8sclientset *kubernetes.Clientset
	// nodeRecords is a map of instanceID as key and marshalled nodeRecord object
	nodeRecords map[string][]byte
}

// newTracker initializes and returns a tracker object
func newTracker(k8sclientset *kubernetes.Clientset) (*tracker, error) {
	if k8sclientset == nil {
		return nil, fmt.Errorf("cannot instantiate tracker without k8s client")
	}

	// Get the namespace the operator is currently deployed in.
	operatorNS, err := k8sutil.GetWatchNamespace()
	if err != nil {
		return nil, errors.Wrap(err, "unable to get operator namespace")
	}
	// Construct the windowsVMs map which has information of the credentials
	windowsVMs, err := initWindowsVMs(k8sclientset, operatorNS)
	if err != nil {
		return nil, errors.Wrap(err, "unable to construct the Windows VM objects from the WMCO node tracker "+
			"ConfigMap")
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

// GetWindowsVM returns a WindowsVM interface for the specified VM
func GetWindowsVM(instanceID, ipAddress string, credentials Credentials) (types.WindowsVM, error) {
	winVM := &types.Windows{}
	windowsCredentials := types.NewCredentials(instanceID, ipAddress, credentials.Password, credentials.Username)
	winVM.Credentials = windowsCredentials
	// Set up Winrm client
	if err := winVM.SetupWinRMClient(); err != nil {
		return nil, errors.Wrap(err, "error instantiating winrm client")
	}
	// Set up SSH client
	if err := winVM.GetSSHClient(); err != nil {
		return nil, errors.Wrap(err, "error instantiating ssh client")
	}
	return winVM, nil
}

// getNodeIP gets the instance IP address associated with a node
func getNodeIP(nodeList *v1.NodeList, instanceID string) (string, error) {
	// Ignore the nodes that are not ready.
	for _, node := range nodeList.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type != v1.NodeReady {
				continue
			}
		}
		if strings.Contains(node.Spec.ProviderID, instanceID) {
			for _, address := range node.Status.Addresses {
				// If external ip exists return it as it is used in AWS, if it doesn't return internal(in case of azure)
				// TODO: collect all the ips and define a priority order, external then internal etc.
				// https://issues.redhat.com/browse/WINC-355?focusedCommentId=14100301&page=com.atlassian.jira.plugin.system.issuetabpanels%3Acomment-tabpanel#comment-14100301
				if address.Type == v1.NodeExternalIP && len(address.Address) > 0 {
					return address.Address, nil
				}
			}
		}
	}
	return "", errors.Errorf("unable to find Windows Worker node for VM with instance ID %s", instanceID)
}

// addWindowsVM adds a new windows VM to the tracker
func (t *tracker) addWindowsVM(windowsVM types.WindowsVM) {
	if _, ok := t.windowsVMs[windowsVM]; !ok {
		t.windowsVMs[windowsVM] = true
	}
}

// deleteWindowsVM deletes the given VM from the tracker
func (t *tracker) deleteWindowsVM(windowsVM types.WindowsVM) {
	delete(t.windowsVMs, windowsVM)
}

// chooseRandomNode chooses one of the windows nodes randomly. The randomization is coming from golang maps since you
// cannot assume the maps to be ordered.
func (t *tracker) chooseRandomNode() types.WindowsVM {
	for windowsVM := range t.windowsVMs {
		return windowsVM
	}
	return nil
}

// getCredsFromSecret gets the credentials associated with the instance.
func getCredsFromSecret(k8sclientset *kubernetes.Clientset, instanceID string, operatorNS string) (Credentials, error) {
	creds := Credentials{}
	instanceSecret, err := k8sclientset.CoreV1().Secrets(operatorNS).Get(instanceID, metav1.GetOptions{})
	if err != nil && k8serrors.IsNotFound(err) {
		return creds, errors.Wrapf(err, "secret asociated with instance %s not found", instanceID)
	}
	if err != nil {
		return creds, errors.Wrap(err, "error while getting secret")
	}
	encodedCreds, ok := instanceSecret.Data[instanceID]
	if !ok {
		return creds, errors.Wrap(err, "instance secret is not present")
	}
	if err := json.Unmarshal(encodedCreds, &creds); err != nil {
		return creds, errors.Wrap(err, "unmarshalling creds failed")
	}
	return creds, err
}

// initWindowsVMs initializes the windowsVMs map from the store configmMap and corresponding secrets. If the ConfigMap
// is changed or if it is having stale entry, we should still go ahead and use it.
func initWindowsVMs(k8sclientset *kubernetes.Clientset, operatorNS string) (map[types.WindowsVM]bool, error) {
	windowsVMs := make(map[types.WindowsVM]bool)
	store, err := k8sclientset.CoreV1().ConfigMaps(operatorNS).Get(StoreName, metav1.GetOptions{})
	// It is ok if the tracker ConfigMap doesn't exist. It is usually the case when the operator starts
	// for the first time but it is not ok when the tracker ConfigMap exists but it unqueryable for some reason
	if err != nil && !k8serrors.IsNotFound(err) {
		return nil, errors.Wrapf(err, "unable to query %s/%s ConfigMap", operatorNS, StoreName)
	}
	if err != nil && k8serrors.IsNotFound(err) {
		log.Info(" Skipping VM map initialization as tracker does not exist")
		return windowsVMs, nil
	}
	// Get all the Windows nodes in the cluster
	nodeList, err := k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: nc.WindowsOSLabel})
	if err != nil {
		return nil, errors.Wrap(err, "error while querying for Windows nodes")
	}
	for instanceID := range store.BinaryData {
		ipAddress, err := getNodeIP(nodeList, instanceID)
		if err != nil {
			// As of now, we're doing best effort to reconstruct the ConfigMap, so ignore errors while getting them
			// from ConfigMap. Having said that, the ConfigMap entries should be perfect. Please look at syncNodeRecords
			// method, where we disallow populating ConfigMap with empty IPAddress, InstanceID and credentials to access
			// the Windows VM
			log.Error(err, "error getting external ip", "instance", instanceID)
			continue
		}
		credentials, err := getCredsFromSecret(k8sclientset, instanceID, operatorNS)
		if err != nil {
			return nil, errors.Wrapf(err, "error getting credentials from the secret for node %s", instanceID)
		}
		windowsVM, err := GetWindowsVM(instanceID, ipAddress, credentials)
		if err != nil {
			log.Error(err, "error constructing windowsVM object", "instance", instanceID)
			continue
		}
		// Insert the windowsVM created.
		windowsVMs[windowsVM] = true
	}
	return windowsVMs, nil
}

// Reconcile ensures that the ConfigMap used by the tracker to store VM -> node information is present on the cluster
// and is populated with nodeRecords.
func (t *tracker) Reconcile() error {
	store, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Get(StoreName, metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return errors.Wrapf(err, "unable to query %s/%s ConfigMap", t.operatorNS, StoreName)
	}
	// The ConfigMap does not exist, so we need to create it based on the WindowsVM slice
	if err != nil && k8serrors.IsNotFound(err) {
		if store, err = t.createStore(); err != nil {
			return errors.Wrapf(err, "unable to create %s/%s ConfigMap", t.operatorNS, StoreName)
		}
	}
	// sync the node records
	t.syncNodeRecords()
	// sync the secrets
	t.syncSecrets(store)
	// sync the store
	if err = t.updateStore(store); err != nil {
		return errors.Wrapf(err, "unable to update %s/%s ConfigMap", t.operatorNS, StoreName)
	}
	return nil
}

// createStore creates the store ConfigMap without any data
func (t *tracker) createStore() (*v1.ConfigMap, error) {
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
func (t *tracker) syncSecrets(cm *v1.ConfigMap) {
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

// updateStore updates the store ConfigMap with the nodeRecords
func (t *tracker) updateStore(cm *v1.ConfigMap) error {
	cm.BinaryData = t.nodeRecords
	cm, err := t.k8sclientset.CoreV1().ConfigMaps(t.operatorNS).Update(cm)
	if err != nil {
		return err
	}
	return nil
}

// syncNodeRecords syncs the node records to be used by tracker. We construct the credentials struct
// and marshall it so that nodeRecords are updated.
func (t *tracker) syncNodeRecords() {
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
			log.Info("ignoring VM with incomplete credentials", "credentials", credentials)
			continue
		}
		if t.nodeRecords != nil {
			if _, ok := t.nodeRecords[credentials.GetInstanceId()]; ok {
				log.Info("node records already exist for the given Windows VM")
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
	// We are overwriting the nodeRecords of tracker here. We'll always use ConfigMap as source of truth
	t.nodeRecords = nodeRecords
}

// createSecret creates the secret that stores the username and password from the credentials.
func (t *tracker) createSecret(credentials *types.Credentials) (string, error) {
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
	if err == nil || (err != nil && !k8serrors.IsNotFound(err)) {
		deleteOptions := &metav1.DeleteOptions{}
		if err := t.k8sclientset.CoreV1().Secrets(t.operatorNS).Delete(credentials.GetInstanceId(), deleteOptions); err != nil {
			return "", errors.Wrapf(err, "error deleting existing secret %s/%s", t.operatorNS,
				credentials.GetInstanceId())
		}
	}

	secret, err = t.k8sclientset.CoreV1().Secrets(t.operatorNS).Create(secret)
	if err != nil {
		return "", errors.Wrapf(err, "error creating secret %s/%s", t.operatorNS, credentials.GetInstanceId())
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
