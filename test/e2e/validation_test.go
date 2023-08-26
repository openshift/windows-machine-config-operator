package e2e

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	config "github.com/openshift/api/config/v1"
	operators "github.com/operator-framework/api/pkg/operators/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/semver"
	batch "k8s.io/api/batch/v1"
	certificates "k8s.io/api/certificates/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	cloudproviderapi "k8s.io/cloud-provider/api"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/csr"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	nc "github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	e2e_windows "github.com/openshift/windows-machine-config-operator/test/e2e/windows"
)

const (
	// wmcoContainerName is the name of the container in the deployment spec of the operator
	wmcoContainerName = "manager"
)

// versionRegex captures the version from the output of the WMCO version command
// example: captures `5.0.0-1b759bf1-dirty` from the string
// `windows-machine-config-operator version: "5.0.0-1b759bf1-dirty", go version: "go1.17.5 linux/amd64"`
var versionRegex = regexp.MustCompile(`version: "([^"]*)"`)

// winService contains information regarding a Windows service's current state
type winService struct {
	state       string
	description string
}

// testNodesBecomeReadyAndSchedulable tests that all Windows nodes become ready and schedulable
func (tc *testContext) testNodesBecomeReadyAndSchedulable(t *testing.T) {
	nodes := gc.allNodes()
	for _, node := range nodes {
		t.Run(node.GetName(), func(t *testing.T) {
			err := wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (done bool, err error) {
				foundNode, err := tc.client.K8s.CoreV1().Nodes().Get(context.TODO(), node.GetName(), meta.GetOptions{})
				require.NoError(t, err)
				return tc.nodeReadyAndSchedulable(*foundNode), nil
			})
			assert.NoError(t, err)
		})
	}
}

// nodeReadyAndSchedulable returns true if the node is both ready and is not marked as unschedulable
func (tc *testContext) nodeReadyAndSchedulable(node core.Node) bool {
	readyCondition := false
	for _, condition := range node.Status.Conditions {
		if condition.Type == core.NodeReady {
			readyCondition = true
		}
		if readyCondition && condition.Status != core.ConditionTrue {
			log.Printf("node %v is expected to be in Ready state", node.Name)
			return false
		}
	}
	if !readyCondition {
		log.Printf("expected node Status to have condition type Ready for node %v", node.Name)
		return false
	}
	// this taint is applied by WMCO some at point after WICD configures the node
	for _, taint := range node.Spec.Taints {
		if taint.Key == cloudproviderapi.TaintExternalCloudProvider && taint.Effect == core.TaintEffectNoSchedule {
			log.Printf("expected node %s to not have the external cloud provider taint", node.GetName())
			return false
		}
	}
	// WMCO will uncordon the node at some point after WICD configures it
	if node.Spec.Unschedulable {
		log.Printf("expected node %s to be schedulable", node.Name)
		return false
	}
	return true
}

// testKubeletPriorityClass tests if kubelet priority class is set to "AboveNormal"
func (tc *testContext) testKubeletPriorityClass(t *testing.T) {
	requiredPriorityClass := "AboveNormal"

	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")
	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			out, err := tc.getKubeletPriorityClass(&node)
			require.NoError(t, err, "error getting kubelet priority class")
			assert.Containsf(t, out, requiredPriorityClass, "node %s missing required kubelet priority class",
				node.GetName())
		})
	}
}

// getKubeletPriorityClass returns the priority class of the kubelet service
func (tc *testContext) getKubeletPriorityClass(node *core.Node) (string, error) {
	command := "Get-Process kubelet | Select-Object PriorityClass"
	addr, err := controllers.GetAddress(node.Status.Addresses)
	if err != nil {
		return "", fmt.Errorf("error getting node address: %w", err)
	}
	out, err := tc.runPowerShellSSHJob("kubelet-priority-class-query", command, addr)
	if err != nil {
		return "", fmt.Errorf("error querying kubelet service for priority class: %w", err)
	}
	return out, nil
}

// testNodeMetadata tests if all nodes have a worker label and are annotated with the version of
// the currently deployed WMCO
func (tc *testContext) testNodeMetadata(t *testing.T) {
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err, "could not get WMCO version")

	_, pubKey, err := tc.getExpectedKeyPair()
	require.NoError(t, err, "error getting the expected public/private key pair")
	pubKeyAnnotation := nc.CreatePubKeyHashAnnotation(pubKey)

	for _, node := range gc.allNodes() {
		t.Run(node.GetName()+" Validation Tests", func(t *testing.T) {
			// The worker label is not actually added by WMCO however we would like to validate if the Machine Api is
			// properly adding the worker label, if it was specified in the MachineSet. The MachineSet created in the
			// test suite has the worker label
			t.Run("Worker Label", func(t *testing.T) {
				assert.Contains(t, node.Labels, nc.WorkerLabel, "expected node label %s was not present on %s",
					nc.WorkerLabel, node.GetName())
			})
			t.Run("Version Annotation", func(t *testing.T) {
				require.Containsf(t, node.Annotations, metadata.VersionAnnotation, "node %s missing version annotation",
					node.GetName())
				assert.Equal(t, operatorVersion, node.Annotations[metadata.VersionAnnotation],
					"WMCO version annotation mismatch")
			})
			t.Run("Public Key Annotation", func(t *testing.T) {
				require.Containsf(t, node.Annotations, nc.PubKeyHashAnnotation, "node %s missing public key annotation",
					node.GetName())
				assert.Equal(t, pubKeyAnnotation, node.Annotations[nc.PubKeyHashAnnotation],
					"public key annotation mismatch")
			})
		})
	}
	t.Run("Windows node metadata not applied to Linux nodes", func(t *testing.T) {
		nodes, err := tc.client.K8s.CoreV1().Nodes().List(context.TODO(), meta.ListOptions{
			LabelSelector: core.LabelOSStable + "=linux"})
		require.NoError(t, err, "error listing Linux nodes")
		for _, node := range nodes.Items {
			assert.NotContainsf(t, node.Annotations, metadata.VersionAnnotation,
				"version annotation applied to Linux node %s", node.GetName())
			assert.NotContainsf(t, node.Annotations, nc.PubKeyHashAnnotation,
				"public key annotation applied to Linux node %s", node.GetName())
		}
	})
}

// testNodeIPArg tests that the node-ip kubelet arg is set only when platform type == none
func (tc *testContext) testNodeIPArg(t *testing.T) {
	nodeIPArg := "--node-ip"

	// Nodes configured from Machines should never have the node-ip arg set
	t.Run("machines", func(t *testing.T) {
		if numberOfMachineNodes == 0 {
			t.Skip("0 expected machine nodes")
		}
		for _, node := range gc.machineNodes {
			out, err := tc.getKubeletServiceBinPath(&node)
			require.NoError(t, err, "error getting kubelet binpath")
			assert.NotContains(t, out, nodeIPArg,
				"node-ip arg should not be set for nodes configured from Machines")
		}
	})

	// BYOH nodes should only have the node-ip arg set when platform type == 'none'
	t.Run("byoh", func(t *testing.T) {
		if numberOfBYOHNodes == 0 {
			t.Skip("0 expected byoh nodes")
		}
		for _, node := range gc.byohNodes {
			t.Run(node.GetName(), func(t *testing.T) {
				out, err := tc.getKubeletServiceBinPath(&node)
				require.NoError(t, err, "error getting kubelet binpath")

				// node-ip flag should only be set when platform type == 'none'
				if tc.CloudProvider.GetType() == config.NonePlatformType {
					// TODO: Check the actual value of this and compare to the value stored in the ConfigMap
					//       https://issues.redhat.com/browse/WINC-671
					assert.Contains(t, out, nodeIPArg, "node-ip arg must be present on platform 'none'")
				} else {
					assert.NotContains(t, out, nodeIPArg,
						"node-ip arg should not be set for platforms other than 'none'")
				}
			})
		}

	})
}

// getKubeletServiceBinPath returns the binpath of the kubelet service. This includes the kubelet executable path and
// arguments.
func (tc *testContext) getKubeletServiceBinPath(node *core.Node) (string, error) {
	command := "Get-WmiObject win32_service | Where-Object {$_.Name -eq \\\"kubelet\\\"}| select PathName | " +
		"ConvertTo-Csv"
	addr, err := controllers.GetAddress(node.Status.Addresses)
	if err != nil {
		return "", fmt.Errorf("error getting node address: %w", err)
	}
	out, err := tc.runPowerShellSSHJob("kubelet-query", command, addr)
	if err != nil {
		return "", fmt.Errorf("error querying kubelet service: %w", err)
	}
	return out, nil
}

// getInstanceID gets the instanceID of VM for a given cloud provider ID
// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
func getInstanceID(providerID string) string {
	providerTokens := strings.Split(providerID, "/")
	return providerTokens[len(providerTokens)-1]
}

// getInstanceIDsOfNodes returns the instanceIDs of all the Windows nodes created
func (tc *testContext) getInstanceIDsOfNodes() ([]string, error) {
	instanceIDs := make([]string, 0, len(gc.allNodes()))
	for _, node := range gc.allNodes() {
		if len(node.Spec.ProviderID) > 0 {
			instanceID := getInstanceID(node.Spec.ProviderID)
			instanceIDs = append(instanceIDs, instanceID)
		}
	}
	return instanceIDs, nil
}

// getWMCOVersion returns the version that the operator reports
func getWMCOVersion() (string, error) {
	cmd := exec.Command("oc", "exec", "deploy/windows-machine-config-operator", "-n", wmcoNamespace, "--",
		"/usr/local/bin/windows-machine-config-operator", "version")
	out, err := cmd.Output()
	if err != nil {
		var exitError *exec.ExitError
		stderr := ""
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		return "", fmt.Errorf("oc exec failed with exit code %s and output: %s: %s", err, string(out), stderr)
	}
	// out is formatted like:
	// windows-machine-config-operator version: "5.0.0-1b759bf1-dirty", go version: "go1.17.5 linux/amd64"
	matches := versionRegex.FindStringSubmatch(string(out))
	if len(matches) < 2 {
		return "", fmt.Errorf("could not parse version from '%s'", string(out))
	}
	return matches[1], nil
}

// testNodeTaint tests if the Windows node has the Windows taint
func (tc *testContext) testNodeTaint(t *testing.T) {
	// windowsTaint is the taint that needs to be applied to the Windows node
	windowsTaint := core.Taint{
		Key:    "os",
		Value:  "Windows",
		Effect: core.TaintEffectNoSchedule,
	}

	for _, node := range gc.allNodes() {
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

// ensureTestRunnerSA ensures the proper ServiceAccount exists, a requirement for SSHing into a Windows node
// noop if the ServiceAccount already exists.
func (tc *testContext) ensureTestRunnerSA() error {
	sa := core.ServiceAccount{ObjectMeta: meta.ObjectMeta{Name: tc.workloadNamespace}}
	_, err := tc.client.K8s.CoreV1().ServiceAccounts(tc.workloadNamespace).Create(context.TODO(), &sa,
		meta.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("unable to create SA: %w", err)
	}
	return nil
}

// ensureTestRunnerRole ensures the proper Role exists, a requirement for SSHing into a Windows node
// noop if the Role already exists.
func (tc *testContext) ensureTestRunnerRole() error {
	role := rbac.Role{
		TypeMeta:   meta.TypeMeta{},
		ObjectMeta: meta.ObjectMeta{Name: tc.workloadNamespace},
		Rules: []rbac.PolicyRule{
			{
				Verbs:         []string{"use"},
				APIGroups:     []string{"security.openshift.io"},
				Resources:     []string{"securitycontextconstraints"},
				ResourceNames: []string{"hostnetwork"},
			},
		},
	}
	_, err := tc.client.K8s.RbacV1().Roles(tc.workloadNamespace).Create(context.TODO(), &role, meta.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("unable to create role: %w", err)
	}
	return nil
}

// ensureTestRunnerRoleBinding ensures the proper RoleBinding exists, a requirement for SSHing into a Windows node
// noop if the RoleBinding already exists.
func (tc *testContext) ensureTestRunnerRoleBinding() error {
	rb := rbac.RoleBinding{
		TypeMeta:   meta.TypeMeta{},
		ObjectMeta: meta.ObjectMeta{Name: tc.workloadNamespace},
		Subjects: []rbac.Subject{{
			Kind:      "ServiceAccount",
			APIGroup:  "",
			Name:      tc.workloadNamespace,
			Namespace: tc.workloadNamespace,
		}},
		RoleRef: rbac.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     tc.workloadNamespace,
		},
	}
	_, err := tc.client.K8s.RbacV1().RoleBindings(tc.workloadNamespace).Create(context.TODO(), &rb, meta.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("unable to create role: %w", err)
	}
	return nil
}

// sshSetup creates all the Kubernetes resources required to SSH into a Windows node
func (tc *testContext) sshSetup() error {
	if err := tc.ensureTestRunnerSA(); err != nil {
		return fmt.Errorf("error ensuring SA created: %w", err)
	}
	if err := tc.ensureTestRunnerRole(); err != nil {
		return fmt.Errorf("error ensuring Role created: %w", err)
	}
	if err := tc.ensureTestRunnerRoleBinding(); err != nil {
		return fmt.Errorf("error ensuring RoleBinding created: %w", err)
	}
	return nil
}

// runPowerShellSSHJob creates and waits for a Kubernetes job to run. The command provided will be executed through
// PowerShell, on the host specified by the provided IP.
func (tc *testContext) runPowerShellSSHJob(name, command, ip string) (string, error) {
	// Modify command to work when default shell is the newer Powershell version present on Windows Server 2022.
	powershellDefaultCommand := command
	if tc.windowsServerVersion == e2e_windows.Server2022 {
		powershellDefaultCommand = strings.ReplaceAll(command, "\\\"", "\"")
	}

	keyMountDir := "/private-key"
	sshCommand := []string{"bash", "-c",
		fmt.Sprintf(
			// first determine if the host has PowerShell or cmd as the default shell by running a simple PowerShell
			// command. If it succeeds, then the host's default shell is PowerShell
			"if ssh -o StrictHostKeyChecking=no -i %s %s@%s 'Get-Help';"+
				"then CMD_PREFIX=\"\";CMD_SUFFIX=\"\";"+
				// to respect quoting within the given command, wrap the command as a script block
				"COMMAND='{"+powershellDefaultCommand+"}';"+
				// if PowerShell is not the default shell, explicitly run the unmodified command through PowerShell
				"else CMD_PREFIX=\""+remotePowerShellCmdPrefix+" \\\"\";CMD_SUFFIX=\"\\\"\";"+
				"COMMAND='{"+command+"}';"+
				"fi;"+
				// execute the command as a script block via the PowerShell call operator `&`
				"ssh -o StrictHostKeyChecking=no -i %s %s@%s ${CMD_PREFIX}\" & $COMMAND \"${CMD_SUFFIX}",
			filepath.Join(keyMountDir, secrets.PrivateKeySecretKey), tc.vmUsername(), ip,
			filepath.Join(keyMountDir, secrets.PrivateKeySecretKey), tc.vmUsername(), ip)}

	return tc.runJob(name, sshCommand)
}

// runJob creates and waits for a Kubernetes job to run. The command provided will be executed on a Linux worker,
// using the tools image.
func (tc *testContext) runJob(name string, command []string) (string, error) {
	// Create a job which runs the provided command via SSH
	keyMountDir := "/private-key"
	keyMode := int32(0600)
	job := &batch.Job{
		ObjectMeta: meta.ObjectMeta{
			GenerateName: name + "-job-",
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				Spec: core.PodSpec{
					OS:                 &core.PodOS{Name: core.Linux},
					HostNetwork:        true,
					RestartPolicy:      core.RestartPolicyNever,
					ServiceAccountName: tc.workloadNamespace,
					Containers: []core.Container{
						{
							Name:            name,
							Image:           tc.toolsImage,
							ImagePullPolicy: core.PullIfNotPresent,
							Command:         command,
							VolumeMounts: []core.VolumeMount{{
								Name:      "private-key",
								MountPath: keyMountDir,
							}},
						},
					},
					Volumes: []core.Volume{{Name: "private-key", VolumeSource: core.VolumeSource{
						Secret: &core.SecretVolumeSource{
							SecretName:  secrets.PrivateKeySecret,
							DefaultMode: &keyMode,
						},
					}}},
				},
			},
		},
	}

	jobsClient := tc.client.K8s.BatchV1().Jobs(tc.workloadNamespace)
	job, err := jobsClient.Create(context.TODO(), job, meta.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating job: %w", err)
	}

	// Wait for the job to complete then gather and return the pod output
	if err = tc.waitUntilJobSucceeds(job.GetName()); err != nil {
		return "", fmt.Errorf("error waiting for job to succeed: %w", err)
	}
	labelSelector := "job-name=" + job.Name
	logs, err := tc.getLogs(labelSelector)
	if err != nil {
		return "", fmt.Errorf("error getting logs from job pod: %w", err)
	}
	return logs, nil
}

// getWinServices returns a map of Windows services from the instance with the given address, the map key being the
// service's name
func (tc *testContext) getWinServices(addr string) (map[string]winService, error) {
	// This command returns CR+newline separated quoted CSV entries consisting of service name, state and description.
	// For example: "kubelet","Running","OpenShift managed kubelet"\r\n"VaultSvc","Stopped",
	command := "Get-CimInstance -ClassName Win32_Service | Select-Object -Property Name,State,Description | " +
		"ConvertTo-Csv -NoTypeInformation"
	out, err := tc.runPowerShellSSHJob("get-windows-svc-list", command, addr)
	if err != nil {
		return nil, fmt.Errorf("error running SSH job: %w", err)
	}

	// Remove the header and trailing whitespace from the command output
	outSplit := strings.SplitAfterN(out, "\"Name\",\"State\",\"Description\"\r\n", 2)
	if len(outSplit) != 2 {
		return nil, fmt.Errorf("unexpected command output: " + out)
	}
	trimmedList := strings.TrimSpace(outSplit[1])

	// Make a map from the services, removing the quotes around each entry
	services := make(map[string]winService)
	lines := strings.Split(trimmedList, "\r\n")
	for _, line := range lines {
		// Split into 3 substrings, Name, State, Description. The description can contain a comma, so SplitN is required
		fields := strings.SplitN(line, ",", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("expected comma separated values, found: " + line)
		}
		name := strings.Trim(fields[0], "\"")
		state := strings.Trim(fields[1], "\"")
		description := strings.Trim(fields[2], "\"")

		services[name] = winService{state: state, description: description}
	}
	return services, nil
}

// testExpectedServicesRunning tests that for each node all the expected services are running
func (tc *testContext) testExpectedServicesRunning(t *testing.T) {
	expectedSvcs, err := tc.expectedWindowsServices(windows.RequiredServices)
	require.NoError(t, err)
	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")
			svcs, err := tc.getWinServices(addr)
			require.NoError(t, err, "error getting service map")
			for svcName, shouldBeRunning := range expectedSvcs {
				t.Run(svcName, func(t *testing.T) {
					if shouldBeRunning {
						require.Contains(t, svcs, svcName, "service not found")
						assert.Equal(t, "Running", svcs[svcName].state)
						assert.Contains(t, svcs[svcName].description, windows.ManagedTag)
					} else {
						require.NotContains(t, svcs, svcName, "service exists when it shouldn't")
					}
				})
			}
		})
	}
}

// expectedWindowsServices returns a map of the names of the WMCO owned Windows services, with a value indicating if it
// should or should not be running on the instance.
func (tc *testContext) expectedWindowsServices(alwaysRequiredSvcs []string) (map[string]bool, error) {
	ownedByCCM, err := cluster.IsCloudControllerOwnedByCCM(tc.client.Config)
	if err != nil {
		return nil, err
	}
	serviceMap := make(map[string]bool)
	for _, svc := range alwaysRequiredSvcs {
		serviceMap[svc] = true
	}
	if ownedByCCM && tc.CloudProvider.GetType() == config.AzurePlatformType {
		serviceMap[windows.AzureCloudNodeManagerServiceName] = true
	} else {
		serviceMap[windows.AzureCloudNodeManagerServiceName] = false
	}
	return serviceMap, nil
}

// testServicesConfigMap tests multiple aspects of expected functionality for the services ConfigMap
// 1. It exists on operator startup 2. It is re-created when deleted 3. It is recreated if invalid contents are detected
func (tc *testContext) testServicesConfigMap(t *testing.T) {
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err)
	servicesConfigMapName := servicescm.NamePrefix + operatorVersion

	// Ensure the windows-services ConfigMap exists in the cluster
	var cmData *servicescm.Data
	t.Run("Services ConfigMap contents", func(t *testing.T) {
		// Get CM and parse data
		cm, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), servicesConfigMapName,
			meta.GetOptions{})
		require.NoErrorf(t, err, "error ensuring ConfigMap %s exists", servicesConfigMapName)
		cmData, err = servicescm.Parse(cm.Data)
		require.NoError(t, err, "unable to parse ConfigMap data")

		// Check that only the expected services are defined within the CM data. WICD itself should not be defined in it
		expectedSvcs, err := tc.expectedWindowsServices(windows.RequiredServices)
		expectedSvcs[windows.WicdServiceName] = false
		require.NoError(t, err)
		for svcName, shouldBeInConfigMap := range expectedSvcs {
			t.Run(svcName, func(t *testing.T) {
				assert.Equalf(t, shouldBeInConfigMap, containsService(svcName, cmData.Services),
					"service existence should be %t", shouldBeInConfigMap)
			})
		}
	})

	t.Run("Services ConfigMap re-creation", func(t *testing.T) {
		err = tc.testServicesCMRegeneration(servicesConfigMapName, cmData)
		assert.NoErrorf(t, err, "error ensuring ConfigMap %s is re-created when deleted", servicesConfigMapName)
	})

	t.Run("Invalid services ConfigMap deletion", func(t *testing.T) {
		err = tc.testInvalidServicesCM(servicesConfigMapName, cmData)
		assert.NoError(t, err, "error testing handling of invalid ConfigMap")
	})
}

// containsService returns true if the given service exists within the services list
func containsService(name string, services []servicescm.Service) bool {
	for _, svc := range services {
		if svc.Name == name {
			return true
		}
	}
	return false
}

// testServicesCMRegeneration tests that if the services ConfigMap is deleted, a valid one is re-created in its place
func (tc *testContext) testServicesCMRegeneration(cmName string, expected *servicescm.Data) error {
	err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(), cmName, meta.DeleteOptions{})
	if err != nil {
		return err
	}
	_, err = tc.waitForValidWindowsServicesConfigMap(cmName, expected)
	return err
}

// testInvalidServicesCM tests that an invalid services ConfigMap is deleted and a valid one is re-created in its place
func (tc *testContext) testInvalidServicesCM(cmName string, expected *servicescm.Data) error {
	// Scale down the WMCO deployment to 0
	if err := tc.scaleWMCODeployment(0); err != nil {
		return err
	}
	// Delete existing services CM
	err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Delete(context.TODO(), cmName, meta.DeleteOptions{})
	if err != nil {
		return err
	}

	// Generate and create a service CM with incorrect data
	invalidServicesCM, err := servicescm.Generate(cmName, wmcoNamespace,
		&servicescm.Data{Services: []servicescm.Service{{Name: "fakeservice", Bootstrap: true}},
			Files: []servicescm.FileInfo{}})
	if err != nil {
		return err
	}
	if _, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Create(context.TODO(), invalidServicesCM,
		meta.CreateOptions{}); err != nil {
		return err
	}

	// Restart the operator pod
	if err := tc.scaleWMCODeployment(1); err != nil {
		return err
	}
	// Try to retrieve newly created ConfigMap and validate its contents
	_, err = tc.waitForValidWindowsServicesConfigMap(cmName, expected)
	if err != nil {
		return fmt.Errorf("error waiting for valid ConfigMap %s: %w", cmName, err)
	}
	return nil
}

// waitForValidWindowsServicesConfigMap returns a reference to the ConfigMap that matches the given name.
// If a ConfigMap with valid contents is not found within the time limit, an error is returned.
func (tc *testContext) waitForValidWindowsServicesConfigMap(cmName string,
	expected *servicescm.Data) (*core.ConfigMap, error) {
	configMap := &core.ConfigMap{}
	err := wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		var err error
		configMap, err = tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), cmName, meta.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Retry if the Get() results in a IsNotFound error
				return false, nil
			}
			return false, fmt.Errorf("error retrieving ConfigMap: %s: %w", cmName, err)
		}
		// Here, we've retreived a ConfigMap but still need to ensure it is valid.
		// If it's not valid, retry in hopes that WMCO will replace it with a valid one as expected.
		data, err := servicescm.Parse(configMap.Data)
		if err != nil || data.ValidateExpectedContent(expected) != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("error waiting for ConfigMap %s/%s: %w", wmcoNamespace, cmName, err)
	}
	return configMap, nil
}

// waitForServicesConfigMapDeletion waits for a ConfigMap by the given name to deleted.
// Returns an error if it is still present in the WMCO namespace at the time limit.
func (tc *testContext) waitForServicesConfigMapDeletion(cmName string) error {
	err := wait.PollImmediate(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		_, err := tc.client.K8s.CoreV1().ConfigMaps(wmcoNamespace).Get(context.TODO(), cmName, meta.GetOptions{})
		if err == nil {
			// Retry if the resource is found
			return false, nil
		}
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("error retrieving ConfigMap: %s: %w", cmName, err)
	})
	if err != nil {
		return fmt.Errorf("error waiting for ConfigMap deletion %s/%s: %w", wmcoNamespace, cmName, err)
	}
	return nil
}

// testCSRApproval tests if the BYOH CSR's have been approved by WMCO CSR approver
func (tc *testContext) testCSRApproval(t *testing.T) {
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH CSR approval testing disabled")
	}
	for _, node := range gc.byohNodes {
		csrs, err := tc.findNodeCSRs(node.GetName())
		require.NotEqual(t, len(csrs), 0, "could not find BYOH node CSR's")
		require.NoError(t, err, "could not find BYOH node CSR's")

		for _, csr := range csrs {
			isWMCOApproved := func() bool {
				for _, c := range csr.Status.Conditions {
					if c.Type == certificates.CertificateApproved && c.Reason == "WMCOApprove" {
						return true
					}
				}
				return false
			}()
			assert.Equal(t, isWMCOApproved, true, "expected BYOH node %s CSR %s to be approved by WMCO CSR approver",
				node.GetName(), csr.GetName())
		}
	}

	// Scale the Cluster Machine Approver deployment back to 1.
	expectedPodCount := int32(1)
	err := tc.scaleDeployment(machineApproverNamespace, machineApproverDeployment, machineApproverPodSelector,
		&expectedPodCount)
	require.NoError(t, err, "failed to scale up Cluster Machine Approver pods")
}

// findNodeCSRs returns the list of CSRs for the given node
func (tc *testContext) findNodeCSRs(nodeName string) ([]certificates.CertificateSigningRequest, error) {
	var nodeCSRs []certificates.CertificateSigningRequest
	csrs, err := tc.client.K8s.CertificatesV1().CertificateSigningRequests().List(context.TODO(),
		meta.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get CSR list: %w", err)
	}
	for _, c := range csrs.Items {
		// In some cases, a CSR is left in pending state when a new CSR is created
		// for a node too quickly before updating the status of the existing one.
		// Such a CSR cannot be approved but it does not affect node configuration
		// and is safe to be ignored.
		if c.Status.Conditions == nil || len(c.Status.Conditions) == 0 {
			continue
		}
		parsedCSR, err := csr.ParseCSR(c.Spec.Request)
		if err != nil {
			return nil, err
		}
		dnsAddr := strings.TrimPrefix(parsedCSR.Subject.CommonName, csr.NodeUserNamePrefix)
		if dnsAddr == "" {
			return nil, err
		}
		if dnsAddr == nodeName {
			nodeCSRs = append(nodeCSRs, c)
		}
	}
	return nodeCSRs, nil
}

// validateUpgradeableCondition ensures that the operator's Upgradeable condition is correctly communicated to OLM
func (tc *testContext) validateUpgradeableCondition(expected meta.ConditionStatus) error {
	ocName, err := tc.getOperatorConditionName()
	if err != nil {
		return err
	}
	err = wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		oc, err := tc.client.Olm.OperatorsV2().OperatorConditions(wmcoNamespace).Get(context.TODO(), ocName, meta.GetOptions{})
		if err != nil {
			log.Printf("unable to get OperatorCondition %s from namespace %s", ocName, wmcoNamespace)
			return false, nil
		}

		specCheck := condition.Validate(oc.Spec.Conditions, operators.Upgradeable, expected)
		statusCheck := condition.Validate(oc.Status.Conditions, operators.Upgradeable, expected)
		return specCheck && statusCheck, nil
	})
	if err != nil {
		return fmt.Errorf("failed to verify condition type %s has state %s: %w", operators.Upgradeable, expected, err)
	}
	return nil
}

// getOperatorConditionName returns the operator condition name using the env var present in the deployment
func (tc *testContext) getOperatorConditionName() (string, error) {
	deployment, err := tc.client.K8s.AppsV1().Deployments(wmcoNamespace).Get(context.TODO(), resourceName,
		meta.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error getting operator deployment: %w", err)
	}
	// Get the operator condition name using the deployment spec
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != wmcoContainerName {
			continue
		}
		for _, envVar := range container.Env {
			if envVar.Name == condition.OperatorConditionName {
				return envVar.Value, nil
			}
		}
	}
	return "", fmt.Errorf("unable to get operatorCondition name from namespace %s", wmcoNamespace)
}

// testNodeAnnotations tests that all required annotations are on each Windows node
func (tc *testContext) testNodeAnnotations(t *testing.T) {
	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			annotations := []string{nc.HybridOverlaySubnet, nc.HybridOverlayMac, metadata.VersionAnnotation,
				nc.PubKeyHashAnnotation}
			for _, annotation := range annotations {
				assert.Contains(t, node.Annotations, annotation, "node missing expected annotation: %s", annotation)
			}

			usernameCorrect, err := tc.checkUsernameAnnotation(&node)
			require.NoError(t, err)
			assert.True(t, usernameCorrect)

			pubKey, err := tc.checkPubKeyAnnotation(&node)
			require.NoError(t, err)
			assert.True(t, pubKey)

			t.Run("CSI Annotation", func(t *testing.T) {
				tc.testCSIAnnotation(t, &node)
			})
		})
	}
}

// testCSIAnnotation tests that the csi Annotation is applied to the Node for the expected platforms and cluster version
func (tc *testContext) testCSIAnnotation(t *testing.T, node *core.Node) {
	minorVersion, err := tc.clusterMinorVersion()
	require.NoError(t, err)
	if nodeShouldHaveCSIAnnotation(tc.CloudProvider.GetType(), minorVersion) {
		assert.Equal(t, "true", node.Annotations[controllers.CSIAnnotation])
	} else {
		assert.Equal(t, "", node.Annotations[controllers.CSIAnnotation])
	}
}

// checkUsernameAnnotation checks that the username annotation value is decipherable and correct
func (tc *testContext) checkUsernameAnnotation(node *core.Node) (bool, error) {
	privKey, _, err := tc.getExpectedKeyPair()
	if err != nil {
		return false, err
	}

	usernameValue, present := node.Annotations[controllers.UsernameAnnotation]
	if !present {
		return false, nil
	}
	username, err := crypto.DecryptFromJSONString(usernameValue, privKey)
	if err != nil {
		return false, err
	}
	if username != tc.vmUsername() {
		return false, nil
	}
	return true, nil
}

// checkPubKeyAnnotation that node is annotated with the public key which matches the private key used to configure it
func (tc *testContext) checkPubKeyAnnotation(node *core.Node) (bool, error) {
	_, pubKey, err := tc.getExpectedKeyPair()
	if err != nil {
		return false, err
	}

	pubKeyAnnotation := nc.CreatePubKeyHashAnnotation(pubKey)
	if pubKeyAnnotation != node.Annotations[nc.PubKeyHashAnnotation] {
		return false, nil
	}
	return true, nil
}

// clusterMinorVersion returns an int representation of the minor version of the OCP cluster
func (tc *testContext) clusterMinorVersion() (int, error) {
	versionInfo, err := tc.client.Config.Discovery().ServerVersion()
	if err != nil {
		return 0, fmt.Errorf("error retrieving server version: %w", err)
	}
	// split the version in the form Major.Minor
	versionString := semver.MajorMinor(versionInfo.GitVersion)
	parsedSplit := strings.Split(versionString, ".")
	if len(parsedSplit) != 2 {
		return 0, fmt.Errorf("unexpected format for major/minor version %s: %w", versionString, err)
	}
	minorVersion, err := strconv.Atoi(parsedSplit[1])
	if err != nil {
		return 0, fmt.Errorf("unable to convert %s to an int: %w", parsedSplit[1], err)
	}
	return minorVersion, nil
}

// testDependentServiceChanges tests that a Windows service which a running service is dependent on can be reconfigured
func testDependentServiceChanges(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, false, false)
	require.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, false, true)
	require.NoError(t, err, "timed out waiting for BYOH Windows nodes")
	nodes := append(gc.machineNodes, gc.byohNodes...)

	queryCommand := "Get-WmiObject win32_service | Where-Object {$_.Name -eq \\\"hybrid-overlay-node\\\"} " +
		"|select -ExpandProperty PathName"
	for _, node := range nodes {
		t.Run(node.GetName(), func(t *testing.T) {
			// Get initial configuration of hybrid-overlay-node, this service is used as kube-proxy is dependent on it
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "error getting node address")
			out, err := tc.runPowerShellSSHJob("hybrid-overlay-query", queryCommand, addr)
			require.NoError(t, err, "error querying hybrid-overlay service")
			// The binPath/pathName will be the final line of the pod logs
			originalPath := finalLine(out)

			// Change hybrid-overlay-node configuration
			newPath, err := changeHybridOverlayCommandVerbosity(originalPath)
			require.NoError(t, err, "error constructing new hybrid-overlay command")
			changeCommand := fmt.Sprintf("sc.exe config hybrid-overlay-node binPath=\\\"%s\\\"", newPath)
			out, err = tc.runPowerShellSSHJob("hybrid-overlay-change", changeCommand, addr)
			require.NoError(t, err, "error changing hybrid-overlay command")

			// Wait until hybrid-overlay-node is returned to correct config
			err = wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
				out, err = tc.runPowerShellSSHJob("hybrid-overlay-query2", queryCommand, addr)
				if err != nil {
					return false, nil
				}
				currentPath := finalLine(out)
				if currentPath != originalPath {
					log.Printf("waiting for hybrid-overlay service config to be reconciled, current value: %s",
						currentPath)
					return false, nil
				}
				return true, nil
			})
			require.NoError(t, err)
		})
	}
}

// logLevelRegex finds the loglevel argument and captures the log level itself
var logLevelRegex = regexp.MustCompile(`loglevel(?:=|\s)(\d+\.?\d*)`)

// changeHybridOverlayCommandVerbosity will change the loglevel argument in the hybrid-overlay-node command to a
// different level. It takes the full command path of the hybrid-overlay-node service, and returns the command with
// altered arguments.
func changeHybridOverlayCommandVerbosity(in string) (string, error) {
	matches := logLevelRegex.FindStringSubmatch(in)
	if len(matches) != 2 {
		return "", fmt.Errorf("'%s' did not match expected argument format", in)
	}
	originalLogLevel, err := strconv.Atoi(matches[1])
	if err != nil {
		return "", fmt.Errorf("could not convert %s into int: %w", matches[1], err)
	}
	var newLogLevel string
	if originalLogLevel == 0 {
		newLogLevel = fmt.Sprintf("%d", originalLogLevel+1)
	} else {
		newLogLevel = fmt.Sprintf("%d", originalLogLevel-1)
	}
	return logLevelRegex.ReplaceAllString(in, "loglevel "+newLogLevel), nil
}

// finalLine returns the contents of the final line of a given string
func finalLine(s string) string {
	lineSplit := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lineSplit[len(lineSplit)-1])
}

// nodeShouldHaveCSIAnnotation returns true if a Node on the given platform and cluster version should be annotated with
// the CSIAnnotation
func nodeShouldHaveCSIAnnotation(platform config.PlatformType, clusterMinorVersion int) bool {
	if inTreeUpgrade {
		return false
	}
	if platform != config.AzurePlatformType && platform != config.VSpherePlatformType {
		return true
	}
	if platform == config.AzurePlatformType && clusterMinorVersion >= 13 {
		return true
	}
	if platform == config.VSpherePlatformType && clusterMinorVersion >= 14 {
		return true
	}
	return false
}
