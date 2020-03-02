package windowsmachineconfig

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/kubernetes"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	wmcv1alpha1 "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	certificates "k8s.io/api/certificates/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// remoteDir is the remote temporary directory that the e2e test uses
	remoteDir = "C:\\Temp\\"
	// winTemp is the default Windows temporary directory
	winTemp = "C:\\Windows\\Temp\\"
	// winCNIDir is the directory where the CNI files are placed
	winCNIDir = winTemp + "\\cni\\"
	// winCNIConfigPath is the CNI configuration file path on the Windows VM
	winCNIConfigPath = "C:\\Windows\\Temp\\cni\\config\\"
	// logDir is the remote kubernetes log director
	kLog = "C:\\k\\log\\"
	// cniConfigTemplate is the location of the cni.conf template file
	cniConfigTemplate = "templates/cni.template"
	// wgetIgnoreCertCmd is the remote location of the wget-ignore-cert.ps1 script
	wgetIgnoreCertCmd = remoteDir + "wget-ignore-cert.ps1"
	// e2eExecutable is the remote location of the WMCB e2e test binary
	e2eExecutable = remoteDir + "wmcb_e2e_test.exe"
	// unitExecutable is the remote location of the WMCB unit test binary
	unitExecutable = remoteDir + "wmcb_unit_test.exe"
	// hybridOverlayName is the name of the hybrid overlay executable
	hybridOverlayName = "hybrid-overlay.exe"
	// hybridOverExecutable is the remote location of the hybrid overlay binary
	hybridOverlayExecutable = remoteDir + hybridOverlayName
	// RetryCount is the amount of times we will retry an api operation
	RetryCount = 20
	// RetryInterval is the interval of time until we retry after a failure
	RetryInterval = 5 * time.Second
)

var log = logf.Log.WithName("controller_windowsmachineconfig")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new WindowsMachineConfig Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	// TODO: This should be moved out to validation
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Error(err, "error while getting clientset")
	}
	return &ReconcileWindowsMachineConfig{client: mgr.GetClient(), scheme: mgr.GetScheme(), k8sclientset: clientset}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("windowsmachineconfig-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &wmcv1alpha1.WindowsMachineConfig{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &wmcv1alpha1.WindowsMachineConfig{},
	})
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileWindowsMachineConfig implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileWindowsMachineConfig{}

// ReconcileWindowsMachineConfig reconciles a WindowsMachineConfig object
type ReconcileWindowsMachineConfig struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client        client.Client
	scheme        *runtime.Scheme
	cloudProvider cloudprovider.Cloud
	windowsVM     map[types.WindowsVM]bool
	k8sclientset  *kubernetes.Clientset
}

// windowsVM is a wrapper for the WindowsVM interface
type windowsVM struct {
	types.WindowsVM
}

// pkg encapsulates information about a package
type pkgInfo struct {
	// url is the URL of the package
	url string
	// sha is the SHA hash of the package
	sha string
	// shaType is the type of SHA used, example: 256 or 512
	shaType string
}

// Reconcile reads that state of the cluster for a WindowsMachineConfig object and makes changes based on the state read
// and what is in the WindowsMachineConfig.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileWindowsMachineConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling WindowsMachineConfig")

	// Fetch the WindowsMachineConfig instance
	instance := &wmcv1alpha1.WindowsMachineConfig{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	if r.cloudProvider == nil {
		// Get cloud provider specific info.
		// TODO: This should be moved to validation section.
		/*if instance.Spec.AWS == nil && instance.Spec.Azure == nil {
			return reconcile.Result{}, errors.New("both the supported cloud providers are nil")
		}*/
		// As of now think of AWS implementation.
		//if instance.Spec.AWS != nil {
		// We assume the cloud provider secret has been mounted to "~/.awscredentials` path
		r.cloudProvider, err = cloudprovider.CloudProviderFactory(os.Getenv("KUBECONFIG"),
			"~/.aws/credentials",
			"default",
			"", "", instance.Spec.InstanceType,
			"", "")
		//}
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("error instantianting cloud provider: %v", err)
		}
	}
	if r.windowsVM == nil {
		// populate the windowsVM map here from configmap as source of truth
		r.windowsVM = make(map[types.WindowsVM]bool)
	}
	if r.k8sclientset == nil {
		return reconcile.Result{}, nil
	}
	nodes, err := r.k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: "node.openshift.io/os_id=Windows"})
	if err != nil {
		return reconcile.Result{}, nil
	}
	fmt.Println(len(nodes.Items))
	// Get the current count of required number of Windows VMs
	currentCountOfWindowsVMs := len(nodes.Items) // As of now hardcoded to 1. We need to get the number of Windows VM node objects
	if instance.Spec.Replicas != currentCountOfWindowsVMs {
		if instance.Spec.Replicas == 0 {
			instance.Spec.Replicas = 2
		}
		if err := r.reconcileWindowsNodes(instance.Spec.Replicas, currentCountOfWindowsVMs); err != nil {
			return reconcile.Result{}, err
		}
	}

	// Get the node objects and do a node count.

	return reconcile.Result{}, nil
}

// reconcileWindowsNodes reconciles the Windows nodes to be
func (r *ReconcileWindowsMachineConfig) reconcileWindowsNodes(desired, current int) error {
	if desired < current {
		r.deleteWindowsVMs(current - desired)
	} else if desired > current {
		// Let's not requeue the result for now. We can get the list of errors
		r.createWindowsVMs(desired - current)
	}
	if err := r.reconcileCredentials(); err != nil {
		// Requeue
	}

	return nil
}

func (r *ReconcileWindowsMachineConfig) reconcileCredentials() error {
	// The windowsMachineConfig object has items related to the Windows objects that we can use to validate
	// the credentials.
	// Get the credentials from the configmap for all the objects we have in the Windows VM, if not go and update
	// the configmap.
	// Add a watch for the secrets, configmap here as well.
	return nil
}

func chooseRandomNode(windowsVMs map[types.WindowsVM]bool) types.WindowsVM {
	for windowsVM := range windowsVMs {
		return windowsVM
	}
	return nil
}

// deleteWindowsVMs returns the instance IDs of the successfully created Windows VMs
func (r *ReconcileWindowsMachineConfig) deleteWindowsVMs(count int) {
	// From the list of Windows VMs choose randomly count number of VMs. As of now sequential
	for i := 0; i < count; i++ {
		vmTobeDeleted := chooseRandomNode(r.windowsVM)
		if vmTobeDeleted.GetCredentials() == nil {
			continue
		}
		instancedID := vmTobeDeleted.GetCredentials().GetInstanceId()
		if len(instancedID) == 0 {
			continue
		}
		// Delete the Windows VM from cloud provider
		log.Info("deleting the Windows VM", instancedID)
		deleted := true
		if err := r.cloudProvider.DestroyWindowsVM(instancedID); err != nil {
			log.Error(err, "error while deleting windows VM %s", instancedID)
			deleted = false
		}
		if deleted {
			delete(r.windowsVM, vmTobeDeleted)
		}
	}
}

// TODO: Take this out, this is not needed, we assume kubelet is already available to be transferred.
var (
	// kubeNode contains the information about  the kubernetes node package for Windows
	kubeNode = pkgInfo{
		url:     "https://dl.k8s.io/v1.16.2/kubernetes-node-windows-amd64.tar.gz",
		sha:     "a88e7a1c6f72ea6073dbb4ddfe2e7c8bd37c9a56d94a33823f531e303a9915e7a844ac5880097724e44dfa7f4a9659d14b79cc46e2067f6b13e6df3f3f1b0f64",
		shaType: "sha512",
	}
)

func (r *ReconcileWindowsMachineConfig) createWindowsVMs(count int) {
	for i := 0; i < count; i++ {
		// Create Windows VM in the cloud provider
		createdVM, err := r.cloudProvider.CreateWindowsVM()
		if err != nil {
			log.Error(err, "error while creating windows VM", err)
			// return here
		}
		log.V(0).Info("created the Windows VM", createdVM.GetCredentials().GetInstanceId())
		log.V(0).Info("created the Windows VM ", createdVM.GetCredentials().GetPassword())
		// Copy paste wmcb e2e code for prepping the node :)
		vm := &windowsVM{createdVM}
		vm.configureWindowsVM()

		// Approve CSRs associated with the configured Windows VM.
		err = r.handleCSRs()
		if err != nil {
			log.Error(err, "error handling csr for the node")
		}

		// update the windowsVM interface
		if _, ok := r.windowsVM[createdVM]; !ok {
			r.windowsVM[createdVM] = true
		}
	}
}

func (vm *windowsVM) configureWindowsVM() {
	// Create the temp directory
	_, _, err := vm.Run(mkdirCmd(remoteDir), false)
	if err != nil {
		log.Error(err, "unable to create remote directory", remoteDir)
	}
	file := "/home/ravig/go/src/github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/powershell/wget-ignore-cert.ps1"
	if err := vm.CopyFile(file, remoteDir); err != nil {
		log.Error(err, "error while copying powershell script")
	}
	vm.runBootstrapper()
}

func (vm *windowsVM) runBootstrapper() {
	if err := vm.CopyFile("/home/ravig/go/src/github.com/openshift/windows-machine-config-bootstrapper/wmcb.exe", remoteDir); err != nil {
		log.Error(err, "error while copying wmcb binary")
	}
	err := vm.initializeTestBootstrapperFiles()
	if err != nil {
		log.Error(err, "error logging message")
	}
	_, _, err = vm.Run(remoteDir+"\\wmcb.exe initialize-kubelet --ignition-file "+winTemp+"worker.ign --kubelet-path "+winTemp+"kubelet.exe", true)
	//log.Info("Stdout", stdout)
	//log.Info("Stderr", stderr)
	if err != nil {
		log.Error(err, "error running bootstrapper")
	}
}

// handleCSRs handles the approval of bootstrap and node CSRs
func (r *ReconcileWindowsMachineConfig) handleCSRs() error {
	// Handle the bootstrap CSR
	err := r.handleCSR("system:serviceaccount:openshift-machine-config-operator:node-bootstrapper")
	if err != nil {
		return fmt.Errorf("unable to handle bootstrap CSR: %v", err)
	}

	// Handle the node CSR
	// Note: for the product we want to get the node name from the instance information
	err = r.handleCSR("system:node:")
	if err != nil {
		return fmt.Errorf("unable to handle node CSR: %v", err)
	}

	return nil
}

//findCSR finds the CSR that matches the requestor filter
func (r *ReconcileWindowsMachineConfig) findCSR(requestor string) (*certificates.CertificateSigningRequest, error) {
	var foundCSR *certificates.CertificateSigningRequest
	// Find the CSR
	for retries := 0; retries < RetryCount; retries++ {
		csrs, err := r.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().List(metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("unable to get CSR list: %v", err)
		}
		if csrs == nil {
			time.Sleep(RetryInterval)
			continue
		}

		for _, csr := range csrs.Items {
			if !strings.Contains(csr.Spec.Username, requestor) {
				continue
			}
			var handledCSR bool
			for _, c := range csr.Status.Conditions {
				if c.Type == certificates.CertificateApproved || c.Type == certificates.CertificateDenied {
					handledCSR = true
					break
				}
			}
			if handledCSR {
				continue
			}
			foundCSR = &csr
			break
		}

		if foundCSR != nil {
			break
		}
		time.Sleep(RetryInterval)
	}

	if foundCSR == nil {
		return nil, fmt.Errorf("unable to find CSR with requestor %s", requestor)
	}
	return foundCSR, nil
}
// approve approves the given CSR if it has not already been approved
// Based on https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/certificates/certificates.go#L237
func (r *ReconcileWindowsMachineConfig) approve(csr *certificates.CertificateSigningRequest) error {
	// Check if the certificate has already been approved
	for _, c := range csr.Status.Conditions {
		if c.Type == certificates.CertificateApproved {
			return nil
		}
	}

	// Approve the CSR
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Ensure we get the current version
		csr, err := r.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().Get(
			csr.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Add the approval status condition
		csr.Status.Conditions = append(csr.Status.Conditions, certificates.CertificateSigningRequestCondition{
			Type:           certificates.CertificateApproved,
			Reason:         "WMCBe2eTestRunnerApprove",
			Message:        "This CSR was approved by WMCB e2e test runner",
			LastUpdateTime: metav1.Now(),
		})

		_, err = r.k8sclientset.CertificatesV1beta1().CertificateSigningRequests().UpdateApproval(csr)
		return err
	})
}


// handleCSR finds the CSR based on the requestor filter and approves it
func (r *ReconcileWindowsMachineConfig) handleCSR(requestorFilter string) error {
	csr, err := r.findCSR(requestorFilter)
	if err != nil {
		return fmt.Errorf("error finding CSR for %s: %v", requestorFilter, err)
	}

	if err = r.approve(csr); err != nil {
		return fmt.Errorf("error approving CSR for %s: %v", requestorFilter, err)
	}

	return nil
}

// initializeTestBootstrapperFiles initializes the files required for initialize-kubelet
func (vm *windowsVM) initializeTestBootstrapperFiles() error {
	// Download and extract the kube package on the VM
	err := vm.remoteDownloadExtract(kubeNode, remoteDir+"kube.tar.gz", remoteDir)
	if err != nil {
		return fmt.Errorf("unable to download kube package: %v", err)
	}

	// Copy kubelet.exe to C:\Windows\Temp\
	_, _, err = vm.Run("cp "+remoteDir+"kubernetes\\node\\bin\\kubelet.exe "+winTemp, true)
	if err != nil {
		return fmt.Errorf("unable to copy kubelet.exe to %s", winTemp)
	}
	// TODO: As of now, getting cluster address from the environment var, remove it.
	// Download the worker ignition to C:\Windows\Tenp\ using the script that ignores the server cert
	ClusterAddress := os.Getenv("CLUSTER_ADDR")
	fmt.Println("ravig", wgetIgnoreCertCmd+" -server https://api-int."+ClusterAddress+":22623/config/worker"+" -output "+winTemp+"worker.ign")
	stdout, stderr, err := vm.Run(wgetIgnoreCertCmd+" -server https://api-int."+ClusterAddress+":22623/config/worker"+" -output "+winTemp+"worker.ign", true)
	fmt.Printf("stdout %v", stdout)
	fmt.Printf("Stderr %v", stderr)
	if err != nil {
		fmt.Printf("ravig %v", err)
		return fmt.Errorf("unable to download worker.ign: %v", err)
	}

	return nil
}

// remoteDownload downloads the tar file in url to the remoteDownloadFile location and checks if the SHA matches
func (vm *windowsVM) remoteDownload(pkg pkgInfo, remoteDownloadFile string) error {
	_, stderr, err := vm.Run("if (!(Test-Path "+remoteDownloadFile+")) { wget "+pkg.url+" -o "+remoteDownloadFile+" }",
		true)
	if err != nil {
		return fmt.Errorf("unable to download %s: %v\n%s", pkg.url, err, stderr)
	}

	if pkg.sha == "" {
		return nil
	}

	// Perform a checksum check
	stdout, _, err := vm.Run("certutil -hashfile "+remoteDownloadFile+" "+pkg.shaType, true)
	if err != nil {
		return fmt.Errorf("unable to check SHA of %s: %v", remoteDownloadFile, err)
	}
	if !strings.Contains(stdout, pkg.sha) {
		return fmt.Errorf("package %s SHA does not match: %v\n%s", remoteDownloadFile, err, stdout)
	}

	return nil
}

// remoteDownloadExtract downloads the tar file in url to the remoteDownloadFile location, checks if the SHA matches and
//  extracts the files to the remoteExtractDir directory
func (vm *windowsVM) remoteDownloadExtract(pkg pkgInfo, remoteDownloadFile, remoteExtractDir string) error {
	// Download the file from the URL
	err := vm.remoteDownload(pkg, remoteDownloadFile)
	if err != nil {
		return fmt.Errorf("unable to download %s: %v", pkg.url, err)
	}

	// Extract files from the archive
	_, stderr, err := vm.Run("tar -xf "+remoteDownloadFile+" -C "+remoteExtractDir, true)
	if err != nil {
		return fmt.Errorf("unable to extract %s: %v\n%s", remoteDownloadFile, err, stderr)
	}
	return nil
}

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
