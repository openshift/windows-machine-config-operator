package e2e

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/test/e2e/smb"
)

// testStorage tests that persistent volumes can be accessed by Windows pods
func testStorage(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	if !tc.StorageSupport() {
		t.Skip("storage is not supported on this platform")
	}
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, false, false)
	require.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, false, true)
	require.NoError(t, err, "timed out waiting for BYOH Windows nodes")
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")

	// Create the PVC and choose the node the deployment will be scheduled on. This is necessary as ReadWriteOnly
	// volumes can only be bound to a single node.
	// https://docs.openshift.com/container-platform/4.12/storage/understanding-persistent-storage.html#pv-access-modes_understanding-persistent-storage
	var pv *core.PersistentVolume
	if tc.CloudProvider.GetType() == config.NonePlatformType {
		err = smb.EnsureSMBControllerResources(tc.client.K8s)
		require.NoError(t, err)
		pv, err = tc.createSMBPV()
		require.NoError(t, err)
	}
	pvc, err := tc.CloudProvider.CreatePVC(tc.client.K8s, tc.workloadNamespace, pv)
	require.NoError(t, err)
	if !skipWorkloadDeletion {
		defer func() {
			err := tc.client.K8s.CoreV1().PersistentVolumeClaims(tc.workloadNamespace).Delete(context.TODO(),
				pvc.GetName(), meta.DeleteOptions{})
			if err != nil {
				log.Printf("error deleting PVC: %s", err)
			}
		}()
	}
	pvcVolumeSource := &core.PersistentVolumeClaimVolumeSource{ClaimName: pvc.GetName()}
	affinity, err := getAffinityForNode(&gc.allNodes()[0])
	require.NoError(t, err)

	// The deployment will not come to ready if the volume is not able to be attached to the pod. If the deployment is
	// successful, storage is working as expected.
	winServerDeployment, err := tc.deployWindowsWebServer("win-webserver-storage-test", affinity, pvcVolumeSource)
	assert.NoError(t, err)
	if err == nil && !skipWorkloadDeletion {
		defer func() {
			err := tc.deleteDeployment(winServerDeployment.GetName())
			if err != nil {
				log.Printf("error deleting deployment: %s", err)
			}
		}()
	}
}

// createSMBPV creates a Persistent Volume backed by an SMB share created on one of the Windows Nodes
func (tc *testContext) createSMBPV() (*core.PersistentVolume, error) {
	err := tc.loadExistingNodes()
	if err != nil {
		return nil, fmt.Errorf("error loading existing nodes: %w", err)
	}
	node := gc.allNodes()[0]
	addr, err := controllers.GetAddress(node.Status.Addresses)
	if err := tc.checkSMBPortOpen(addr); err != nil {
		return nil, fmt.Errorf("port unreachable")
	}
	username := "SMBUser"
	password := generateWindowsPassword()
	shareName := "TestShare"
	createShareCommand := fmt.Sprintf("$Password = (ConvertTo-SecureString -Force -AsPlainText '%s');"+
		"New-LocalUser -Name '%s' -Password $Password;"+
		"mkdir /smbshare;"+
		"New-SmbShare -Name '%s' -Path C:\\smbshare -FullAccess '%s'", password, username, shareName, username)
	if err != nil {
		return nil, fmt.Errorf("error getting address: %w", err)
	}
	if out, err := tc.runPowerShellSSHJob("create-smb-share", createShareCommand, addr); err != nil {
		return nil, fmt.Errorf("error creating SMB share %s: %w", out, err)
	}
	secretName := "smbcreds"
	credentialSecret := core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name: secretName,
		},
		StringData: map[string]string{"username": username, "password": password},
		Type:       "generic",
	}
	_, err = tc.client.K8s.CoreV1().Secrets(tc.workloadNamespace).Create(context.TODO(), &credentialSecret, meta.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("error creating smbcreds secret")
	}
	shareAddress := fmt.Sprintf("//%s/%s", addr, shareName)
	pv := core.PersistentVolume{
		ObjectMeta: meta.ObjectMeta{
			Name: "pv-smb",
		},
		Spec: core.PersistentVolumeSpec{
			Capacity: core.ResourceList{
				core.ResourceStorage: resource.MustParse("5Gi"),
			},
			PersistentVolumeSource: core.PersistentVolumeSource{
				CSI: &core.CSIPersistentVolumeSource{
					Driver:           "smb.csi.k8s.io",
					ReadOnly:         false,
					VolumeHandle:     "smb-vol-1",
					VolumeAttributes: map[string]string{"source": shareAddress},
					NodeStageSecretRef: &core.SecretReference{
						Name:      secretName,
						Namespace: tc.workloadNamespace,
					},
				},
			},
			AccessModes:                   []core.PersistentVolumeAccessMode{core.ReadWriteMany},
			PersistentVolumeReclaimPolicy: core.PersistentVolumeReclaimRetain,
			StorageClassName:              "smb",
			MountOptions: []string{
				"dir_mode=0777",
				"file_mode=0777",
				"uid=1001",
				"gid=1001",
				"noperm",
				"mfsymlinks",
				"cache=strict",
				"noserverino",
			},
		},
	}
	return tc.client.K8s.CoreV1().PersistentVolumes().Create(context.TODO(), &pv, meta.CreateOptions{})
}

// checkSMBPortOpen returns no error if port 445 is reachable at the given address
func (tc *testContext) checkSMBPortOpen(addr string) error {
	_, err := tc.runJob("check-smb-port-status", []string{"nc", "-v", "-z", addr, "445"})
	return err
}

// generateWindowsPassword generates a random password usable for a Windows user account
func generateWindowsPassword() string {
	r := rand.New(rand.NewSource(time.Now().UnixMilli()))
	// Ensure password requirements are hit by including a mix of lowercase letters, a number, and a special character
	passwordLength := 32
	password := make([]byte, passwordLength)
	// Pick 30 characters from the lowercase letter set
	letterSet := "abcdefghijklmnopqrstuvwxyz"
	for i := 0; i < passwordLength-2; i++ {
		password[i] = letterSet[r.Intn(len(letterSet)-1)]
	}
	// Pick one number from 0-9, and add a special character
	password[passwordLength-2] = strconv.Itoa(r.Intn(10))[0]
	password[passwordLength-1] = '-'
	return string(password)
}
