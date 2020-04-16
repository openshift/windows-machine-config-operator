package e2e

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	operator "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wmc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/tracker"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// waitForTrackerConfigMap waits for the Windows tracker configmap to be created with appropriate values
func (tc *testContext) waitForTrackerConfigMap() error {
	var trackerConfigMap *corev1.ConfigMap
	// timeout is a factor of the number of nodes we are dealing with as all nodes have to finish their full
	// configuration before the ConfigMap is updated.
	err := wait.Poll(tc.retryInterval, time.Duration(gc.numberOfNodes)*tc.timeout, func() (done bool, err error) {
		trackerConfigMap, err = tc.kubeclient.CoreV1().ConfigMaps(tc.namespace).Get(tracker.StoreName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("Waiting for availability of tracker configmap to be created: %s\n", tracker.StoreName)
				return false, nil
			}
			return false, err
		}
		if len(trackerConfigMap.BinaryData) == gc.numberOfNodes {
			log.Println("Tracker configmap tracking required number of configmap")
			return true, nil
		}
		log.Printf("still waiting for %d number of "+
			"Windows worker nodes to be tracked but as of now we have %d\n", gc.numberOfNodes,
			len(trackerConfigMap.BinaryData))
		return false, nil
	})
	tc.tracker = trackerConfigMap
	return err
}

// getInstanceID gets the instanceID of VM for a given cloud provider ID
// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
func getInstanceID(providerID string) string {
	providerTokens := strings.Split(providerID, "/")
	return providerTokens[len(providerTokens)-1]
}

// getInstanceIDsOfNodes returns the instanceIDs of all the Windows nodes created
func (tc *testContext) getInstanceIDsOfNodes() ([]string, error) {
	instanceIDs := make([]string, 0, len(gc.nodes))
	for _, node := range gc.nodes {
		if len(node.Spec.ProviderID) > 0 {
			instanceID := getInstanceID(node.Spec.ProviderID)
			instanceIDs = append(instanceIDs, instanceID)
		}
	}
	return instanceIDs, nil
}

// testConfigMapValidation ensures that the required configMap is created and is having appropriate
// entries
func testConfigMapValidation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	err = testCtx.waitForTrackerConfigMap()
	require.NoError(t, err, "error waiting for tracker configmap")

	// Get the instance id from the cloud provider for the windows Nodes created
	instanceIDs, err := testCtx.getInstanceIDsOfNodes()
	require.NoError(t, err, "error while getting provider specific instanceIDs")

	for _, instanceID := range instanceIDs {
		assert.Contains(t, testCtx.tracker.BinaryData, instanceID)
	}
}

// getWindowsVM returns a windowsVM interface to be used for running commands against
func (tc *testContext) getWindowsVM(ipAddress, instanceID string, credentials tracker.Credentials) (types.WindowsVM, error) {
	winVM := &types.Windows{}
	windowsCredentials := types.NewCredentials(instanceID, ipAddress, credentials.Password, credentials.Username)
	winVM.Credentials = windowsCredentials
	// Set up Winrm client
	err := winVM.SetupWinRMClient()
	if err != nil {
		return nil, errors.Wrap(err, "error instantiating winrm client")
	}
	return winVM, nil
}

// validateConnectivity ensures that we have connectivity for the Windows VM
func (tc *testContext) validateConnectivity(windowsVM types.WindowsVM) error {
	stdout, stderr, err := windowsVM.Run("dir", false)
	if err != nil {
		return errors.Wrap(err, "failed to run dir command on remote Windows VM")
	}
	if stderr != "" {
		return errors.New("test returned stderr output")
	}
	if strings.Contains(stdout, "FAIL") {
		return errors.New("test output showed a failure")
	}
	if strings.Contains(stdout, "panic") {
		return errors.New("test output showed panic")
	}
	return nil
}

// getInstanceIP gets the instance IP address associated with a node
func (tc *testContext) getInstanceIP(instanceID string) (string, error) {
	nodes, err := tc.kubeclient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: wmc.WindowsOSLabel})
	if err != nil {
		return "", errors.Wrap(err, "error while querying for Windows nodes")
	}
	for _, node := range nodes.Items {
		if strings.Contains(node.Spec.ProviderID, instanceID) {
			for _, address := range node.Status.Addresses {
				if address.Type == corev1.NodeExternalIP {
					return address.Address, nil
				}
			}
		}
	}
	return "", errors.New("unable to find Windows Worker nodes")
}

// getCredsFromSecret gets the credentials associated with the instance.
func (tc *testContext) getCredsFromSecret(instanceID string) (tracker.Credentials, error) {
	var creds tracker.Credentials
	err := wait.Poll(tc.retryInterval, tc.timeout, func() (done bool, err error) {
		instanceSecret, err := tc.kubeclient.CoreV1().Secrets(tc.namespace).Get(instanceID, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("Waiting for instance secret to be created: %s\n", instanceSecret.Name)
				return false, nil
			}
			return false, err
		}
		encodedCreds := instanceSecret.Data[instanceID]
		if err := json.Unmarshal(encodedCreds, &creds); err != nil {
			return false, errors.Wrap(err, "unmarshalling creds failed")
		}
		return true, nil
	})
	return creds, err
}

// validateInstanceSecret validates the instance secret.
func (tc *testContext) validateInstanceSecret(instanceID string) error {
	ipAddress, err := tc.getInstanceIP(instanceID)
	if err != nil {
		return err
	}
	creds, err := tc.getCredsFromSecret(instanceID)
	if err != nil {
		return err
	}
	if creds == (tracker.Credentials{}) {
		return errors.New("expected credentials to be present but got a nil value")
	}
	windowsVM, err := tc.getWindowsVM(ipAddress, instanceID, creds)
	if err != nil {
		return err
	}
	err = tc.validateConnectivity(windowsVM)
	if err != nil {
		return err
	}
	gc.windowsVMs = append(gc.windowsVMs, testVM{windowsVM})
	return nil
}

// testValidateSecrets ensures we've valid secrets in place to be used by trackerConfigmap to construct node objects
func testValidateSecrets(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// Get the instance id from the cloud provider for the windows Nodes created
	instanceIDs, err := testCtx.getInstanceIDsOfNodes()
	require.NoError(t, err, "error while getting instance ids")
	require.Equal(t, len(instanceIDs), gc.numberOfNodes, "mismatched node count")

	// Reset the windowsVMs to avoid staleness
	gc.windowsVMs = make([]testVM, 0, numberOfNodes)

	for _, instanceID := range instanceIDs {
		err := testCtx.validateInstanceSecret(instanceID)
		assert.NoError(t, err, "error validating instance secret")
	}
}

// testWMCValidation tests if validations of the fields of WindowsMachineConfigs CRD are working as expected
// We are only checking negative test cases here, positive test cases would check if custom resource is getting created
// as expected and they are handled in testWindowsNodeCreation function in test/e2e/create_test.go
func testWMCValidation(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	require.NoError(t, err, "Could not fetch a namespace")

	var wmcReplicasFieldValidationTests = []struct {
		name                       string
		wmc                        *operator.WindowsMachineConfig
		isTestExpectedToThrowError bool
		expectedErrorInTest        string
	}{
		{
			name:                       "replicas field absent",
			wmc:                        createWindowsMachineConfig(testCtx.namespace, false, 0),
			isTestExpectedToThrowError: false,
			expectedErrorInTest:        "",
		},
		{
			name:                       "replicas field value less than 0",
			wmc:                        createWindowsMachineConfig(testCtx.namespace, true, -1),
			isTestExpectedToThrowError: true,
			expectedErrorInTest:        "spec.replicas in body should be greater than or equal to 0",
		},
	}

	for _, test := range wmcReplicasFieldValidationTests {
		t.Run(test.name, func(t *testing.T) {
			// create WMC custom resource as per the test requirement
			err = framework.Global.Client.Create(context.TODO(), test.wmc,
				&framework.CleanupOptions{TestContext: testCtx.osdkTestCtx, Timeout: cleanupTimeout, RetryInterval: cleanupRetryInterval})

			if test.isTestExpectedToThrowError {
				require.Error(t, err, "Creation of WMC custom resource did not throw an error when it was expected to")
				assert.Contains(t, err.Error(), test.expectedErrorInTest,
					"Creation of WMC custom resource threw an unexpected error")
			} else {
				require.NoError(t, err, "Creation of the WMC custom resource threw an error when it was expected not to")
				// Fetching WMC persisted in etcd and checking if replicas field value is initialized as expected
				actualWMC := &operator.WindowsMachineConfig{}
				err = framework.Global.Client.Get(context.TODO(),
					kubeTypes.NamespacedName{Name: wmcCRName, Namespace: testCtx.namespace}, actualWMC)
				require.NoError(t, err, "Could not get the WMC custom resource")
				assert.Equal(t, test.wmc.Spec.Replicas, actualWMC.Spec.Replicas, "Replicas value of the  WMC custom "+
					"resource is not as expected")
			}
		})
	}
}

// createWindowsMachineConfig creates a WindowsMachineConfig object
func createWindowsMachineConfig(namespace string, isReplicasFieldRequired bool, replicasFieldValue int) *operator.WindowsMachineConfig {
	wmc := &operator.WindowsMachineConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WindowsMachineConfig",
			APIVersion: "wmc.openshift.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      wmcCRName,
			Namespace: namespace,
		},
		Spec: operator.WindowsMachineConfigSpec{
			InstanceType: instanceType,
			AWS:          &operator.AWS{CredentialAccountID: credentialAccountID, SSHKeyPair: gc.sshKeyPair},
		},
	}
	if isReplicasFieldRequired {
		wmc.Spec.Replicas = replicasFieldValue
	}
	return wmc
}
