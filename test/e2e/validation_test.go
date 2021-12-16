package e2e

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/openshift/api/config/v1"
	operators "github.com/operator-framework/api/pkg/operators/v2"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batch "k8s.io/api/batch/v1"
	certificates "k8s.io/api/certificates/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/csr"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	nc "github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

// testNodeMetadata tests if all nodes have a worker label and kubelet version and are annotated with the version of
// the currently deployed WMCO
func testNodeMetadata(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	operatorVersion, err := getWMCOVersion()
	require.NoError(t, err, "could not get WMCO version")

	clusterKubeletVersion, err := tc.getClusterKubeVersion()
	require.NoError(t, err, "could not get cluster kube version")

	_, pubKey, err := tc.getExpectedKeyPair()
	require.NoError(t, err, "error getting the expected public/private key pair")
	pubKeyAnnotation := nc.CreatePubKeyHashAnnotation(pubKey)

	for _, node := range gc.allNodes() {
		t.Run(node.GetName()+" Validation Tests", func(t *testing.T) {
			t.Run("Kubelet Version", func(t *testing.T) {
				isValidVersion := strings.HasPrefix(node.Status.NodeInfo.KubeletVersion, clusterKubeletVersion)
				assert.True(t, isValidVersion,
					"expected kubelet version %s was not present on %s. Found %s", clusterKubeletVersion,
					node.GetName(), node.Status.NodeInfo.KubeletVersion)
			})
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
func testNodeIPArg(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
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
		return "", errors.Wrap(err, "error getting node address")
	}
	out, err := tc.runPowerShellSSHJob("kubelet-query", command, addr)
	if err != nil {
		return "", errors.Wrap(err, "error querying kubelet service")
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

// getClusterKubeVersion returns the major and minor Kubernetes version of the cluster
func (tc *testContext) getClusterKubeVersion() (string, error) {
	serverVersion, err := tc.client.K8s.Discovery().ServerVersion()
	if err != nil {
		return "", errors.Wrapf(err, "error getting cluster kube version")
	}
	versionSplit := strings.Split(serverVersion.GitVersion, ".")
	if versionSplit == nil {
		return "", fmt.Errorf("unexpected cluster kube version output")
	}
	return strings.Join(versionSplit[0:2], "."), nil
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

// testNodeTaint tests if the Windows node has the Windows taint
func testNodeTaint(t *testing.T) {
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
		return errors.Wrap(err, "unable to create SA")
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
		return errors.Wrap(err, "unable to create role")
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
		return errors.Wrap(err, "unable to create role")
	}
	return nil
}

// sshSetup creates all the Kubernetes resources required to SSH into a Windows node
func (tc *testContext) sshSetup() error {
	if err := tc.ensureTestRunnerSA(); err != nil {
		return errors.Wrap(err, "error ensuring SA created")
	}
	if err := tc.ensureTestRunnerRole(); err != nil {
		return errors.Wrap(err, "error ensuring Role created")
	}
	if err := tc.ensureTestRunnerRoleBinding(); err != nil {
		return errors.Wrap(err, "error ensuring RoleBinding created")
	}
	return nil
}

// runPowerShellSSHJob creates and waits for a Kubernetes job to run. The command provided will be executed through
// PowerShell, on the host specified by the provided IP.
func (tc *testContext) runPowerShellSSHJob(name, command, ip string) (string, error) {
	keyMountDir := "/private-key"
	sshCommand := []string{"bash", "-c",
		fmt.Sprintf(
			// first determine if the host has PowerShell or cmd as the default shell by running a simple PowerShell
			// command. If it succeeds, then the host's default shell is PowerShell
			"if ssh -o StrictHostKeyChecking=no -i %s %s@%s 'Get-Help';"+
				"then CMD_PREFIX=\"\";CMD_SUFFIX=\"\";"+
				// if PowerShell is not the default shell, explicitly run the command through PowerShell
				"else CMD_PREFIX=\""+remotePowerShellCmdPrefix+" \\\"\";CMD_SUFFIX=\"\\\"\";"+
				"fi;"+
				"ssh -o StrictHostKeyChecking=no -i %s %s@%s ${CMD_PREFIX}' %s '${CMD_SUFFIX}",
			filepath.Join(keyMountDir, secrets.PrivateKeySecretKey), tc.vmUsername(), ip,
			filepath.Join(keyMountDir, secrets.PrivateKeySecretKey), tc.vmUsername(), ip, command)}

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
		return "", errors.Wrap(err, "error creating job")
	}

	// Wait for the job to complete then gather and return the pod output
	if err = tc.waitUntilJobSucceeds(job.GetName()); err != nil {
		return "", errors.Wrap(err, "error waiting for job to succeed")
	}
	labelSelector := "job-name=" + job.Name
	logs, err := tc.getLogs(labelSelector)
	if err != nil {
		return "", errors.Wrap(err, "error getting logs from job pod")
	}
	return logs, nil
}

// getWinServices returns a map of Windows services from the instance with the given address, the key:value format being
// name:status
func (tc *testContext) getWinServices(addr string) (map[string]string, error) {
	// This command returns CR+newline separated quoted CSV entries consisting of service name and status. For example:
	// "kubelet","Running"\r\n"VaultSvc","Stopped"
	command := "Get-Service | Select-Object -Property Name,Status | ConvertTo-Csv -NoTypeInformation"
	out, err := tc.runPowerShellSSHJob("get-windows-svc-list", command, addr)
	if err != nil {
		return nil, errors.Wrap(err, "error running SSH job")
	}

	// Remove the header and trailing whitespace from the command output
	outSplit := strings.SplitAfterN(out, "\"Name\",\"Status\"\r\n", 2)
	if len(outSplit) != 2 {
		return nil, errors.New("unexpected command output: " + out)
	}
	trimmedList := strings.TrimSpace(outSplit[1])

	// Make a map from the services, removing the quotes around each entry
	services := make(map[string]string)
	lines := strings.Split(trimmedList, "\r\n")
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return nil, errors.New("expected comma separated values, found: " + line)
		}
		services[strings.Trim(fields[0], "\"")] = strings.Trim(fields[1], "\"")
	}
	return services, nil
}

// testExpectedServicesRunning tests that for each node all the expected services are running
func testExpectedServicesRunning(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)

	for _, node := range gc.allNodes() {
		t.Run(node.GetName(), func(t *testing.T) {
			addr, err := controllers.GetAddress(node.Status.Addresses)
			require.NoError(t, err, "unable to get node address")
			svcs, err := tc.getWinServices(addr)
			require.NoError(t, err, "error getting service map")
			for _, svcName := range windows.RequiredServices {
				t.Run(svcName, func(t *testing.T) {
					require.Contains(t, svcs, svcName, "service not found")
					assert.Equal(t, "Running", svcs[svcName])
				})
			}
		})
	}
}

// testCSRApproval tests if the BYOH CSR's have been approved by WMCO CSR approver
func testCSRApproval(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	if gc.numberOfBYOHNodes == 0 {
		t.Skip("BYOH CSR approval testing disabled")
	}

	deployment, err := testCtx.client.K8s.AppsV1().Deployments("openshift-cluster-machine-approver").Get(context.TODO(),
		"machine-approver", meta.GetOptions{})
	require.NoError(t, err, "error listing Cluster Machine Approver deployment")
	log.Printf("before testCSRApproval deployment %s/%s... currently at %d pods, expected %d pods",
		deployment.GetNamespace(), deployment.GetName(), *deployment.Spec.Replicas, 0)

	for _, node := range gc.byohNodes {
		csrs, err := testCtx.findNodeCSRs(node.GetName())
		require.NotEqual(t, len(csrs), 0, "could not find BYOH node CSR's")
		require.NoError(t, err, "could not find BYOH node CSR's")

		for _, csr := range csrs {
			isWMCOApproved := func() bool {
				for _, c := range csr.Status.Conditions {
					if c.Type == certificates.CertificateApproved && c.Reason == "WMCOApproved" {
						return true
					}
				}
				return false
			}()
			assert.Equal(t, isWMCOApproved, true, "expected BYOH node CSR to be approved by WMCO CSR approver: %s", node.GetName())
		}
	}

	// Scale the Cluster Machine Approver deployment back to 1.
	expectedPodCount := int32(1)
	err = testCtx.scaleMachineApproverDeployment(&expectedPodCount)
	require.NoError(t, err, "failed to scale Cluster Machine Approver pods")
	log.Printf("after testCSRApproval deployment %s/%s... currently at %d pods, expected %d pods",
		deployment.GetNamespace(), deployment.GetName(), *deployment.Spec.Replicas, expectedPodCount)
}

// findNodeCSRs returns the list of CSRs for the given node
func (tc *testContext) findNodeCSRs(nodeName string) ([]certificates.CertificateSigningRequest, error) {
	var nodeCSRs []certificates.CertificateSigningRequest
	csrs, err := tc.client.K8s.CertificatesV1().CertificateSigningRequests().List(context.TODO(),
		meta.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "unable to get CSR list")
	}
	for _, c := range csrs.Items {
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
	ocName, present := os.LookupEnv(condition.OperatorConditionName)
	if !present {
		// Implies operator is not OLM-managed
		return nil
	}
	err := wait.Poll(retry.Interval, retry.ResourceChangeTimeout, func() (bool, error) {
		oc := &operators.OperatorCondition{}
		err := tc.client.Cache.Get(context.TODO(), types.NamespacedName{Namespace: tc.namespace, Name: ocName}, oc)
		if err != nil {
			log.Printf("unable to get OperatorCondition %s from namespace %s", ocName, tc.namespace)
			return false, nil
		}

		specCheck := condition.Validate(oc.Spec.Conditions, operators.Upgradeable, expected)
		statusCheck := condition.Validate(oc.Status.Conditions, operators.Upgradeable, expected)
		return specCheck && statusCheck, nil
	})
	if err != nil {
		return errors.Wrapf(err, "failed to verify condition type %s has status %s", operators.Upgradeable, expected)
	}
	return nil
}
