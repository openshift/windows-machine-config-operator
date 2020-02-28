package windowsmachineconfig

import (
	"context"
	"fmt"
	"os"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	wmcv1alpha1 "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
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
	return &ReconcileWindowsMachineConfig{client: mgr.GetClient(), scheme: mgr.GetScheme()}
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
	windowsVM     []types.WindowsVM
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

	// Get the current count of required number of Windows VMs
	currentCountOfWindowsVMs := 1 // As of now hardcoded to 1. We need to get the number of Windows VM node objects
	if instance.Spec.Replicas != currentCountOfWindowsVMs {
		if err := r.reconcileWindowsNodes(instance.Spec.Replicas, currentCountOfWindowsVMs); err != nil {
			return reconcile.Result{}, err
		}
	}

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

// deleteWindowsVMs returns the instance IDs of the successfully created Windows VMs
func (r *ReconcileWindowsMachineConfig) deleteWindowsVMs(count int) {
	// From the list of Windows VMs choose randomly count number of VMs. As of now sequential
	for i := 0; i < count; i++ {
		if r.windowsVM[i].GetCredentials() == nil {
			continue
		}
		if len(r.windowsVM[i].GetCredentials().GetInstanceId()) == 0 {
			continue
		}
		instance := r.windowsVM[i].GetCredentials().GetInstanceId()
		// Delete the Windows VM here. Make a call to the Windows
		log.Info("deleting the Windows VM %v", instance)
		if err := r.cloudProvider.DestroyWindowsVM(instance); err != nil {
			log.Error(err, "error while deleting windows VM %s", instance)
		}
	}

}

func (r *ReconcileWindowsMachineConfig) createWindowsVMs(count int) {
	for i := 0; i < count; i++ {
		// Create Windows VM
		windowsVM, err := r.cloudProvider.CreateWindowsVM()
		if err != nil {
			log.Error(err, "error while creating windows VM", err)
		}
		r.windowsVM = append(r.windowsVM, windowsVM)
	}
}
