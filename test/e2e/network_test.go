package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	require.NoError(t, testCtx.createNamespace(testCtx.workloadNamespace), "error creating test namespace")
	defer testCtx.deleteNamespace(testCtx.workloadNamespace)
	t.Run("East West Networking", testEastWestNetworking)
	t.Run("North south networking", testNorthSouthNetworking)
}

var (
	// ubi8Image is the name/location of the linux image we will use for testing
	ubi8Image = "registry.access.redhat.com/ubi8/ubi:latest"
	// retryCount is the amount of times we will retry an api operation
	retryCount = 60
	// retryInterval is the interval of time until we retry after a failure
	retryInterval = 5 * time.Second
)

// operatingSystem is used to specify an operating system to run workloads on
type operatingSystem string

const (
	linux   operatingSystem = "linux"
	windows operatingSystem = "windows"
)

// testEastWestNetworking deploys Windows and Linux pods, and tests that the pods can communicate
func testEastWestNetworking(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)
	defer testCtx.cleanup()

	testCases := []struct {
		name            string
		curlerOS        operatingSystem
		useClusterIPSVC bool
	}{
		{
			name:            "linux and windows",
			curlerOS:        linux,
			useClusterIPSVC: false,
		},
		{
			name:            "windows and windows",
			curlerOS:        windows,
			useClusterIPSVC: false,
		},
		{
			name:            "linux and windows through a clusterIP svc",
			curlerOS:        linux,
			useClusterIPSVC: true,
		},
		{
			name:            "windows and windows through a clusterIP svc",
			curlerOS:        windows,
			useClusterIPSVC: true,
		},
	}
	require.Greater(t, len(gc.nodes), 0, "test requires at least one Windows node to run")
	firstNodeAffinity, err := getAffinityForNode(&gc.nodes[0])
	require.NoError(t, err, "could not get affinity for node")

	for _, node := range gc.nodes {
		t.Run(node.Name, func(t *testing.T) {
			affinity, err := getAffinityForNode(&node)
			require.NoError(t, err, "could not get affinity for node")

			// Deploy a webserver pod on the new node. This is prone to timing out due to having to pull the Windows image
			// So trying multiple times
			var winServerDeployment *appsv1.Deployment
			for i := 0; i < deploymentRetries; i++ {
				winServerDeployment, err = testCtx.deployWindowsWebServer("win-webserver-"+strings.ToLower(node.Status.NodeInfo.MachineID), affinity)
				if err == nil {
					break
				}
			}
			require.NoError(t, err, "could not create Windows Server deployment")
			defer testCtx.deleteDeployment(winServerDeployment.Name)

			// Get the pod so we can use its IP
			winServerIP, err := testCtx.getPodIP(*winServerDeployment.Spec.Selector)
			require.NoError(t, err, "could not retrieve pod with selector %v", *winServerDeployment.Spec.Selector)

			// Create a clusterIP service which can be used to reach the Windows webserver
			intermediarySVC, err := testCtx.createService(winServerDeployment.Name, v1.ServiceTypeClusterIP, *winServerDeployment.Spec.Selector)
			require.NoError(t, err, "could not create service")
			defer testCtx.deleteService(intermediarySVC.Name)

			for _, tt := range testCases {
				t.Run(tt.name, func(t *testing.T) {
					var curlerJob *batchv1.Job
					// Depending on the test the curler pod will reach the Windows webserver either directly or through a
					// clusterIP service.
					endpointIP := winServerIP
					if tt.useClusterIPSVC {
						endpointIP = intermediarySVC.Spec.ClusterIP
					}

					// create the curler job based on the specified curlerOS
					if tt.curlerOS == linux {
						curlerCommand := []string{"bash", "-c", "yum update; yum install curl -y; curl " + endpointIP}
						curlerJob, err = testCtx.createLinuxJob("linux-curler-"+strings.ToLower(node.Status.NodeInfo.MachineID), curlerCommand)
						require.NoError(t, err, "could not create Linux job")
					} else if tt.curlerOS == windows {
						// Always deploy the Windows curler pod on the first node. Because we test scaling multiple
						// Windows nodes, this allows us to test that Windows pods can communicate with other Windows
						// pods located on both the same node, and other nodes.
						curlerJob, err = testCtx.createWinCurlerJob(strings.ToLower(node.Status.NodeInfo.MachineID),
							endpointIP, firstNodeAffinity)
						require.NoError(t, err, "could not create Windows job")
					} else {
						t.Fatalf("unsupported curler OS %s", tt.curlerOS)
					}
					defer testCtx.deleteJob(curlerJob.Name)

					err = testCtx.waitUntilJobSucceeds(curlerJob.Name)
					assert.NoError(t, err, "could not curl the Windows server")
				})
			}
		})
	}
}

// testNorthSouthNetworking deploys a Windows Server pod, and tests that we can network with it from outside the cluster
func testNorthSouthNetworking(t *testing.T) {
	testCtx, err := NewTestContext(t)
	require.NoError(t, err)

	// Use the 0th node to test
	require.NotEmpty(t, gc.nodes)
	node := gc.nodes[0]

	affinity, err := getAffinityForNode(&node)
	require.NoError(t, err, "Could not get affinity for node")

	// Deploy a webserver pod on the new node. This is prone to timing out due to having to pull the Windows image
	// So trying multiple times
	var winServerDeployment *appsv1.Deployment
	for i := 0; i < deploymentRetries; i++ {
		winServerDeployment, err = testCtx.deployWindowsWebServer("win-webserver-"+
			strings.ToLower(node.Status.NodeInfo.MachineID), affinity)
		if err == nil {
			break
		}
	}
	require.NoError(t, err, "could not create Windows Server deployment")
	defer testCtx.deleteDeployment(winServerDeployment.Name)

	// Assert that we can successfully GET the webserver
	err = testCtx.getThroughLoadBalancer(winServerDeployment)
	assert.NoError(t, err, "unable to GET the webserver through a load balancer")
}

// getThroughLoadBalancer does a GET request to the given webserver through a load balancer service
func (tc *testContext) getThroughLoadBalancer(webserver *appsv1.Deployment) error {
	// Create a load balancer svc to expose the webserver
	loadBalancer, err := tc.createService(webserver.Name, v1.ServiceTypeLoadBalancer, *webserver.Spec.Selector)
	if err != nil {
		return errors.Wrap(err, "could not create load balancer for Windows Server")
	}
	defer tc.deleteService(loadBalancer.Name)
	loadBalancer, err = tc.waitForLoadBalancerIngress(loadBalancer.Name)
	if err != nil {
		return errors.Wrap(err, "error waiting for load balancer ingress")
	}

	// Try and read from the webserver through the load balancer.
	// On AWS the LB ingress object contains a hostname, on Azure an IP.
	// The load balancer takes a fair amount of time, ~3 min, to start properly routing connections.
	var locator string
	if loadBalancer.Status.LoadBalancer.Ingress[0].Hostname != "" {
		locator = loadBalancer.Status.LoadBalancer.Ingress[0].Hostname
	} else if loadBalancer.Status.LoadBalancer.Ingress[0].IP != "" {
		locator = loadBalancer.Status.LoadBalancer.Ingress[0].IP
	} else {
		return errors.New("load balancer ingress object is empty")
	}
	resp, err := retryGET("http://" + locator)
	if err != nil {
		return fmt.Errorf("could not GET from load balancer: %v", loadBalancer)
	}
	resp.Body.Close()
	return nil
}

// retryGET will repeatedly try to GET from the provided URL until a 200 response is received or timeout
func retryGET(url string) (*http.Response, error) {
	var resp *http.Response
	var err error
	for i := 0; i < retryCount*3; i++ {
		resp, err = http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		time.Sleep(retryInterval)
	}
	return nil, fmt.Errorf("timed out trying to GET %s: %s", url, err)
}

// createService creates a new service of type serviceType for pods matching the label selector
func (tc *testContext) createService(name string, serviceType v1.ServiceType, selector metav1.LabelSelector) (*v1.Service, error) {
	svcSpec := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.ServiceSpec{
			Type: serviceType,
			Ports: []v1.ServicePort{
				{
					Protocol: v1.ProtocolTCP,
					Port:     80,
				},
			},
			Selector: selector.MatchLabels,
		}}
	return tc.kubeclient.CoreV1().Services(tc.workloadNamespace).Create(context.TODO(), svcSpec, metav1.CreateOptions{})
}

// waitForLoadBalancerIngress waits until the load balancer has an external hostname ready
func (tc *testContext) waitForLoadBalancerIngress(name string) (*v1.Service, error) {
	var svc *v1.Service
	var err error
	for i := 0; i < retryCount; i++ {
		svc, err = tc.kubeclient.CoreV1().Services(tc.workloadNamespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if len(svc.Status.LoadBalancer.Ingress) == 1 {
			return svc, nil
		}
		time.Sleep(retryInterval)
	}
	return nil, fmt.Errorf("timed out waiting for single ingress: %v", svc)
}

// deleteService deletes the service with the given name
func (tc *testContext) deleteService(name string) error {
	svcClient := tc.kubeclient.CoreV1().Services(tc.workloadNamespace)
	return svcClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
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

// createNamespace creates a namespace with the provided name
func (tc *testContext) createNamespace(name string) error {
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	_, err := tc.kubeclient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	return err
}

// deleteNamespace deletes the namespace with the provided name
func (tc *testContext) deleteNamespace(name string) error {
	return tc.kubeclient.CoreV1().Namespaces().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// deployWindowsWebServer creates a deployment with a single Windows Server pod, listening on port 80
func (tc *testContext) deployWindowsWebServer(name string, affinity *v1.Affinity) (*appsv1.Deployment, error) {
	// This will run a Server on the container, which can be reached with a GET request
	winServerCommand := []string{"pwsh.exe", "-command",
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
	deploymentsClient := tc.kubeclient.AppsV1().Deployments(tc.workloadNamespace)
	return deploymentsClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// getPodIP returns the IP of the pod that matches the label selector. If more than one pod match the
// selector, the function will return an error
func (tc *testContext) getPodIP(selector metav1.LabelSelector) (string, error) {
	selectorString := labels.Set(selector.MatchLabels).String()
	podList, err := tc.kubeclient.CoreV1().Pods(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
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

// getWindowsServerContainerImage gets the appropriate WindowsServer image based on VXLAN port
func (tc *testContext) getWindowsServerContainerImage() string {
	var windowsServerImage string
	// If we're using a custom VXLANPort we need to use 1909 or else use Windows Server 2019
	if tc.hasCustomVXLAN {
		windowsServerImage = "mcr.microsoft.com/powershell:lts-nanoserver-1909"
	} else {
		windowsServerImage = "mcr.microsoft.com/powershell:lts-nanoserver-1809"
	}
	return windowsServerImage
}

// createWindowsServerDeployment creates a deployment with a Windows Server 2019 container
func (tc *testContext) createWindowsServerDeployment(name string, command []string, affinity *v1.Affinity) (*appsv1.Deployment, error) {
	deploymentsClient := tc.kubeclient.AppsV1().Deployments(tc.workloadNamespace)
	replicaCount := int32(1)
	windowsServerImage := tc.getWindowsServerContainerImage()
	containerUserName := "ContainerAdministrator"
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
							// The default user for nanoserver image is ContainerUser.
							// Change user to ContainerAdministrator to run HttpListener in admin mode.
							SecurityContext: &v1.SecurityContext{
								WindowsOptions: &v1.WindowsSecurityContextOptions{
									RunAsUserName: &containerUserName,
								},
							},
							Command: command,
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
	deploy, err := deploymentsClient.Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "could not create deployment")
	}
	return deploy, err
}

// waitUntilDeploymentScaled will return nil if the deployment reaches the amount of replicas specified in its spec
func (tc *testContext) waitUntilDeploymentScaled(name string) error {
	var deployment *appsv1.Deployment
	var err error
	// Retry if we fail to get the deployment
	for i := 0; i < 5; i++ {
		deployment, err = tc.kubeclient.AppsV1().Deployments(tc.workloadNamespace).Get(context.TODO(),
			name,
			metav1.GetOptions{})
		if err != nil {
			return errors.Wrapf(err, "could not get deployment for %s", name)
		}
		if *deployment.Spec.Replicas == deployment.Status.AvailableReplicas {
			return nil
		}
		// The timeout limit for the image pull is 10m. So retry for a total of 10m
		// to give time for the deployment to come up.
		time.Sleep(2 * time.Minute)
	}
	events, _ := tc.getPodEvents(name)
	return errors.Errorf("timed out waiting for deployment %v to scale: %v", deployment, events)
}

// getPodEvents gets all events for any pod with the input in its name. Used for debugging purposes
func (tc *testContext) getPodEvents(name string) ([]v1.Event, error) {
	eventList, err := tc.kubeclient.CoreV1().Events(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
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
	return tc.createJob(name, ubi8Image, command, linuxNodeSelector, []v1.Toleration{}, nil)
}

//  createWinCurlerJob creates a Job to curl Windows server at given IP address
func (tc *testContext) createWinCurlerJob(name string, winServerIP string, affinity *v1.Affinity) (*batchv1.Job, error) {
	winCurlerCommand := getWinCurlerCommand(winServerIP)
	winCurlerJob, err := tc.createWindowsServerJob("win-curler-"+name, winCurlerCommand, affinity)
	return winCurlerJob, err
}

// getWinCurlerCommand generates a command to curl a Windows server from the given IP address
func getWinCurlerCommand(winServerIP string) []string {
	// This will continually try to read from the Windows Server. We have to try multiple times as the Windows container
	// takes some time to finish initial network setup.
	winCurlerCommand := []string{"pwsh.exe", "-command", "for (($i =0), ($j = 0); $i -lt 10; $i++) { " +
		"$response = Invoke-Webrequest -UseBasicParsing -Uri " + winServerIP +
		"; $code = $response.StatusCode; echo \"GET returned code $code\";" +
		"If ($code -eq 200) {exit 0}; Start-Sleep -s 10;}; exit 1"}
	return winCurlerCommand
}

// createWindowsServerJob creates a job which will run the provided command with a Windows Server image
func (tc *testContext) createWindowsServerJob(name string, command []string, affinity *v1.Affinity) (*batchv1.Job, error) {
	windowsNodeSelector := map[string]string{"beta.kubernetes.io/os": "windows"}
	windowsTolerations := []v1.Toleration{{Key: "os", Value: "Windows", Effect: v1.TaintEffectNoSchedule}}
	windowsServerImage := tc.getWindowsServerContainerImage()
	return tc.createJob(name, windowsServerImage, command, windowsNodeSelector, windowsTolerations, affinity)
}

// createJob creates a job on the cluster using the given parameters
func (tc *testContext) createJob(name, image string, command []string, selector map[string]string,
	tolerations []v1.Toleration, affinity *v1.Affinity) (*batchv1.Job, error) {
	jobsClient := tc.kubeclient.BatchV1().Jobs(tc.workloadNamespace)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-job",
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Affinity:      affinity,
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
	job, err := jobsClient.Create(context.TODO(), job, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return job, err
}

// deleteJob deletes the job with the given name
func (tc *testContext) deleteJob(name string) error {
	jobsClient := tc.kubeclient.BatchV1().Jobs(tc.workloadNamespace)
	return jobsClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// waitUntilJobSucceeds will return an error if the job fails or reaches a timeout
func (tc *testContext) waitUntilJobSucceeds(name string) error {
	var job *batchv1.Job
	var err error
	for i := 0; i < retryCount; i++ {
		job, err = tc.kubeclient.BatchV1().Jobs(tc.workloadNamespace).Get(context.TODO(), name, metav1.GetOptions{})
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
