package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudproviderapi "k8s.io/cloud-provider/api"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/patch"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

const (
	// vmConfigurationTime is the maximum amount of time expected for a Windows VM to be fully configured and ready for WMCO
	// after the hardware is provisioned.
	vmConfigurationTime = 10 * time.Minute

	machineApproverNamespace   = "openshift-cluster-machine-approver"
	machineApproverDeployment  = "machine-approver"
	machineApproverPodSelector = "app=machine-approver"
)

func creationTestSuite(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	require.NoError(t, tc.loadExistingNodes(), "error getting the current Windows nodes in the cluster")
	if !t.Run("Creation", tc.testWindowsNodeCreation) {
		// No point in running the other tests if creation failed
		return
	}
	t.Run("Node Metadata", tc.testNodeMetadata)
	t.Run("Services ConfigMap validation", tc.testServicesConfigMap)
	t.Run("Services running", tc.testExpectedServicesRunning)
	t.Run("NodeIP Arg", tc.testNodeIPArg)
	t.Run("NodeTaint validation", tc.testNodeTaint)
	t.Run("CSR Validation", tc.testCSRApproval)
	t.Run("Certificates", tc.testCertificates)
	t.Run("Node Logs", tc.testNodeLogs)
	t.Run("Metrics validation", tc.testMetrics)
	t.Run("UserData validation", tc.testUserData)
	t.Run("Kubelet priority class validation", tc.testKubeletPriorityClass)
}

// loadExistingNodes adds all configured Windows Nodes to the globalContext cache
func (tc *testContext) loadExistingNodes() error {
	byohNodes, err := tc.listFullyConfiguredWindowsNodes(true)
	if err != nil {
		return err
	}
	machineNodes, err := tc.listFullyConfiguredWindowsNodes(false)
	if err != nil {
		return err
	}
	gc.byohNodes = byohNodes
	gc.machineNodes = machineNodes
	return nil
}

// testWindowsNodeCreation tests the Windows node creation in the cluster
func (tc *testContext) testWindowsNodeCreation(t *testing.T) {
	if len(gc.machineNodes) == numberOfMachineNodes && len(gc.byohNodes) == numberOfBYOHNodes {
		t.Skip("expected nodes already exist in the cluster")
	}
	// Create a private key secret with the known private key.
	require.NoError(t, tc.createPrivateKeySecret(true), "could not create known private key secret")

	t.Run("Machine controller", tc.testMachineConfiguration)
	// BYOH creation must occur after the Machine creation, as the MachineConfiguration tests change the private key
	// multiple times, and BYOH doesnt have the functionality of rotating keys on the VMs. This would result in BYOH
	// failing the pub key annotation validation as it compares the current private key secret with the annotation.
	// TODO: Remove this dependency by rotating keys as part of https://issues.redhat.com/browse/WINC-655
	t.Run("ConfigMap controller", tc.testBYOHConfiguration)
}

// nodelessLogCollection runs a job which will print to stdout all logs in /var/log on the given instance
// these logs can will be written to the artifact directory
func (tc *testContext) nodelessLogCollection(name, address string) error {
	// recurse through all files in /var/log and print the file name and file contents
	cmd := "Get-ChildItem -Path /var/log -Recurse | ForEach-Object {Write-Output $_.FullName; Get-Content $_.FullName}"
	_, err := tc.runPowerShellSSHJob("print-logs-"+name, cmd, address)
	return err
}

// deleteWindowsInstanceConfigMap deletes the windows-instances configmap if it exists
func (tc *testContext) deleteWindowsInstanceConfigMap() error {
	err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(), wiparser.InstanceConfigMap,
		metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// createWindowsInstanceConfigMap creates a ConfigMap for the ConfigMap controller to act on, comprised of the Machines
// in the given MachineList
func (tc *testContext) createWindowsInstanceConfigMap(machines *mapi.MachineList) error {
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: wiparser.InstanceConfigMap,
		},
		Data: make(map[string]string),
	}
	for _, machine := range machines.Items {
		addr, err := controllers.GetAddress(machine.Status.Addresses)
		if err != nil {
			return fmt.Errorf("unable to get usable address: %w", err)
		}
		cm.Data[addr] = "username=" + tc.vmUsername()
	}
	_, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Create(context.TODO(), cm, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create configmap: %w", err)
	}
	return nil
}

// validateWindowsInstanceConfigMap validates the windows-instance ConfigMap
func (tc *testContext) validateWindowsInstanceConfigMap(expectedCount int) error {
	windowsInstances, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
		wiparser.InstanceConfigMap, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error retrieving ConfigMap: %s: %w", wiparser.InstanceConfigMap, err)
	}
	// check instance count
	actualCount := len(windowsInstances.Data)
	if actualCount != expectedCount {
		return fmt.Errorf("invalid BYOH instance count: %v, expected: %v", actualCount, expectedCount)
	}
	// TODO: Validate windowsInstances.Data (See https://issues.redhat.com/browse/WINC-671)
	// ConfigMap is valid, return no error
	return nil
}

// testMachineConfiguration tests that the Windows Machine controller can properly configure Machines
func (tc *testContext) testMachineConfiguration(t *testing.T) {
	if gc.numberOfMachineNodes == 0 {
		t.Skip("Machine Controller testing disabled")
	}
	_, err := tc.createWindowsMachineSet(gc.numberOfMachineNodes, false)
	require.NoError(t, err, "failed to create Windows MachineSet")
	// We need to cover the case where a user changes the private key secret before the WMCO has a chance to
	// configure the Machine. In order to simulate that case we need to wait for the MachineSet to be fully
	// provisioned and then change the key. The correct amount of nodes being configured is proof that the
	// mismatched Machine created with the mismatched key was deleted and replaced.
	// Depending on timing and configuration flakes this will either cause all Machines, or all Machines after
	// the first configured Machines to hit this scenario. This is a platform agonistic test so we run it only on
	// Azure.
	machines, err := tc.waitForWindowsMachines(int(gc.numberOfMachineNodes), "Provisioned", false)
	require.NoError(t, err, "error waiting for Windows Machines to be provisioned")
	if tc.CloudProvider.GetType() == config.AzurePlatformType {
		// Replace the known private key with a randomly generated one.
		err = tc.createPrivateKeySecret(false)
		require.NoError(t, err, "error replacing private key secret")
	}
	err = tc.waitForWindowsNodes(gc.numberOfMachineNodes, false, false, false)
	assert.NoError(t, err, "Windows node creation failed")
	tc.machineLogCollection(machines.Items)
}

// machineLogCollection makes a best effort attempt to collect logs from each Machine instance
func (tc *testContext) machineLogCollection(machines []mapi.Machine) {
	for _, machine := range machines {
		addr, err := controllers.GetAddress(machine.Status.Addresses)
		if err != nil {
			log.Printf("Machine %s does not have a valid address, unable to get logs", machine.GetName())
			continue
		}
		err = tc.nodelessLogCollection(machine.GetName(), addr)
		if err != nil {
			log.Printf("failed to collect logs from %s: %v", machine.GetName(), err)
		}
	}

}

// testBYOHConfiguration tests that the ConfigMap controller can properly configure VMs
func (tc *testContext) testBYOHConfiguration(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH testing disabled")
	}

	// For platform-agnostic infrastructure just validate the BYOH ConfigMap
	// TODO: See https://github.com/openshift/windows-machine-config-operator/pull/858#discussion_r780316359
	if tc.CloudProvider.GetType() == config.NonePlatformType {
		err := tc.validateWindowsInstanceConfigMap(int(gc.numberOfBYOHNodes))
		require.NoError(t, err, "error validating windows-instances ConfigMap")
		log.Printf("using %v BYOH instance(s) already provisioned", gc.numberOfBYOHNodes)
	} else {
		// Otherwise, provision BYOH instances with MachineSet
		err := tc.disableClusterMachineApprover()
		require.NoError(t, err, "failed to scale down Machine Approver pods")
		err = tc.provisionBYOHConfigMapWithMachineSet()
		require.NoError(t, err, "error provisioning BYOH ConfigMap with MachineSets")
	}
	// Wait for Windows worker node to become available
	t.Run("VM is configured by ConfigMap controller", func(t *testing.T) {
		err := tc.waitForWindowsNodes(gc.numberOfBYOHNodes, false, false, true)
		assert.NoError(t, err, "Windows node creation failed")
	})
	// Make a best effort attempt to collect logs from each BYOH instance
	tc.byohLogCollection()
}

// byohLogCollection kicks off the collection of logs for instances listed in the windows-instances configmap
func (tc *testContext) byohLogCollection() {
	windowsInstances, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(),
		wiparser.InstanceConfigMap, metav1.GetOptions{})
	if err != nil {
		log.Printf("unable to collect logs, error retrieving instances ConfigMap: %v", err)
		return
	}
	for addr := range windowsInstances.Data {
		// Resource names can't have the character `.` in them.
		nameSuffix := strings.ReplaceAll(addr, ".", "-")
		err = tc.nodelessLogCollection("byoh-"+nameSuffix, addr)
		if err != nil {
			log.Printf("failed to collect byoh instance logs: %v", err)
		}
	}
}

// provisionBYOHConfigMapWithMachineSet provisions BYOH instances using MachineSet and creates the `windows-instances`
// ConfigMap.
// The Cluster Machine Approver must be disabled to test BYOH CSR approval feature, so that BYOH instances CSR's are
// not approved by Cluster Machine Approver
func (tc *testContext) provisionBYOHConfigMapWithMachineSet() error {
	_, err := tc.createWindowsMachineSet(gc.numberOfBYOHNodes, true)
	if err != nil {
		return fmt.Errorf("failed to create Windows MachineSet: %w", err)
	}
	machines, err := tc.waitForWindowsMachines(int(gc.numberOfBYOHNodes), "Provisioned", true)
	if err != nil {
		return fmt.Errorf("machines did not reach expected state: %w", err)
	}
	// Change the default shell to PowerShell for the first BYOH VM. We expect there to be BYOH VMs with either
	// cmd or PowerShell as the default shell, so getting a mix of both, either between different BYOH VMs, or with the
	// VMs spun up for the Machine testing is important.
	err = tc.setPowerShellDefaultShell(&machines.Items[0])
	if err != nil {
		return fmt.Errorf("unable to change default shell of machine %s: %w", machines.Items[0].GetName(), err)
	}
	if tc.CloudProvider.GetType() == config.VSpherePlatformType {
		// By default the Windows Defender antivirus is running on VMs in the test suite. Disable it for a BYOH node to
		// validate instances are able to be configured into Nodes regardless of the Windows Defender antivirus status.
		err = tc.uninstallWindowsDefender(&machines.Items[0])
		if err != nil {
			return fmt.Errorf("unable to uninstall Windows Defender on machine %s: %w", machines.Items[0].GetName(),
				err)
		}
	}
	return tc.createWindowsInstanceConfigMap(machines)
}

// disableClusterMachineApprover disables the Cluster Machine Approver.
// This is a prerequisite to configure BYOH instances created using MachineSets
func (tc *testContext) disableClusterMachineApprover() error {
	// Patch the CVO with overrides spec value for cluster-machine-approver deployment
	// Doing so, stops CVO from creating/updating its deployment hereafter.
	nodeCSRApproverOverride := config.ComponentOverride{
		Kind:      "Deployment",
		Group:     "apps",
		Namespace: "openshift-cluster-machine-approver",
		Name:      "machine-approver",
		Unmanaged: true,
	}
	patchData, err := json.Marshal([]*patch.JSONPatch{
		patch.NewJSONPatch("add", "/spec/overrides", []config.ComponentOverride{nodeCSRApproverOverride})})
	if err != nil {
		return fmt.Errorf("unable to generate patch request body for CVO override: %v: %w", nodeCSRApproverOverride,
			err)
	}

	_, err = tc.client.Config.ConfigV1().ClusterVersions().Patch(context.TODO(), "version", types.JSONPatchType,
		patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("unable to apply patch %s to ClusterVersion: %w", patchData, err)
	}

	// Scale the Cluster Machine Approver Deployment to 0
	// This is required for testing BYOH CSR approval feature so that BYOH instances
	// CSR's are not approved by Cluster Machine Approver
	expectedPodCount := int32(0)
	return tc.scaleDeployment(machineApproverNamespace, machineApproverDeployment, machineApproverPodSelector,
		&expectedPodCount)
}

// setPowerShellDefaultShell changes the instance backed by the given Machine to have a default SSH shell of PowerShell
func (tc *testContext) setPowerShellDefaultShell(machine *mapi.Machine) error {
	// This needs to be retried as it will only succeed once the VM is fully initialized
	// even though the machine is fully provisioned with an IP address, the Windows VM is not guaranteed to be fully
	// set up. It can take more than 5 minutes for the VM to be fully configured with the SSH server running, after the
	// Machine has hit the 'Provisioned' state.
	// This should not increase the overall test time, as WMCO would have to wait for the VM to be ready anyway.
	return wait.Poll(nodeRetryInterval, vmConfigurationTime, func() (done bool, err error) {
		command := "New-ItemProperty -Path \"HKLM:\\SOFTWARE\\OpenSSH\" -Name DefaultShell " +
			"-Value \"C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe\" -PropertyType String -Force"
		addr, err := controllers.GetAddress(machine.Status.Addresses)
		if err != nil {
			log.Printf("Machine %s does not have a valid address, retrying...", machine.GetName())
			return false, nil
		}
		_, err = tc.runPowerShellSSHJob("change-default-shell", command, addr)
		if err != nil {
			log.Printf("failed to change %s default shell to Powershell", machine.GetName())
			return false, nil
		}
		return true, nil
	})
}

// uninstallWindowsDefender uninstalls Windows Defender from the given machine. Reboots the underlying VM.
func (tc *testContext) uninstallWindowsDefender(machine *mapi.Machine) error {
	addr, err := controllers.GetAddress(machine.Status.Addresses)
	if err != nil {
		return err
	}
	command := "Uninstall-WindowsFeature -Name Windows-Defender; Restart-Computer -Force"
	_, err = tc.runPowerShellSSHJob("uninstall-windows-defender", command, addr)
	if err != nil {
		return fmt.Errorf("failed to uninstall Windows Defender antivirus: %w", err)
	}
	return nil
}

// createWindowsMachineSet creates given number of Windows Machines.
func (tc *testContext) createWindowsMachineSet(replicas int32, ignoreLabel bool) (*mapi.MachineSet, error) {
	machineSet, err := tc.CloudProvider.GenerateMachineSet(ignoreLabel, replicas, tc.windowsServerVersion)
	if err != nil {
		return nil, err
	}
	return tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Create(context.TODO(), machineSet, metav1.CreateOptions{})
}

// deleteMachineSet deletes the MachineSet passed to it
func (tc *testContext) deleteMachineSet(ms *mapi.MachineSet) error {
	return tc.client.Machine.MachineSets(clusterinfo.MachineAPINamespace).Delete(context.TODO(), ms.GetName(),
		metav1.DeleteOptions{})
}

// waitForWindowsMachines waits for a certain amount of Windows Machines to reach a certain phase
// if machineCount = 0, it implies we are only waiting for Machines to be deleted and the phase is
// ignored in this case. Returns the set of Machines that matched the provided ignoreLabel criteria.
// TODO: Have this function take in a list of Windows Machines to wait for https://issues.redhat.com/browse/WINC-620
func (tc *testContext) waitForWindowsMachines(machineCount int, phase string, ignoreLabel bool) (*mapi.MachineList, error) {
	if machineCount == 0 && phase != "" {
		return nil, fmt.Errorf("expected phase to be to be an empty string if machineCount is 0")
	}

	var machines *mapi.MachineList
	machineStateTimeLimit := time.Minute * 5
	startTime := time.Now()
	// Increasing the time limit due to https://bugzilla.redhat.com/show_bug.cgi?id=1936556
	if tc.CloudProvider.GetType() == config.VSpherePlatformType {
		// When deleting Machines, set the time limit to 10 minutes
		if machineCount == 0 {
			machineStateTimeLimit = time.Minute * 10
		} else {
			machineStateTimeLimit = time.Minute * 20
		}
	}
	log.Printf("waiting (timeout: %s) for %d Windows Machines to reach phase %q", machineStateTimeLimit.String(), machineCount, phase)

	listOptions := metav1.ListOptions{LabelSelector: clusterinfo.MachineE2ELabel + "=true"}
	if ignoreLabel {
		listOptions.LabelSelector += "," + controllers.IgnoreLabel + "=true"
	} else {
		listOptions.LabelSelector += "," + controllers.IgnoreLabel + "!=true"
	}
	err := wait.Poll(retryInterval, machineStateTimeLimit, func() (done bool, err error) {
		machines, err = tc.client.Machine.Machines(clusterinfo.MachineAPINamespace).List(context.TODO(), listOptions)
		if err != nil {
			if apierrors.IsNotFound(err) {
				log.Printf("waiting for %d Windows Machines", machineCount)
				return false, nil
			}
			log.Printf("machine object listing failed: %v", err)
			return false, nil
		}
		if len(machines.Items) != machineCount {
			log.Printf("waiting for %d/%d Windows Machines", machineCount-len(machines.Items), machineCount)
			return false, nil
		}
		// A phase of "" skips the phase check
		if phase == "" {
			return true, nil
		}
		for _, machine := range machines.Items {
			if machine.Status.Phase == nil || *machine.Status.Phase != phase {
				return false, nil
			}
			// If waiting for a provisioned Machine, ensure the Machine is fully attached to the network and has an
			// assigned IPv4 address.
			if phase != "Provisioned" {
				continue
			}
			hasIPv4 := false
			for _, address := range machine.Status.Addresses {
				if address.Type != v1.NodeInternalIP {
					continue
				}
				if net.ParseIP(address.Address) != nil && net.ParseIP(address.Address).To4() != nil {
					hasIPv4 = true
					break
				}
			}
			if !hasIPv4 {
				return false, nil
			}
		}
		return true, nil
	})
	if phase == "" {
		phase = "deleted"
	}

	// Log the time elapsed while waiting for creation of the Machines
	var machineType string
	if ignoreLabel {
		machineType = "with the ignore label"
	} else {
		machineType = "without the ignore label"
	}
	endTime := time.Now()
	log.Printf("%v time is required for %d Machines %s to reach phase %s", endTime.Sub(startTime),
		len(machines.Items), machineType, phase)
	return machines, err
}

// waitForWindowsNode waits until there exists nodeCount Windows nodes with the correct set of annotations.
// if expectError = true, the function will wait for duration of 10 minutes if we are deleting all nodes i.e. 0 nodesCount
// else 5 minutes for the nodes as the error would be thrown immediately, else we will wait for the duration given by
// nodeCreationTime variable which is 20 minutes increasing the overall wait time in test suite
func (tc *testContext) waitForWindowsNodes(nodeCount int32, expectError, checkVersion bool, isBYOH bool) error {
	annotations := []string{nodeconfig.HybridOverlaySubnet, nodeconfig.HybridOverlayMac, metadata.VersionAnnotation,
		nodeconfig.PubKeyHashAnnotation, controllers.UsernameAnnotation}

	var creationTime time.Duration
	startTime := time.Now()
	if expectError {
		if nodeCount == 0 {
			creationTime = time.Minute * 10
		} else {
			// The time we expect to wait, if the ignore label is
			// not used while creating nodes.
			creationTime = time.Minute * 5
		}
	} else {
		creationTime = nodeCreationTime
	}

	privKey, pubKey, err := tc.getExpectedKeyPair()
	if err != nil {
		return fmt.Errorf("error getting the expected public/private key pair: %w", err)
	}
	pubKeyAnnotation := nodeconfig.CreatePubKeyHashAnnotation(pubKey)

	// We are waiting 20 minutes for each windows VM to be shown up in the cluster. The value comes from
	// nodeCreationTime variable.  If we are testing a scale down from n nodes to 0, then we should
	// not take the number of nodes into account. If we are testing node creation without applying the ignore label, we
	// should throw error within 5 mins.
	err = wait.Poll(nodeRetryInterval, time.Duration(math.Max(float64(nodeCount), 1))*creationTime, func() (done bool, err error) {
		nodes, err := tc.listFullyConfiguredWindowsNodes(isBYOH)
		if err != nil {
			log.Printf("failed to get list of configured Windows nodes: %s", err)
			return false, nil
		}

		for _, node := range nodes {
			// check node status
			readyCondition := false
			for _, condition := range node.Status.Conditions {
				if condition.Type == v1.NodeReady {
					readyCondition = true
				}
				if readyCondition && condition.Status != v1.ConditionTrue {
					log.Printf("node %v is expected to be in Ready state", node.Name)
					return false, nil
				}
			}
			if !readyCondition {
				log.Printf("expected node Status to have condition type Ready for node %v", node.Name)
				return false, nil
			}
			// explicitly check for the external cloud provider taint for more helpful test logging
			for _, taint := range node.Spec.Taints {
				if taint.Key == cloudproviderapi.TaintExternalCloudProvider && taint.Effect == v1.TaintEffectNoSchedule {
					log.Printf("expected node %s to not have the external cloud provider taint", node.GetName())
					return false, nil
				}
			}
			if node.Spec.Unschedulable {
				log.Printf("expected node %s to be schedulable", node.Name)
				return false, nil
			}

			for _, annotation := range annotations {
				_, found := node.Annotations[annotation]
				if !found {
					log.Printf("node %s does not have annotation: %s", node.GetName(), annotation)
					return false, nil
				}
			}
			if checkVersion {
				operatorVersion, err := getWMCOVersion()
				if err != nil {
					log.Printf("error getting operator version : %v", err)
					return false, nil
				}
				if node.Annotations[metadata.VersionAnnotation] != operatorVersion {
					log.Printf("node %s has mismatched version annotation %s. expected: %s", node.GetName(),
						node.Annotations[metadata.VersionAnnotation], operatorVersion)
					return false, nil
				}
			}
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] != pubKeyAnnotation {
				log.Printf("node %s has mismatched pubkey annotation value %s expected: %s", node.GetName(),
					node.Annotations[nodeconfig.PubKeyHashAnnotation], pubKeyAnnotation)
				return false, nil
			}
			// Ensure username annotation is decipherable and correct. Skip if deconfiguring node
			if !expectError {
				username, err := crypto.DecryptFromJSONString(node.Annotations[controllers.UsernameAnnotation], privKey)
				if err != nil {
					log.Printf("error decrypting username annotation for node %s: %s", node.Name, err)
					return false, nil
				}
				if username != tc.vmUsername() {
					log.Printf("username %s does not match expected value %s for node %s:", username, tc.vmUsername(),
						node.Name)
					return false, nil
				}
			}
		}
		// Now verify that we have found all the nodes being waited for
		if len(nodes) != int(nodeCount) {
			log.Printf("waiting for %d/%d Windows nodes", len(nodes), nodeCount)
			return false, nil
		}

		// Initialize/update nodes to avoid staleness
		if isBYOH {
			gc.byohNodes = nodes
		} else {
			gc.machineNodes = nodes
		}
		return true, nil
	})

	// Log the time elapsed while waiting for creation of the nodes
	endTime := time.Now()
	log.Printf("%v time is required to configure %v nodes", endTime.Sub(startTime), nodeCount)

	return err
}

// listFullyConfiguredWindowsNodes returns a slice of nodes. If isBYOH is set to true, the nodes returned will be
// BYOH nodes, else they will be nodes configured by the Machine controller.
// A node is considered fully configured once it has the WMCO version annotation applied to it.
func (tc *testContext) listFullyConfiguredWindowsNodes(isBYOH bool) ([]v1.Node, error) {
	labelSelector := v1.LabelOSStable + "=windows"
	if isBYOH {
		// BYOH label is set to true
		labelSelector = fmt.Sprintf("%s,%s=true", labelSelector, controllers.BYOHLabel)
	} else {
		// BYOH label is not set
		labelSelector = fmt.Sprintf("%s,!%s", labelSelector, controllers.BYOHLabel)
	}
	nodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(),
		metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("unable to list nodes: %w", err)
	}
	var windowsNodes []v1.Node
	for _, node := range nodes.Items {
		// filter out nodes that haven't been fully configured
		if _, present := node.Annotations[metadata.VersionAnnotation]; !present {
			continue
		}
		windowsNodes = append(windowsNodes, node)
	}
	return windowsNodes, nil
}

// scaleDeployment scales the deployment associated with the given namespace and name to the expectedPodCount
func (tc *testContext) scaleDeployment(namespace, name, selector string, expectedPodCount *int32) error {
	deployment, err := tc.client.K8s.AppsV1().Deployments(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting deployment %s/%s: %w", namespace, name, err)
	}

	deployment.Spec.Replicas = expectedPodCount
	_, err = tc.client.K8s.AppsV1().Deployments(namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating deployment %s/%s: %w", namespace, name, err)
	}

	err = wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		// List the pods using the given selector and ensure there are the expected number
		pods, err := tc.client.K8s.CoreV1().Pods(namespace).List(context.TODO(),
			metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false, fmt.Errorf("error listing pods for deployment %s/%s: %w", namespace, name, err)
		}
		return len(pods.Items) == int(*expectedPodCount), nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for deployment %s/%s to be scaled: %w", namespace, name, err)
	}
	return nil
}
