package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/windows"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// testNetwork runs all the cluster and node network tests
func testNetwork(t *testing.T) {
	t.Run("Hybrid overlay running", testHybridOverlayRunning)
	t.Run("OpenShift HNS networks", testHNSNetworksCreated)
	t.Run("East West Networking across Linux and Windows nodes",
		func(t *testing.T) { testEastWestNetworking(t) })
	t.Run("East West Networking across Windows nodes",
		func(t *testing.T) { testEastWestNetworkingAcrossWindowsNodes(t) })
}

var (
	// windowsServerImage is the name/location of the Windows Server 2019 image we will use to test pod deployment
	windowsServerImage = "mcr.microsoft.com/windows/servercore:ltsc2019"
	// ubi8Image is the name/location of the linux image we will use for testing
	ubi8Image = "registry.access.redhat.com/ubi8/ubi:latest"
	// retryCount is the amount of times we will retry an api operation
	retryCount = 20
	// retryInterval is the interval of time until we retry after a failure
	retryInterval = 5 * time.Second
)

// testHNSNetworksCreated tests that the required HNS Networks have been created on the bootstrapped nodes
func testHNSNetworksCreated(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	for _, vm := range gc.windowsVMs {
		// We don't need to retry as we are waiting long enough for the secrets to be created which implies that the
		// network setup has succeeded.
		stdout, _, err := vm.Run("Get-HnsNetwork", true)
		require.NoError(t, err, "could not run Get-HnsNetwork command")
		assert.Contains(t, stdout, windows.BaseOVNKubeOverlayNetwork,
			"could not find %s in %s", windows.BaseOVNKubeOverlayNetwork, vm.GetCredentials().GetInstanceId())
		assert.Contains(t, stdout, windows.OVNKubeOverlayNetwork,
			"could not find %s in %s", windows.OVNKubeOverlayNetwork, vm.GetCredentials().GetInstanceId())
	}
}

// testHybridOverlayRunning checks if the hybrid-overlay process is running on all the bootstrapped nodes
func testHybridOverlayRunning(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	for _, vm := range gc.windowsVMs {
		_, stderr, err := vm.Run("Get-Process -Name \""+windows.HybridOverlayProcess+"\"", true)
		require.NoError(t, err, "could not run Get-Process command")
		// stderr being empty implies that hybrid-overlay was running.
		assert.Equal(t, "", stderr, "hybrid-overlay was not running in %s",
			vm.GetCredentials().GetInstanceId())
	}
}

// testEastWestNetworking deploys Windows and Linux pods, and tests that the pods can communicate
func testEastWestNetworking(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	for _, vm := range gc.windowsVMs {
		instanceID := vm.GetCredentials().GetInstanceId()
		node, err := testCtx.getNode(instanceID)
		require.NoError(t, err, "could not get Windows node object associated with the vm")

		affinity, err := getAffinityForNode(node)
		require.NoError(t, err, "could not get affinity for first node")

		// Deploy a webserver pod on the new node
		winServerDeployment, err := testCtx.deployWindowsWebServer("win-webserver-"+vm.GetCredentials().GetInstanceId(), vm, affinity)
		require.NoError(t, err, "could not create Windows Server deployment")

		// Get the pod so we can use its IP
		winServerIP, err := testCtx.getPodIP(*winServerDeployment.Spec.Selector)
		require.NoError(t, err, "could not retrieve pod with selector %v", *winServerDeployment.Spec.Selector)

		// test Windows <-> Linux
		// This will install curl and then curl the windows server.
		linuxCurlerCommand := []string{"bash", "-c", "yum update; yum install curl -y; curl " + winServerIP}
		linuxCurlerJob, err := testCtx.createLinuxJob("linux-curler-"+vm.GetCredentials().GetInstanceId(), linuxCurlerCommand)
		require.NoError(t, err, "could not create Linux job")
		err = testCtx.waitUntilJobSucceeds(linuxCurlerJob.Name)
		assert.NoError(t, err, "could not curl the Windows server from a linux container")

		// test Windows <-> Windows on same node
		winCurlerJob, err := testCtx.createWinCurlerJob(vm, winServerIP)
		require.NoError(t, err, "could not create Windows job")
		err = testCtx.waitUntilJobSucceeds(winCurlerJob.Name)
		assert.NoError(t, err, "could not curl the Windows webserver pod from a separate Windows container")

		// delete the deployments and jobs created
		if err = testCtx.deleteDeployment(winServerDeployment.Name); err != nil {
			t.Logf("could not delete deployment %s", winServerDeployment.Name)
		}
		if err = testCtx.deleteJob(linuxCurlerJob.Name); err != nil {
			t.Logf("could not delete job %s", linuxCurlerJob.Name)
		}
		if err = testCtx.deleteJob(winCurlerJob.Name); err != nil {
			t.Logf("could not delete job %s", winCurlerJob.Name)
		}
	}
}

//  testEastWestNetworkingAcrossWindowsNodes deploys Windows pods on two different Nodes, and tests that the pods can communicate
func testEastWestNetworkingAcrossWindowsNodes(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	// Need at least two Windows VMs to run these tests, throwing error if this condition is not met
	require.GreaterOrEqualf(t, len(gc.nodes), 2, "insufficient number of Windows VMs to run tests across"+
		" VMs, Minimum VM count: 2, Current VM count: %d", len(gc.nodes))

	firstVM := gc.windowsVMs[0]
	secondVM := gc.windowsVMs[1]

	instanceIDFirstVM := firstVM.GetCredentials().GetInstanceId()
	firstNode, err := testCtx.getNode(instanceIDFirstVM)
	require.NoError(t, err, "could not get Windows node object from first VM")

	affinityForFirstNode, err := getAffinityForNode(firstNode)
	require.NoError(t, err, "could not get affinity for first node")

	// Deploy a webserver pod on the first node
	winServerDeploymentOnFirstNode, err := testCtx.deployWindowsWebServer("win-webserver-"+firstVM.GetCredentials().GetInstanceId(),
		firstVM, affinityForFirstNode)
	require.NoError(t, err, "could not create Windows Server deployment on first Node")

	// Get the pod so we can use its IP
	winServerIP, err := testCtx.getPodIP(*winServerDeploymentOnFirstNode.Spec.Selector)
	require.NoError(t, err, "could not retrieve pod with selector %v", *winServerDeploymentOnFirstNode.Spec.Selector)

	// test Windows <-> Windows across nodes
	winCurlerJobOnSecondNode, err := testCtx.createWinCurlerJob(secondVM, winServerIP)
	require.NoError(t, err, "could not create Windows job on second Node")

	err = testCtx.waitUntilJobSucceeds(winCurlerJobOnSecondNode.Name)
	assert.NoError(t, err, "could not curl the Windows webserver pod on the first node from Windows container "+
		"on the second node")

	// delete the deployment and job created
	if err = testCtx.deleteDeployment(winServerDeploymentOnFirstNode.Name); err != nil {
		t.Logf("could not delete deployment %s", winServerDeploymentOnFirstNode.Name)
	}

	if err = testCtx.deleteJob(winCurlerJobOnSecondNode.Name); err != nil {
		t.Logf("could not delete job %s", winCurlerJobOnSecondNode.Name)
	}
}

// getAffinityForNode returns an affinity which matches the associated node's name
func getAffinityForNode(node *v1.Node) (*v1.Affinity, error) {
	return &v1.Affinity{
		NodeAffinity: &v1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchFields: []v1.NodeSelectorRequirement{
							{
								Key:      "metadata.name",
								Operator: v1.NodeSelectorOpIn,
								Values:   []string{node.Name},
							},
						},
					},
				},
			},
		},
	}, nil
}

// deployWindowsWebServer creates a deployment with a single Windows Server pod, listening on port 80
func (tc *testContext) deployWindowsWebServer(name string, vm testVM, affinity *v1.Affinity) (*appsv1.Deployment, error) {
	// Preload the image that will be used on the Windows node, to prevent download timeouts
	// and separate possible failure conditions into multiple operations
	if err := pullContainerImage(windowsServerImage, vm); err != nil {
		return nil, errors.Wrapf(err, "could not pull Windows Server image")
	}
	// This will run a Server on the container, which can be reached with a GET request
	winServerCommand := []string{"powershell.exe", "-command",
		"$listener = New-Object System.Net.HttpListener; $listener.Prefixes.Add('http://*:80/'); $listener.Start(); " +
			"Write-Host('Listening at http://*:80/'); while ($listener.IsListening) { " +
			"$context = $listener.GetContext(); $response = $context.Response; " +
			"$content='<html><body><H1>Windows Container Web Server</H1></body></html>'; " +
			"$buffer = [System.Text.Encoding]::UTF8.GetBytes($content); $response.ContentLength64 = $buffer.Length; " +
			"$response.OutputStream.Write($buffer, 0, $buffer.Length); $response.Close(); };"}
	winServerDeployment, err := tc.createWindowsServerDeployment(name, winServerCommand, affinity)
	if err != nil {
		return nil, errors.Wrapf(err, "could not create Windows deployment")
	}
	// Wait until the server is ready to be queried
	err = tc.waitUntilDeploymentScaled(winServerDeployment.Name)
	if err != nil {
		tc.deleteDeployment(winServerDeployment.Name)
		return nil, errors.Wrapf(err, "deployment was unable to scale")
	}
	return winServerDeployment, nil
}

// deleteDeployment deletes the deployment with the given name
func (tc *testContext) deleteDeployment(name string) error {
	deploymentsClient := tc.kubeclient.AppsV1().Deployments(v1.NamespaceDefault)
	return deploymentsClient.Delete(name, &metav1.DeleteOptions{})
}

// pullContainerImage pulls the designated image on the remote host
func pullContainerImage(name string, vm testVM) error {
	command := "docker pull " + name
	_, _, err := vm.Run(command, false)
	if err != nil {
		return errors.Wrapf(err, "failed to remotely run docker pull")
	}
	return nil
}

// getPodIP returns the IP of the pod that matches the label selector. If more than one pod match the
// selector, the function will return an error
func (tc *testContext) getPodIP(selector metav1.LabelSelector) (string, error) {
	selectorString := labels.Set(selector.MatchLabels).String()
	podList, err := tc.kubeclient.CoreV1().Pods(v1.NamespaceDefault).List(metav1.ListOptions{
		LabelSelector: selectorString})
	if err != nil {
		return "", err
	}
	if len(podList.Items) != 1 {
		return "", errors.Errorf("expected one pod matching %s, but found %d", selectorString,
			len(podList.Items))
	}

	return podList.Items[0].Status.PodIP, nil
}

// createWindowsServerDeployment creates a deployment with a Windows Server 2019 container
func (tc *testContext) createWindowsServerDeployment(name string, command []string, affinity *v1.Affinity) (*appsv1.Deployment, error) {
	deploymentsClient := tc.kubeclient.AppsV1().Deployments(v1.NamespaceDefault)
	replicaCount := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-deployment",
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicaCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": name,
					},
				},
				Spec: v1.PodSpec{
					Affinity: affinity,
					Tolerations: []v1.Toleration{
						{
							Key:    "os",
							Value:  "Windows",
							Effect: v1.TaintEffectNoSchedule,
						},
					},
					Containers: []v1.Container{
						// Windows web server
						{
							Name:            name,
							Image:           windowsServerImage,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         command,
							Ports: []v1.ContainerPort{
								{
									Protocol:      v1.ProtocolTCP,
									ContainerPort: 80,
								},
							},
						},
					},
					NodeSelector: map[string]string{"beta.kubernetes.io/os": "windows"},
				},
			},
		},
	}

	// Create Deployment
	deploy, err := deploymentsClient.Create(deployment)
	if err != nil {
		return nil, errors.Wrapf(err, "could not create deployment")
	}
	return deploy, err
}

// waitUntilDeploymentScaled will return nil if the deployment reaches the amount of replicas specified in its spec
func (tc *testContext) waitUntilDeploymentScaled(name string) error {
	var deployment *appsv1.Deployment
	var err error
	for i := 0; i < retryCount; i++ {
		deployment, err = tc.kubeclient.AppsV1().Deployments(v1.NamespaceDefault).Get(name,
			metav1.GetOptions{})
		if err != nil {
			return errors.Wrapf(err, "could not get deployment for %s", name)
		}
		if *deployment.Spec.Replicas == deployment.Status.AvailableReplicas {
			return nil
		}
		time.Sleep(retryInterval)
	}
	events, _ := tc.getPodEvents(name)
	return errors.Errorf("timed out waiting for deployment %v to scale: %v", deployment, events)
}

// getPodEvents gets all events for any pod with the input in its name. Used for debugging purposes
func (tc *testContext) getPodEvents(name string) ([]v1.Event, error) {
	eventList, err := tc.kubeclient.CoreV1().Events(v1.NamespaceDefault).List(metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod"})
	if err != nil {
		return []v1.Event{}, err
	}
	var podEvents []v1.Event
	for _, event := range eventList.Items {
		if strings.Contains(event.InvolvedObject.Name, name) {
			podEvents = append(podEvents, event)
		}
	}
	return podEvents, nil
}

// createLinuxJob creates a job which will run the provided command with a ubi8 image
func (tc *testContext) createLinuxJob(name string, command []string) (*batchv1.Job, error) {
	linuxNodeSelector := map[string]string{"beta.kubernetes.io/os": "linux"}
	return tc.createJob(name, ubi8Image, command, linuxNodeSelector, []v1.Toleration{})
}

//  createWinCurlerJob creates a Job to curl Windows server at given IP address
func (tc *testContext) createWinCurlerJob(vm testVM, winServerIP string) (*batchv1.Job, error) {
	winCurlerCommand := getWinCurlerCommand(winServerIP)
	winCurlerJob, err := tc.createWindowsServerJob("win-curler-"+vm.GetCredentials().GetInstanceId(), winCurlerCommand)
	return winCurlerJob, err
}

// getWinCurlerCommand generates a command to curl a Windows server from the given IP address
func getWinCurlerCommand(winServerIP string) []string {
	// This will continually try to read from the Windows Server. We have to try multiple times as the Windows container
	// takes some time to finish initial network setup.
	winCurlerCommand := []string{"powershell.exe", "-command", "for (($i =0), ($j = 0); $i -lt 10; $i++) { " +
		"$response = Invoke-Webrequest -UseBasicParsing -Uri " + winServerIP +
		"; $code = $response.StatusCode; echo \"GET returned code $code\";" +
		"If ($code -eq 200) {exit 0}; Start-Sleep -s 10;}; exit 1"}
	return winCurlerCommand
}

// createWindowsServerJob creates a job which will run the provided command with a Windows Server image
func (tc *testContext) createWindowsServerJob(name string, command []string) (*batchv1.Job, error) {
	windowsNodeSelector := map[string]string{"beta.kubernetes.io/os": "windows"}
	windowsTolerations := []v1.Toleration{{Key: "os", Value: "Windows", Effect: v1.TaintEffectNoSchedule}}
	return tc.createJob(name, windowsServerImage, command, windowsNodeSelector, windowsTolerations)
}

func (tc *testContext) createJob(name, image string, command []string, selector map[string]string,
	tolerations []v1.Toleration) (*batchv1.Job, error) {
	jobsClient := tc.kubeclient.BatchV1().Jobs(v1.NamespaceDefault)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-job",
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Tolerations:   tolerations,
					Containers: []v1.Container{
						{
							Name:            name,
							Image:           image,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         command,
						},
					},
					NodeSelector: selector,
				},
			},
		},
	}

	// Create job
	job, err := jobsClient.Create(job)
	if err != nil {
		return nil, err
	}
	return job, err
}

// deleteJob deletes the job with the given name
func (tc *testContext) deleteJob(name string) error {
	jobsClient := tc.kubeclient.BatchV1().Jobs(v1.NamespaceDefault)
	return jobsClient.Delete(name, &metav1.DeleteOptions{})
}

// waitUntilJobSucceeds will return an error if the job fails or reaches a timeout
func (tc *testContext) waitUntilJobSucceeds(name string) error {
	var job *batchv1.Job
	var err error
	for i := 0; i < retryCount; i++ {
		job, err = tc.kubeclient.BatchV1().Jobs(v1.NamespaceDefault).Get(name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if job.Status.Succeeded > 0 {
			return nil
		}
		if job.Status.Failed > 0 {
			events, _ := tc.getPodEvents(name)
			return errors.Errorf("job %v failed: %v", job, events)
		}
		time.Sleep(retryInterval)
	}
	events, _ := tc.getPodEvents(name)
	return errors.Errorf("job %v timed out: %v", job, events)
}
