package e2e

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"testing"
	"time"

	framework "github.com/operator-framework/operator-sdk/pkg/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
	nc "github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/nodeconfig"
)

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

// testWorkerLabel tests if the worker label has been applied properly
func testWorkerLabel(t *testing.T) {
	testCtx := framework.NewTestCtx(t)
	defer testCtx.Cleanup()
	for _, node := range gc.nodes {
		assert.Contains(t, node.Labels, nc.WorkerLabel, "expected node label %s was not present on %s", nc.WorkerLabel, node.GetName())
	}
}

// getWMCOVersion returns the version of the operator. This is sourced from the WMCO binary used to create the operator image.
// This function will return an error if the binary is missing.
func getWMCOVersion() (string, error) {
	cmd := exec.Command(wmcoPath, "version")
	out, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "error running %s", cmd.String())
	}
	// out is formatted like:
	// ./build/_output/bin/windows-machine-config-operator version: "0.0.1+4165dda-dirty", go version: "go1.13.7 linux/amd64"
	versionSplit := strings.Split(string(out), "\"")
	if len(versionSplit) < 3 {
		return "", fmt.Errorf("unexpected version output")
	}
	return versionSplit[1], nil
}

// testVersionAnnotation tests all nodes are annotated with the version of the currently deployed WMCO
func testVersionAnnotation(t *testing.T) {
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err, "could not get WMCO version")
	for _, node := range gc.nodes {
		t.Run(node.GetName(), func(t *testing.T) {
			require.Containsf(t, node.Annotations, nc.VersionAnnotation, "node %s missing version annotation", node.GetName())
			assert.Equal(t, operatorVersion, node.Annotations[nc.VersionAnnotation], "WMCO version annotation mismatch")
		})
	}
}

// testNodeTaint tests if the Windows node has the Windows taint
func testNodeTaint(t *testing.T) {
	// windowsTaint is the taint that needs to be applied to the Windows node
	windowsTaint := core.Taint{
		Key:    "os",
		Value:  "Windows",
		Effect: core.TaintEffectNoSchedule,
	}

	for _, node := range gc.nodes {
		hasTaint := func() bool {
			for _, taint := range node.Spec.Taints {
				if taint.Key == windowsTaint.Key && taint.Value == windowsTaint.Value && taint.Effect == windowsTaint.Effect {
					return true
				}
			}
			return false
		}()
		assert.Equal(t, hasTaint, true, "expected Windows Taint to be present on the Node: %s", node.GetName())
	}
}

// createSigner creates a signer using the private key retrieved from the secret
func createSigner() (ssh.Signer, error) {
	privateKeySecret := &core.Secret{}
	err := framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "cloud-private-key", Namespace: "openshift-windows-machine-config-operator"}, privateKeySecret)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve cloud private key secret")
	}

	privateKeyBytes := privateKeySecret.Data[secrets.PrivateKeySecretKey][:]
	if privateKeyBytes == nil {
		return nil, errors.New("failed to retrieve private key using cloud private key secret")
	}

	signer, err := ssh.ParsePrivateKey(privateKeyBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse private key")
	}
	return signer, nil
}

// testUserData tests if the userData created in the 'openshift-machine-api' namespace is valid
func testUserData(t *testing.T) {
	signer, err := createSigner()
	require.NoError(t, err, "error creating signer using private key")
	pubKeyBytes := ssh.MarshalAuthorizedKey(signer.PublicKey())
	require.NotNil(t, pubKeyBytes, "failed to retrieve public key using signer for private key")
	found := &core.Secret{}
	err = framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data", Namespace: "openshift-machine-api"}, found)
	require.NoError(t, err, "could not find Windows user data secret in required namespace")
	assert.Contains(t, string(found.Data["userData"][:]), string(pubKeyBytes[:]), "expected user data not present in required namespace")
}

// testUserDataTamper tests if userData reverts to previous value if updated
func testUserDataTamper(t *testing.T) {
	secretInstance := &core.Secret{}
	validUserDataSecret, err := framework.Global.KubeClient.CoreV1().Secrets("openshift-machine-api").Get(context.TODO(), "windows-user-data", meta.GetOptions{})
	require.NoError(t, err, "could not find Windows userData secret in required namespace")

	var tests = []struct {
		name           string
		operation      string
		expectedSecret *core.Secret
	}{
		{"Update the userData secret with invalid data", "Update", validUserDataSecret},
		{"Delete the userData secret", "Delete", validUserDataSecret},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.operation == "Update" {
				updatedSecret := validUserDataSecret.DeepCopy()
				updatedSecret.Data["userData"] = []byte("invalid data")
				err := framework.Global.Client.Update(context.TODO(), updatedSecret)
				require.NoError(t, err, "could not update userData secret")
			}
			if tt.operation == "Delete" {
				err := framework.Global.KubeClient.CoreV1().Secrets("openshift-machine-api").Delete(context.TODO(), "windows-user-data", meta.DeleteOptions{})
				require.NoError(t, err, "could not delete userData secret")
			}

			// wait for userData secret creation / update to take effect.
			err := wait.Poll(5*time.Second, 20*time.Second, func() (done bool, err error) {
				err = framework.Global.Client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data",
					Namespace: "openshift-machine-api"}, secretInstance)
				if err != nil {
					if apierrors.IsNotFound(err) {
						log.Printf("still waiting for user data secret: %v", err)
						return false, nil
					}
					log.Printf("error listing secrets: %v", err)
					return false, nil
				}
				if string(validUserDataSecret.Data["userData"][:]) != string(secretInstance.Data["userData"][:]) {
					return false, nil
				}
				return true, nil
			})
			require.NoError(t, err, "could not find a valid userData secret in the namespace : %v", secretInstance.Namespace)
		})
	}
}
