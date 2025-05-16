package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	config "github.com/openshift/api/config/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	kexec "k8s.io/utils/exec"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/test/e2e/windows"
)

// testNetwork runs all the cluster and node network tests
func testNetwork(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfMachineNodes, false, false)
	assert.NoError(t, err, "timed out waiting for Windows Machine nodes")
	err = tc.waitForConfiguredWindowsNodes(gc.numberOfBYOHNodes, false, true)
	assert.NoError(t, err, "timed out waiting for BYOH Windows nodes")

	for _, node := range gc.allNodes() {
		require.NoError(t, tc.startPacketTrace(&node))
	}

	t.Run("East West Networking", tc.testEastWestNetworking)
	t.Run("North south networking", tc.testNorthSouthNetworking)
	t.Run("Pod DNS Resolution", tc.testPodDNSResolution)

	for _, node := range gc.allNodes() {
		require.NoError(t, tc.stopPacketTrace(&node))
	}
}

var (
	// ubi8Image is the name/location of the linux image we will use for testing
	ubi8Image = "registry.access.redhat.com/ubi8/ubi-minimal:latest"
	// retryCount is the amount of times we will retry an api operation
	retryCount = 120
	// retryInterval is the interval of time until we retry after a failure
	retryInterval = 5 * time.Second
)

// operatingSystem is used to specify an operating system to run workloads on
type operatingSystem string

const (
	linux         operatingSystem = "linux"
	windowsOS     operatingSystem = "windows"
	powerShellExe                 = "pwsh.exe"
)

// startPacketTrace starts a packetmon packet trace on the given Windows node
func (tc *testContext) startPacketTrace(node *v1.Node) error {
	addr, err := controllers.GetAddress(node.Status.Addresses)
	if err != nil {
		return err
	}
	// Arguments are adapted from https://github.com/microsoft/SDN/blob/master/Kubernetes/windows/debug/startpacketcapture.cmd
	cmd := "mkdir C:\\debug; pktmon start --file-name C:\\debug\\pktmon.etl --capture --trace " +
		"-p 0c885e0d-6eb6-476c-a048-2457eed3a5c1 -p Microsoft-Windows-TCPIP -l 5 " +
		"-p 80CE50DE-D264-4581-950D-ABADEEE0D340 -p D0E4BC17-34C7-43fc-9A72-D89A59D6979A " +
		"-p 93f693dc-9163-4dee-af64-d855218af242 -p 564368D6-577B-4af5-AD84-1C54464848E6 " +
		"-p Microsoft-Windows-Hyper-V-VfpExt -p microsoft-windows-winnat -p AE3F6C6D-BF2A-4291-9D07-59E661274EE3 -k 0xffffffff -l 6 " +
		"-p 9B322459-4AD9-4F81-8EEA-DC77CDD18CA6 -k 0xffffffff -l 6 -p 0c885e0d-6eb6-476c-a048-2457eed3a5c1 -l 6 -p Microsoft-Windows-Hyper-V-VmSwitch -l 5"
	_, err = tc.runPowerShellSSHJob("packet-cap-start", cmd, addr)
	return err
}

// stopPacketTrace stops a packetmon packet trace on the given Windows node, and saves the trace to the test artifacts
func (tc *testContext) stopPacketTrace(node *v1.Node) error {
	// The approach to collecting the logs from the Windows Node is heavily borrowed from oc must-gather
	// We are creating a pod with two containers, when the first container which is collecting the trace completes, we
	// know we are set to copy the trace from the second container. Both containers have the host volume mounted.
	trueBool := true
	aff, err := getAffinityForNode(node)
	if err != nil {
		return err
	}
	hostPathType := v1.HostPathDirectory
	administratorUser := "NT AUTHORITY\\SYSTEM"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "must-gather-",
			Labels: map[string]string{
				"app": "must-gather",
			},
		},
		Spec: v1.PodSpec{
			PriorityClassName:  "system-cluster-critical",
			RestartPolicy:      v1.RestartPolicyNever,
			ServiceAccountName: tc.workloadNamespace,
			SecurityContext: &v1.PodSecurityContext{
				WindowsOptions: &v1.WindowsSecurityContextOptions{
					HostProcess:   &trueBool,
					RunAsUserName: &administratorUser,
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "host",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "C:\\debug",
							Type: &hostPathType,
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:            "gather",
					Image:           tc.getWindowsServerContainerImage(),
					ImagePullPolicy: v1.PullIfNotPresent,
					// Complete the trace, and export an additional copy in the pcapng format
					Command: []string{"powershell.exe", "-command", "pktmon stop; pktmon etl2pcap C:\\debug\\pktmon.etl --out C:\\debug\\trace.pcapng"},
					Env: []v1.EnvVar{
						{
							Name: "NODE_NAME",
							ValueFrom: &v1.EnvVarSource{
								FieldRef: &v1.ObjectFieldSelector{
									FieldPath: "spec.nodeName",
								},
							},
						},
						{
							Name: "POD_NAME",
							ValueFrom: &v1.EnvVarSource{
								FieldRef: &v1.ObjectFieldSelector{
									FieldPath: "metadata.name",
								},
							},
						},
					},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "host",
							MountPath: "C:\\debug",
							ReadOnly:  false,
						},
					},
				},
				{
					Name:            "copy",
					Image:           tc.getWindowsServerContainerImage(),
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"powershell.exe", "-command", "while ($true) {Start-Sleep -Seconds 1}"},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "host",
							MountPath: "C:\\debug",
							ReadOnly:  false,
						},
					},
				},
			},
			HostNetwork: true,
			Affinity:    aff,
			Tolerations: []v1.Toleration{
				{
					Operator: "Exists",
				},
			},
		},
	}
	createdPod, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	defer tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).Delete(context.TODO(), createdPod.GetName(), metav1.DeleteOptions{})
	err = wait.PollUntilContextTimeout(context.TODO(), 5*time.Second, 1*time.Minute, true,
		func(ctx context.Context) (done bool, err error) {
			return tc.isGatherDone(createdPod)
		})

	// Copy the packet trace from the pod to $ARTIFACT_DIR/nodes/$nodename/trace
	nodeDir := filepath.Join(os.Getenv("ARTIFACT_DIR"), "nodes", node.Name)
	err = os.MkdirAll(filepath.Join(nodeDir), os.ModePerm)
	if err != nil {
		return err
	}
	cmd := exec.Command("oc", "cp", "-n", tc.workloadNamespace, "-c", "copy",
		createdPod.GetName()+":/debug/", filepath.Join(nodeDir, "trace"))

	retries := 10
	retryDelay := 5 * time.Second

	for i := 0; i < retries; i++ {
		out, err := cmd.Output()
		if err != nil {
			var exitError *exec.ExitError
			stderr := ""
			if errors.As(err, &exitError) {
				stderr = string(exitError.Stderr)
			}
			err = fmt.Errorf("oc cp failed with exit code %s and output: %s: %s", err, string(out), stderr)
		}
		time.Sleep(retryDelay)
	}
	return err
}

// isGatherDone returns true when the container named "gather" in the given pod has terminated successfully
// taken from oc must-gather code
func (tc *testContext) isGatherDone(pod *v1.Pod) (bool, error) {
	var err error
	if pod, err = tc.client.K8s.CoreV1().Pods(pod.Namespace).Get(context.TODO(), pod.Name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return true, err
		}
		return false, nil
	}
	var state *v1.ContainerState
	for _, cstate := range pod.Status.ContainerStatuses {
		if cstate.Name == "gather" {
			state = &cstate.State
			break
		}
	}

	// missing status for gather container => timeout in the worst case
	if state == nil {
		return false, nil
	}

	if state.Terminated != nil {
		if state.Terminated.ExitCode == 0 {
			return true, nil
		}
		return true, &kexec.CodeExitError{
			Err:  fmt.Errorf("%s/%s unexpectedly terminated: exit code: %v, reason: %s, message: %s", pod.Namespace, pod.Name, state.Terminated.ExitCode, state.Terminated.Reason, state.Terminated.Message),
			Code: int(state.Terminated.ExitCode),
		}
	}
	return false, nil
}

// testEastWestNetworking deploys Windows and Linux pods, and tests that the pods can communicate
func (tc *testContext) testEastWestNetworking(t *testing.T) {
	testCases := []struct {
		name            string
		curlerOS        operatingSystem
		webserverOS     operatingSystem
		useClusterIPSVC bool
	}{
		{
			name:            "linux curling windows",
			curlerOS:        linux,
			webserverOS:     windowsOS,
			useClusterIPSVC: false,
		},
		{
			name:            "windows curling windows",
			curlerOS:        windowsOS,
			webserverOS:     windowsOS,
			useClusterIPSVC: false,
		},
		{
			name:            "linux curling windows through a clusterIP svc",
			curlerOS:        linux,
			webserverOS:     windowsOS,
			useClusterIPSVC: true,
		},
		{
			name:            "windows curling windows through a clusterIP svc",
			curlerOS:        windowsOS,
			webserverOS:     windowsOS,
			useClusterIPSVC: true,
		},
		{
			name:            "windows curling linux through a clusterIP svc",
			curlerOS:        windowsOS,
			webserverOS:     linux,
			useClusterIPSVC: true,
		},
		{
			name:            "windows curling linux",
			curlerOS:        windowsOS,
			webserverOS:     linux,
			useClusterIPSVC: false,
		},
	}
	require.Greater(t, len(gc.allNodes()), 0, "test requires at least one Windows node to run")
	firstNodeAffinity, err := getAffinityForNode(&gc.allNodes()[0])
	require.NoError(t, err, "could not get affinity for node")

	linuxServerDeployment, err := tc.deployLinuxWebServer()
	require.NoError(t, err)
	defer tc.collectDeploymentLogs(linuxServerDeployment)
	defer tc.deleteDeployment(linuxServerDeployment.GetName())
	linuxServerClusterIP, err := tc.createService(linuxServerDeployment.GetName(), 8080, v1.ServiceTypeClusterIP,
		*linuxServerDeployment.Spec.Selector)
	require.NoError(t, err)
	defer tc.deleteService(linuxServerClusterIP.GetName())
	require.NoError(t, err)
	for _, node := range gc.allNodes() {
		t.Run(node.Name, func(t *testing.T) {
			affinity, err := getAffinityForNode(&node)
			require.NoError(t, err, "could not get affinity for node")

			// Deploy a webserver pod on the new node. This is prone to timing out due to having to pull the Windows image
			// So trying multiple times
			var winServerDeployment *appsv1.Deployment
			for i := 0; i < deploymentRetries; i++ {
				winServerDeployment, err = tc.deployWindowsWebServer("win-webserver-"+strings.ToLower(
					node.Status.NodeInfo.MachineID), affinity, nil)
				if err == nil {
					break
				}
			}
			require.NoError(t, err, "could not create Windows Server deployment")
			defer tc.collectDeploymentLogs(winServerDeployment)
			defer tc.deleteDeployment(winServerDeployment.Name)

			// Create a clusterIP service which can be used to reach the Windows webserver
			intermediarySVC, err := tc.createService(winServerDeployment.Name, 80, v1.ServiceTypeClusterIP, *winServerDeployment.Spec.Selector)
			require.NoError(t, err, "could not create service")
			defer tc.deleteService(intermediarySVC.Name)

			for _, tt := range testCases {
				t.Run(tt.name, func(t *testing.T) {
					var curlerJob *batchv1.Job
					// Depending on the test the curler pod will reach the webserver either directly or through a
					// clusterIP service.
					var endpointIP string
					if tt.webserverOS == windowsOS {
						if tt.useClusterIPSVC {
							endpointIP = intermediarySVC.Spec.ClusterIP
						} else {
							// Get the pod so we can use its IP
							endpointIP, err = tc.getPodIP(*winServerDeployment.Spec.Selector)
							require.NoError(t, err, "could not retrieve pod with selector %v", *winServerDeployment.Spec.Selector)
						}
					} else {
						if tt.useClusterIPSVC {
							endpointIP = linuxServerClusterIP.Spec.ClusterIP
						} else {
							linuxServerIP, err := tc.getPodIP(*linuxServerDeployment.Spec.Selector)
							require.NoError(t, err)

							endpointIP = linuxServerIP + ":8080"
						}
					}

					// create the curler job based on the specified curlerOS
					if tt.curlerOS == linux {
						curlerJob, err = tc.createLinuxCurlerJob(strings.ToLower(node.Status.NodeInfo.MachineID),
							endpointIP, false)
						require.NoError(t, err, "could not create Linux job")
					} else if tt.curlerOS == windowsOS {
						// Always deploy the Windows curler pod on the first node. Because we test scaling multiple
						// Windows nodes, this allows us to test that Windows pods can communicate with other Windows
						// pods located on both the same node, and other nodes.
						curlerJob, err = tc.createWinCurlerJob(strings.ToLower(node.Status.NodeInfo.MachineID),
							endpointIP, firstNodeAffinity)
						require.NoError(t, err, "could not create Windows job")
					} else {
						t.Fatalf("unsupported curler OS %s", tt.curlerOS)
					}
					defer tc.deleteJob(curlerJob.Name)

					_, err = tc.waitUntilJobSucceeds(curlerJob.Name)
					assert.NoErrorf(t, err, "error curling endpoint %s from %s pod", endpointIP, tt.curlerOS)
				})
			}
			t.Run("service DNS resolution", func(t *testing.T) {
				serviceDNS := fmt.Sprintf("%s.%s.svc.cluster.local", intermediarySVC.GetName(),
					intermediarySVC.GetNamespace())
				nodeAffinity, err := getAffinityForNode(&node)
				require.NoError(t, err)
				curler, err := tc.createWinCurlerJob(strings.ToLower(node.Status.NodeInfo.MachineID)+"-dns-test",
					serviceDNS, nodeAffinity)
				require.NoError(t, err)
				defer tc.deleteJob(curler.GetName())
				_, err = tc.waitUntilJobSucceeds(curler.Name)
				assert.NoError(t, err)
			})
		})
	}
}

// testPodDNSResolution test the DNS resolution in a Windows pod
func (tc *testContext) testPodDNSResolution(t *testing.T) {
	// This test has 25% failure rate in CI.  WINC-743 has been created to address
	// this issue. Until we address WINC-743, this test has been temporarily
	// disabled.
	t.Skip("Pod DNS resolution test is disabled, pending flake investigation")

	// the following DNS resolution tests use curl.exe because nslookup tool is not present
	// in the selected container image for the e2e tests. Ideally, we would use the native
	// PowerShell cmdlet Resolve-DnsName, but it is not present either.
	// TODO: Use a compatible container image for the e2e test suite that includes the
	//  PowerShell cmdlet Resolve-DnsName.
	winJob, err := tc.createWindowsServerJob("win-dns-tester",
		// curl'ing with --head flag to fetch only the headers as a lightweight alternative
		"curl.exe --head -k https://www.openshift.com",
		nil)
	require.NoError(t, err, "could not create Windows tester job")
	defer tc.deleteJob(winJob.Name)
	_, err = tc.waitUntilJobSucceeds(winJob.Name)
	assert.NoError(t, err, "Windows tester job failed")
}

// collectDeploymentLogs collects logs of a deployment to the Artifacts directory
func (tc *testContext) collectDeploymentLogs(deployment *appsv1.Deployment) error {
	// map of labels expected to be on each pod in the deployment
	matchLabels := deployment.Spec.Selector.MatchLabels
	if len(matchLabels) == 0 {
		return fmt.Errorf("deployment pod label map is empty")
	}
	var keyValPairs []string
	for key, value := range matchLabels {
		keyValPairs = append(keyValPairs, key+"="+value)
	}
	labelSelector := strings.Join(keyValPairs, ",")
	_, err := tc.gatherPodLogs(labelSelector)
	return err
}

// getLogs uses a label selector and returns the logs associated with each pod
func (tc *testContext) getLogs(podLabelSelector string) (string, error) {
	if podLabelSelector == "" {
		return "", fmt.Errorf("pod label selector is empty")
	}
	pods, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: podLabelSelector})
	if err != nil {
		return "", fmt.Errorf("error getting pod list: %w", err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("expected at least 1 pod and found 0")
	}
	var logs string
	for _, pod := range pods.Items {
		logStream, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).GetLogs(pod.Name,
			&v1.PodLogOptions{}).Stream(context.TODO())
		if err != nil {
			return "", fmt.Errorf("error getting pod logs: %w", err)
		}
		podLogs, err := ioutil.ReadAll(logStream)
		if err != nil {
			logStream.Close()
			return "", fmt.Errorf("error reading pod logs: %w", err)
		}
		// appending the pod logs onto the existing logs
		logs += fmt.Sprintf("%s: %s\n", pod.Name, podLogs)
		logStream.Close()
	}
	return logs, nil
}

// testNorthSouthNetworking deploys a Windows Server pod, and tests that we can network with it from outside the cluster
func (tc *testContext) testNorthSouthNetworking(t *testing.T) {
	// Ignore the application ingress load balancer test for None and vSphere platforms as it has to be created manually
	// https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/networking/configuring-ingress-cluster-traffic#configuring-ingress-cluster-traffic-ingress-controller
	if tc.CloudProvider.GetType() == config.VSpherePlatformType ||
		tc.CloudProvider.GetType() == config.NutanixPlatformType ||
		tc.CloudProvider.GetType() == config.NonePlatformType {
		t.Skipf("NorthSouthNetworking test is disabled for platform %s", tc.CloudProvider.GetType())
	}
	// Require at least one node to test
	require.NotEmpty(t, gc.allNodes())

	// Deploy a webserver pod on the new node. This is prone to timing out due to having to pull the Windows image
	// So trying multiple times
	var winServerDeployment *appsv1.Deployment
	var err error
	for i := 0; i < deploymentRetries; i++ {
		winServerDeployment, err = tc.deployWindowsWebServer("win-webserver", nil, nil)
		if err == nil {
			break
		}
	}
	require.NoError(t, err, "could not create Windows Server deployment")
	defer tc.deleteDeployment(winServerDeployment.GetName())
	if err := tc.collectDeploymentLogs(winServerDeployment); err != nil {
		log.Printf("error collecting deployment logs: %v", err)
	}
	// Assert that we can successfully GET the webserver
	err = tc.getThroughLoadBalancer(winServerDeployment)
	assert.NoError(t, err, "unable to GET the webserver through a load balancer")
}

// getThroughLoadBalancer does a GET request to the given webserver through a load balancer service
func (tc *testContext) getThroughLoadBalancer(webserver *appsv1.Deployment) error {
	// Create a load balancer svc to expose the webserver
	loadBalancer, err := tc.createService(webserver.Name, 80, v1.ServiceTypeLoadBalancer, *webserver.Spec.Selector)
	if err != nil {
		return fmt.Errorf("could not create load balancer for Windows Server: %w", err)
	}
	defer tc.deleteService(loadBalancer.Name)
	loadBalancer, err = tc.waitForLoadBalancerIngress(loadBalancer.Name)
	if err != nil {
		return fmt.Errorf("error waiting for load balancer ingress: %w", err)
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
		return fmt.Errorf("load balancer ingress object is empty")
	}
	resp, err := retryGET("http://" + locator)
	if err != nil {
		return fmt.Errorf("could not GET from load balancer: %v", err)
	}
	resp.Body.Close()
	return nil
}

// retryGET will repeatedly try to GET from the provided URL until a 200 response is received or timeout
func retryGET(url string) (*http.Response, error) {
	var resp *http.Response
	var err error
	log.Printf("GET %s", url)
	for i := 0; i < retryCount*3; i++ {
		resp, err = http.Get(url)
		log.Printf("%d: %v %v", i, resp, err)
		if err == nil && resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		time.Sleep(retryInterval)
	}
	return nil, fmt.Errorf("timed out trying to GET %s: %s", url, err)
}

// createService creates a new service of type serviceType for pods matching the label selector
func (tc *testContext) createService(name string, targetPort int32, serviceType v1.ServiceType,
	selector metav1.LabelSelector) (*v1.Service, error) {
	svcSpec := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-",
		},
		Spec: v1.ServiceSpec{
			Type: serviceType,
			Ports: []v1.ServicePort{
				{
					Protocol:   v1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt32(targetPort),
				},
			},
			Selector: selector.MatchLabels,
		}}
	return tc.client.K8s.CoreV1().Services(tc.workloadNamespace).Create(context.TODO(), svcSpec, metav1.CreateOptions{})
}

// waitForLoadBalancerIngress waits until the load balancer has an external hostname ready
func (tc *testContext) waitForLoadBalancerIngress(name string) (*v1.Service, error) {
	var svc *v1.Service
	var err error
	for i := 0; i < retryCount; i++ {
		svc, err = tc.client.K8s.CoreV1().Services(tc.workloadNamespace).Get(context.TODO(), name, metav1.GetOptions{})
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
	svcClient := tc.client.K8s.CoreV1().Services(tc.workloadNamespace)
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

// ensureNamespace checks if a namespace with the provided name exists and creates one if it does not with the given
// labels
func (tc *testContext) ensureNamespace(name string, labels map[string]string) error {
	// Check if the namespace exists
	ns, err := tc.client.K8s.CoreV1().Namespaces().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil {
		// ensure namespace was properly created with the required labels
		for k, expectedValue := range labels {
			if foundValue, found := ns.Labels[k]; found && expectedValue != foundValue {
				return fmt.Errorf("labels mismatch for namespace %s label: %s expected: %s found: %s",
					name, k, expectedValue, foundValue)
			}
		}
		// required labels present in namespace, nothing to do!
		return nil
	}

	// The namespace does not exists, so lets create it
	ns = &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	_, err = tc.client.K8s.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{})
	return err
}

// deleteNamespace deletes the namespace with the provided name
func (tc *testContext) deleteNamespace(name string) error {
	return tc.client.K8s.CoreV1().Namespaces().Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// deployWindowsWebServer creates a deployment with a single Windows Server pod, listening on port 80
func (tc *testContext) deployWindowsWebServer(name string, affinity *v1.Affinity,
	pvcToMount *v1.PersistentVolumeClaimVolumeSource) (*appsv1.Deployment, error) {
	var volumes []v1.Volume
	var volumeMounts []v1.VolumeMount
	if pvcToMount != nil {
		volumeName := pvcToMount.ClaimName
		volumeSource := v1.VolumeSource{PersistentVolumeClaim: pvcToMount}
		mountName := pvcToMount.ClaimName
		mountPath := "C:\\mnt\\storage"
		v, vm := getVolumeSpec(volumeName, volumeSource, mountName, mountPath)
		volumes, volumeMounts = append([]v1.Volume{}, v), append([]v1.VolumeMount{}, vm)
	}
	// This will run a Server on the container, which can be reached with a GET request
	winServerCommand := []string{
		powerShellExe,
		"-command",
		"$ipconfigOutput = ipconfig;" +
			"$listener = New-Object System.Net.HttpListener;" +
			"$listener.Prefixes.Add('http://*:80/');" +
			"$listener.Start();" +
			"Write-Host('Listening at http://*:80/');" +
			"while ($listener.IsListening) { " +
			"  $context = $listener.GetContext();" +
			"  $clientIPAddress = $context.Request.RemoteEndpoint.Address.ToString();" +
			"  $timestamp = Get-Date;" +
			"  Write-Host $clientIPAddress [$timestamp] $context.Request.HttpMethod $context.Request.Url.AbsolutePath" +
			"  'HTTP/'$context.Request.ProtocolVersion $context.Request.UserAgent;" +
			"  $response = $context.Response;" +
			"  $content='<html><body><H1>Windows Container Web Server</H1>'+$ipconfigOutput+'</body></html>'; " +
			"  $buffer = [System.Text.Encoding]::UTF8.GetBytes($content);" +
			"  $response.ContentLength64 = $buffer.Length;" +
			"  $response.OutputStream.Write($buffer, 0, $buffer.Length);" +
			"  $response.Close();" +
			"};"}
	winServerDeployment, err := tc.createWindowsServerDeployment(name, winServerCommand, affinity, volumes, volumeMounts)
	if err != nil {
		return nil, fmt.Errorf("could not create Windows deployment: %w", err)
	}
	// Wait until the server is ready to be queried
	err = tc.waitUntilDeploymentScaled(winServerDeployment.Name)
	if err != nil {
		tc.deleteDeployment(winServerDeployment.Name)
		return nil, fmt.Errorf("deployment was unable to scale: %w", err)
	}
	return winServerDeployment, nil
}

// deployLinuxWebServer deploys an apache webserver
func (tc *testContext) deployLinuxWebServer() (*appsv1.Deployment, error) {
	name := "linux-webserver"
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name,
		},
		Spec: appsv1.DeploymentSpec{
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
					Containers: []v1.Container{
						{
							Name:            "apache",
							Image:           "registry.access.redhat.com/ubi8/httpd-24:1-299",
							ImagePullPolicy: v1.PullIfNotPresent,
							Ports: []v1.ContainerPort{
								{
									Protocol:      v1.ProtocolTCP,
									ContainerPort: 8080,
								},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "html",
									ReadOnly:  true,
									MountPath: "/var/www/html/",
								},
							},
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("500m"),
									v1.ResourceMemory: resource.MustParse("256Mi"),
								},
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("500m"),
									v1.ResourceMemory: resource.MustParse("256Mi"),
								},
							},
						},
					},
					InitContainers: []v1.Container{
						{
							Name:    "index-creator",
							Image:   tc.toolsImage,
							Command: []string{"bash"},
							Args: []string{"-c",
								"echo '<!DOCTYPE html>" +
									"<html>" +
									"	<head>" +
									"		<title>Linux Webserver</title>" +
									"	</head>" +
									"	<body>" +
									"		<p>Linux pod IP: '$(POD_IP)'</p>" +
									"		<p>Linux host IP: '$(HOST_IP)'</p>" +
									"	</body>" +
									"</html>'" +
									" > /var/www/html/index.html"},
							Env: []v1.EnvVar{
								{
									Name: "POD_IP",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "status.podIP",
										},
									},
								},
								{
									Name: "HOST_IP",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "status.hostIP",
										},
									},
								},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "html",
									ReadOnly:  false,
									MountPath: "/var/www/html/",
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "html",
							VolumeSource: v1.VolumeSource{
								EmptyDir: &v1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	// Create Deployment
	deploy, err := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace).Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not create deployment: %w", err)
	}
	err = tc.waitUntilDeploymentScaled(deploy.GetName())
	return deploy, err
}

// getVolumeSpec returns a Volume and VolumeMount spec given the volume name, volume source, volume mount name and
// mount path.
func getVolumeSpec(volumeName string, volumeSource v1.VolumeSource, mountName string, mountPath string) (v1.Volume, v1.VolumeMount) {
	volumes := v1.Volume{
		Name:         volumeName,
		VolumeSource: volumeSource,
	}
	volumeMounts := v1.VolumeMount{
		Name:      mountName,
		MountPath: mountPath,
	}
	return volumes, volumeMounts
}

// deleteDeployment deletes the deployment with the given name
func (tc *testContext) deleteDeployment(name string) error {
	deploymentsClient := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace)
	return deploymentsClient.Delete(context.TODO(), name, metav1.DeleteOptions{})
}

// getPodIP returns the IP of the pod that matches the label selector. If more than one pod match the
// selector, the function will return an error
func (tc *testContext) getPodIP(selector metav1.LabelSelector) (string, error) {
	selectorString := labels.Set(selector.MatchLabels).String()
	podList, err := tc.client.K8s.CoreV1().Pods(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: selectorString})
	if err != nil {
		return "", err
	}
	if len(podList.Items) != 1 {
		return "", fmt.Errorf("expected one pod matching %s, but found %d", selectorString,
			len(podList.Items))
	}

	return podList.Items[0].Status.PodIP, nil
}

// getWindowsServerContainerImage gets the appropriate WindowsServer image based on the OS version
func (tc *testContext) getWindowsServerContainerImage() string {
	switch tc.windowsServerVersion {
	case windows.Server2019:
		return "mcr.microsoft.com/powershell:lts-nanoserver-1809"
	case windows.Server2022:
	default:
	}
	// the default container image must be compatible with Windows Server 2022
	return "mcr.microsoft.com/powershell:lts-nanoserver-ltsc2022"
}

// createWindowsServerDeployment creates a deployment with a Windows Server container. If affinity is nil then the
// number of replicas will be set to 3 to allow for network testing across nodes. If pvcToMount is set, the pod will
// mount the given pvc as a volume
func (tc *testContext) createWindowsServerDeployment(name string, command []string, affinity *v1.Affinity,
	volumes []v1.Volume, volumeMounts []v1.VolumeMount) (*appsv1.Deployment, error) {
	deploymentsClient := tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace)
	replicaCount := int32(1)
	// affinity being nil is a hint that the caller does not care which nodes the pods are deployed to
	if affinity == nil && volumes == nil {
		replicaCount = int32(3)
	}
	rcName, err := tc.getRuntimeClassName()
	if err != nil {
		return nil, err
	}
	windowsServerImage := tc.getWindowsServerContainerImage()
	containerUserName := "ContainerAdministrator"
	runAsNonRoot := false
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-deployment-",
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
					OS: &v1.PodOS{
						Name: v1.Windows,
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
								RunAsNonRoot: &runAsNonRoot,
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
							VolumeMounts: volumeMounts,
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("500m"),
									v1.ResourceMemory: resource.MustParse("500Mi"),
								},
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("500m"),
									v1.ResourceMemory: resource.MustParse("500Mi"),
								},
							},
						},
					},
					RuntimeClassName: &rcName,
					Volumes:          volumes,
				},
			},
		},
	}

	// Create Deployment
	deploy, err := deploymentsClient.Create(context.TODO(), deployment, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not create deployment: %w", err)
	}
	return deploy, err
}

// waitUntilDeploymentScaled will return nil if the deployment reaches the amount of replicas specified in its spec
func (tc *testContext) waitUntilDeploymentScaled(name string) error {
	var deployment *appsv1.Deployment
	var err error
	err = wait.PollUntilContextTimeout(context.TODO(), 5*time.Second, 5*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			deployment, err = tc.client.K8s.AppsV1().Deployments(tc.workloadNamespace).Get(context.TODO(), name,
				metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("could not get deployment for %s: %w", name, err)
			}
			if *deployment.Spec.Replicas == deployment.Status.AvailableReplicas {
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		events, _ := tc.getPodEvents(name)
		return fmt.Errorf("error waiting for deployment %v to scale: %v: %w", deployment, events, err)
	}
	return nil
}

// getPodEvents gets all events for any pod with the input in its name. Used for debugging purposes
func (tc *testContext) getPodEvents(name string) ([]v1.Event, error) {
	eventList, err := tc.client.K8s.CoreV1().Events(tc.workloadNamespace).List(context.TODO(), metav1.ListOptions{
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

// createLinuxCurlerJob creates a linux job to curl a specific endpoint. curl must be present in the container image.
func (tc *testContext) createLinuxCurlerJob(jobSuffix, endpoint string, continuous bool) (*batchv1.Job, error) {
	// Retries a failed curl attempt once to avoid flakes
	curlCommand := fmt.Sprintf(
		"curl -v %s;"+
			" if [ $? != 0 ]; then"+
			" sleep 60;"+
			" curl -v %s || exit 1;"+
			" fi",
		endpoint, endpoint)
	if continuous {
		// curl every 5 seconds indefinitely. If the endpoint is inaccessible for a few minutes, pod exits with an error
		curlCommand = fmt.Sprintf(
			"while true; do "+
				"%s;"+
				" sleep 5;"+
				" done",
			curlCommand)
	}
	return tc.createLinuxJob("linux-curler-"+jobSuffix, []string{"bash", "-c", curlCommand})
}

// createLinuxJob creates a job which will run the provided command with a ubi8 image
func (tc *testContext) createLinuxJob(name string, command []string) (*batchv1.Job, error) {
	return tc.createJob(name, ubi8Image, command, nil, nil, &v1.PodOS{Name: v1.Linux}, nil)
}

// createWinCurlerJob creates a Job to curl Windows server at given IP address
func (tc *testContext) createWinCurlerJob(name string, winServerIP string, affinity *v1.Affinity) (*batchv1.Job, error) {
	winCurlerCommand := tc.getWinCurlerCommand(winServerIP)
	winCurlerJob, err := tc.createWindowsServerJob("win-curler-"+name, winCurlerCommand, affinity)
	return winCurlerJob, err
}

// getWinCurlerCommand generates a PowerShell command to curl the given server URI
// The command will attempt to curl the server URI up to 25 times, waiting 5 seconds between each attempt
// resulting in a total timeout of 2 minutes. We have to try multiple times as a Windows container
// may take more time to pull image and finish initial network setup.
func (tc *testContext) getWinCurlerCommand(serverURI string) string {
	return "ipconfig;" +
		"for ($i = 1; $i -le 25; $i++) { " +
		" echo \"\";" +
		" echo \"Attempt #$i\";" +
		" echo \"Curling server URI: " + serverURI + "\";" +
		" $response = Invoke-WebRequest -UseBasicParsing -Uri " + serverURI + ";" +
		" $code = $response.StatusCode;" +
		" echo \"GET returned code $code\";" +
		" echo \"GET returned content:\";" +
		" echo $response.RawContent;" +
		" If ($code -eq 200) {" +
		"  exit 0" +
		" };" +
		" echo \"Waiting 5 seconds...\";" +
		" Start-Sleep -s 5;" +
		"};" +
		"echo \"Time exceeded, cannot reach " + serverURI + "\";" +
		"exit 1"
}

// createWindowsServerJob creates a job which will run the provided PowerShell command with a Windows Server image
func (tc *testContext) createWindowsServerJob(name, pwshCommand string, affinity *v1.Affinity) (*batchv1.Job, error) {
	rcName, err := tc.getRuntimeClassName()
	if err != nil {
		return nil, err
	}
	windowsOS := &v1.PodOS{Name: v1.Windows}
	windowsServerImage := tc.getWindowsServerContainerImage()
	command := []string{powerShellExe, "-command", pwshCommand}
	return tc.createJob(name, windowsServerImage, command, &rcName, affinity, windowsOS, nil)
}

// createJob creates a job on the cluster using the given parameters
func (tc *testContext) createJob(name, image string, command []string, runtimeClassName *string, affinity *v1.Affinity,
	os *v1.PodOS, pullSecret []v1.LocalObjectReference) (*batchv1.Job, error) {
	jobsClient := tc.client.K8s.BatchV1().Jobs(tc.workloadNamespace)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: name + "-job-",
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Affinity:         affinity,
					OS:               os,
					RestartPolicy:    v1.RestartPolicyNever,
					RuntimeClassName: runtimeClassName,
					Containers: []v1.Container{
						{
							Name:            name,
							Image:           image,
							ImagePullPolicy: v1.PullIfNotPresent,
							Command:         command,
						},
					},
					ImagePullSecrets: pullSecret,
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
	jobsClient := tc.client.K8s.BatchV1().Jobs(tc.workloadNamespace)
	propPolicy := metav1.DeletePropagationOrphan
	return jobsClient.Delete(context.TODO(), name, metav1.DeleteOptions{PropagationPolicy: &propPolicy})
}

// waitUntilJobSucceeds will return an error if the job fails or reaches a timeout and return logs on success
func (tc *testContext) waitUntilJobSucceeds(name string) (string, error) {
	var job *batchv1.Job
	var err error
	var labelSelector string
	// Timeout after 5 min
	for i := 0; i < 60; i++ {
		job, err = tc.client.K8s.BatchV1().Jobs(tc.workloadNamespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		labelSelector = "job-name=" + job.Name
		if job.Status.Succeeded > 0 {
			logs, err := tc.gatherPodLogs(labelSelector)
			if err != nil {
				log.Printf("Unable to get logs associated with pod %s: %v", labelSelector, err)
			}
			return logs, nil
		}
		if job.Status.Failed > 0 {
			_, err = tc.gatherPodLogs(labelSelector)
			if err != nil {
				log.Printf("Unable to get logs associated with pod %s: %v", labelSelector, err)
			}
			events, _ := tc.getPodEvents(name)
			return "", fmt.Errorf("job %v failed: %v", job, events)
		}
		time.Sleep(retryInterval)
	}
	_, err = tc.gatherPodLogs(labelSelector)
	if err != nil {
		log.Printf("Unable to get logs associated with pod %s: %v", labelSelector, err)
	}
	events, _ := tc.getPodEvents(name)
	return "", fmt.Errorf("job %v timed out: %v", job, events)
}

// gatherPodLogs writes the logs associated with the label selector of a given pod job or deployment to the Artifacts
// dir. Returns the written logs.
func (tc *testContext) gatherPodLogs(labelSelector string) (string, error) {
	podArtifacts := filepath.Join(os.Getenv("ARTIFACT_DIR"), "pods")
	podDir := filepath.Join(podArtifacts, labelSelector)
	err := os.MkdirAll(podDir, os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("error creating pod log collection directory %s: %w", podDir, err)
	}

	logs, err := tc.getLogs(labelSelector)
	if err != nil {
		return "", fmt.Errorf("unable to get logs for pod %s: %w", labelSelector, err)
	}
	podLogFile := fmt.Sprintf("%s.log", labelSelector)
	outputFile := filepath.Join(podDir, filepath.Base(podLogFile))
	logsErr := ioutil.WriteFile(outputFile, []byte(logs), os.ModePerm)
	if logsErr != nil {
		return "", fmt.Errorf("unable to write %s pod logs to %s: %w", labelSelector, outputFile, logsErr)
	}
	return logs, nil
}

// getRuntimeClassName returns the name of a runtime class for the given server version. If one does not exist on the
// cluster, it will be created.
func (tc *testContext) getRuntimeClassName() (string, error) {
	build, ok := windows.BuildNumber[tc.windowsServerVersion]
	if !ok {
		return "", fmt.Errorf("no known build number for server version %s", tc.windowsServerVersion)
	}
	rcName := "windows" + string(tc.windowsServerVersion)
	rc, err := tc.client.K8s.NodeV1().RuntimeClasses().Get(context.TODO(), rcName, metav1.GetOptions{})
	if err == nil {
		return rc.GetName(), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	rc = newRuntimeClass(rcName, build)
	rc, err = tc.client.K8s.NodeV1().RuntimeClasses().Create(context.TODO(), rc, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("error creating RuntimeClass: %w", err)
	}
	return rc.GetName(), nil
}

// newRuntimeClass returns a runtime class for the given windows build
func newRuntimeClass(name, windowsBuild string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		// the handler for containerd
		Handler: "runhcs-wcow-process",
		Scheduling: &nodev1.Scheduling{
			NodeSelector: map[string]string{
				v1.LabelOSStable:     string(v1.Windows),
				v1.LabelArchStable:   "amd64",
				v1.LabelWindowsBuild: windowsBuild,
			},
			Tolerations: []v1.Toleration{
				{
					Key:    "os",
					Value:  string(v1.Windows),
					Effect: v1.TaintEffectNoSchedule,
				},
				// K8s documentation suggests using lowercase "windows", but WMCO registers nodes with uppercase "Windows".
				{
					Key:    "os",
					Value:  "Windows",
					Effect: v1.TaintEffectNoSchedule,
				},
			},
		},
	}
}
