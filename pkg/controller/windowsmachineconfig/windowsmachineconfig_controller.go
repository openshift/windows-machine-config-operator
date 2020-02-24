package windowsmachineconfig

import (
	"context"
	"fmt"
	"os"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	wmcapi "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/nodeconfig"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// windowsOSLabel is the label that is applied by WMCB to identify the Windows nodes bootstrapped via WMCB
	WindowsOSLabel = "node.openshift.io/os_id=Windows"
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
	// TODO: This should be moved out to validation for reconciler struct.
	// 		Jira story: https://issues.redhat.com/browse/WINC-277
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
	// TODO: Add a predicate here. As of now, we get event notifications for all the WindowsMachineConfig objects, we
	//		want the predicate to filter the WMC object called `instance`
	//		Jira Story: https://issues.redhat.com/browse/WINC-282
	// Watch for changes to primary resource WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &wmcapi.WindowsMachineConfig{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &wmcapi.WindowsMachineConfig{},
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
	client client.Client
	scheme *runtime.Scheme
	// cloudProvider holds the information related to the cloud provider.
	cloudProvider cloudprovider.Cloud
	// windowsVM is map of interfaces that holds the information related to windows VMs created via the cloud provider.
	// the bool value represents the existence of the key so that we can confirm to _, ok pattern of golang maps
	windowsVMs map[types.WindowsVM]bool
	// k8sclientset holds the kube client that we can re-use for all kube objects other than custom resources.
	k8sclientset *kubernetes.Clientset
}

// Reconcile reads that state of the cluster for a WindowsMachineConfig object and makes changes based on the state read
// and what is in the WindowsMachineConfig.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileWindowsMachineConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling WindowsMachineConfig")

	// Fetch the WindowsMachineConfig instance
	instance := &wmcapi.WindowsMachineConfig{}
	err := r.client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
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
		// 		Jira story: https://issues.redhat.com/browse/WINC-277
		if instance.Spec.AWS == nil {
			return reconcile.Result{}, errors.New("AWS Cloud provider is nil, cannot proceed further")
		}
		// TODO: We assume the cloud provider secret has been mounted to "/etc/cloud/credentials` and private key is
		// 		present at "/etc/private-key.pem". We should have a validation method which checks for the existence
		// 		of these paths.
		//		Jira story: https://issues.redhat.com/browse/WINC-262
		// TODO: Add validation for the fields in the WindowsMachineConfig CRD.
		// 		Jira story: https://issues.redhat.com/browse/WINC-279
		r.cloudProvider, err = cloudprovider.CloudProviderFactory(os.Getenv("KUBECONFIG"),
			// We assume the credential path is `/etc/aws/credentials` mounted as a secret.
			wkl.CloudCredentialsPath,
			instance.Spec.AWS.CredentialAccountID,
			"", "", instance.Spec.InstanceType,
			instance.Spec.AWS.SSHKeyPair, wkl.PrivateKeyPath)

		if err != nil {
			return reconcile.Result{}, fmt.Errorf("error instantiating cloud provider: %v", err)
		}
	}
	if r.k8sclientset == nil {
		return reconcile.Result{}, nil
	}
	if r.windowsVMs == nil {
		// populate the windowsVM map here from configmap as source of truth
		r.windowsVMs = make(map[types.WindowsVM]bool)
	}
	// Get the current number of Windows VMs created by WMCO.
	// TODO: Get all the running Windows nodes in the cluster
	//		jira story: https://issues.redhat.com/browse/WINC-280
	windowsNodes, err := r.k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: WindowsOSLabel})
	if err != nil {
		return reconcile.Result{}, nil
	}

	// Get the current count of required number of Windows VMs
	currentCountOfWindowsVMs := len(windowsNodes.Items)
	if instance.Spec.Replicas != currentCountOfWindowsVMs {
		// TODO: We're swallowing the error which is a bad pattern, let's clean this up
		//		Jira story: https://issues.redhat.com/browse/WINC-266
		if !r.reconcileWindowsNodes(instance.Spec.Replicas, currentCountOfWindowsVMs) {
			return reconcile.Result{}, nil
		}
	}

	return reconcile.Result{}, nil
}

// reconcileWindowsNodes reconciles the Windows nodes so that required number of the Windows nodes are present in the
// cluster
func (r *ReconcileWindowsMachineConfig) reconcileWindowsNodes(desired, current int) bool {
	if desired < current {
		return r.deleteWindowsVMs(current - desired)
	} else if desired > current {
		// Let's not requeue the result for now. We can get the list of errors
		return r.createWindowsWorkerNodes(desired - current)
	} else if desired == current {
		return true
	}
	return false
}

// chooseRandomNode chooses one of the windows nodes randomly. The randomization is coming from golang maps since you
// cannot assume the maps to be ordered.
func chooseRandomNode(windowsVMs map[types.WindowsVM]bool) types.WindowsVM {
	for windowsVM := range windowsVMs {
		return windowsVM
	}
	return nil
}

// deleteWindowsVMs deletes the required number of Windows VMs from the cluster and returns a bool indicating the
// status of deletion. This method will return false if any of the VMs fail to get deleted.
// TODO: This method should return a slice of errors that we collected.
//		Jira story: https://issues.redhat.com/browse/WINC-266
func (r *ReconcileWindowsMachineConfig) deleteWindowsVMs(count int) bool {
	var errs []error
	// From the list of Windows VMs choose randomly count number of VMs.
	for i := 0; i < count; i++ {
		// Choose of the Windows worker nodes randomly
		vmTobeDeleted := chooseRandomNode(r.windowsVMs)
		if vmTobeDeleted.GetCredentials() == nil {
			errs = append(errs, errors.New("One of the VM deletions failed, will reconcile..."))
			continue
		}

		// Get the instance associated with the Windows worker node
		instancedID := vmTobeDeleted.GetCredentials().GetInstanceId()
		if len(instancedID) == 0 {
			errs = append(errs, errors.New("One of the VM deletions failed, will reconcile..."))
			continue
		}

		// Delete the Windows VM from cloud provider
		log.Info(fmt.Sprintf("deleting the Windows VM: %s", instancedID))
		if err := r.cloudProvider.DestroyWindowsVM(instancedID); err != nil {
			log.Error(err, "error while deleting windows VM: %s", instancedID)
			errs = append(errs, errors.Wrap(err, "One of the VM deletions failed, will reconcile"))
		}
		delete(r.windowsVMs, vmTobeDeleted)
	}

	// If any of the Windows VM fails to get deleted consider this as a failure and return false
	if len(errs) > 0 {
		return false
	}
	return true
}

// createWindowsVMs creates the required number of windows Windows VM and configure them to make
// them a worker node
// TODO: This method should return a slice of errors that we collected.
//		Jira story: https://issues.redhat.com/browse/WINC-266
func (r *ReconcileWindowsMachineConfig) createWindowsWorkerNodes(count int) bool {
	var errs []error
	for i := 0; i < count; i++ {
		// Create Windows VM in the cloud provider
		createdVM, err := r.cloudProvider.CreateWindowsVM()
		if err != nil {
			errs = append(errs, errors.Wrap(err, "error creating windows VM"))
			log.Error(err, "error creating windows VM")
		}
		log.V(5).Info(fmt.Sprintf("created the Windows VM: %s",
			createdVM.GetCredentials().GetInstanceId()))

		// Make the Windows VM a Windows worker node.
		nc := nodeconfig.NewNodeConfig(r.k8sclientset, createdVM)
		if err := nc.Configure(); err != nil {
			// TODO: Unwrap to extract correct error
			errs = append(errs, errors.Wrap(err, "configuring Windows VM failed"))
			log.Error(err, "configuring Windows VM failed", err)
		}

		// update the windowsVMs slice
		if _, ok := r.windowsVMs[createdVM]; !ok {
			r.windowsVMs[createdVM] = true
		}
	}

	// If any of the Windows VM fails to get created consider this as a failure and return false
	if len(errs) > 0 {
		return false
	}
	return true
}
