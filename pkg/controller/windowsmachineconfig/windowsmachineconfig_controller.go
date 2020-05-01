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
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/tracker"
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
	// ControllerName is the name of the WMC controller
	ControllerName = "windowsmachineconfig-controller"
)

var log = logf.Log.WithName("controller_wmc")

// Add creates a new WindowsMachineConfig Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	reconciler, err := newReconciler(mgr)
	if err != nil {
		return errors.Wrapf(err, "could not create %s reconciler", ControllerName)
	}
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	// TODO: This should be moved out to validation for reconciler struct.
	// 		Jira story: https://issues.redhat.com/browse/WINC-277
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	windowsVMs := make(map[types.WindowsVM]bool)
	vmTracker, err := tracker.NewTracker(clientset, windowsVMs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to instantiate tracker")
	}

	return &ReconcileWindowsMachineConfig{client: mgr.GetClient(),
			scheme:       mgr.GetScheme(),
			k8sclientset: clientset,
			tracker:      vmTracker,
			windowsVMs:   windowsVMs,
		},
		nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(ControllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return errors.Wrapf(err, "could not create %s", ControllerName)
	}
	// TODO: Add a predicate here. As of now, we get event notifications for all the WindowsMachineConfig objects, we
	//		want the predicate to filter the WMC object called `instance`
	//		Jira Story: https://issues.redhat.com/browse/WINC-282
	// Watch for changes to primary resource WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &wmcapi.WindowsMachineConfig{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return errors.Wrap(err, "could not create watch on WindowsMachineConfig objects")
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
	// tracker is used to track all the Windows nodes created via WMCO
	tracker *tracker.Tracker
}

// getCloudProvider gathers the cloud provider information and sets the cloudProvider struct field
func (r *ReconcileWindowsMachineConfig) getCloudProvider(instance *wmcapi.WindowsMachineConfig) error {
	var err error
	if instance == nil {
		return fmt.Errorf("cannot get cloud provider if instance is not set")
	}
	// Get cloud provider specific info.
	// TODO: This should be moved to validation section.
	//              Jira story: https://issues.redhat.com/browse/WINC-277
	if instance.Spec.AWS == nil {
		return fmt.Errorf("aws cloud provider is nil, cannot proceed further")
	}

	// TODO: We assume the cloud provider secret has been mounted to "/etc/cloud/credentials` and private key is
	//              present at "/etc/private-key.pem". We should have a validation method which checks for the existence
	//              of these paths.
	//              Jira story: https://issues.redhat.com/browse/WINC-262
	// TODO: Add validation for the fields in the WindowsMachineConfig CRD.
	//              Jira story: https://issues.redhat.com/browse/WINC-279
	r.cloudProvider, err = cloudprovider.CloudProviderFactory(os.Getenv("KUBECONFIG"),
		// We assume the credential path is `/etc/aws/credentials` mounted as a secret.
		wkl.CloudCredentialsPath,
		instance.Spec.AWS.CredentialAccountID,
		"/tmp", "", instance.Spec.InstanceType,
		instance.Spec.AWS.SSHKeyPair, wkl.PrivateKeyPath)

	if err != nil {
		return errors.Wrap(err, "error instantiating cloud provider")
	}

	return nil
}

// Reconcile reads that state of the cluster for a WindowsMachineConfig object and makes changes based on the state read
// and what is in the WindowsMachineConfig.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileWindowsMachineConfig) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("reconciling", "namespace", request.Namespace, "name", request.Name)

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

	if err := r.getCloudProvider(instance); err != nil {
		log.Error(err, "could not get cloud provider")
		// Not going to requeue as an issue here indicates a problem with the provided credentials
		return reconcile.Result{}, nil
	}

	// Get the current number of Windows VMs created by WMCO.
	// TODO: Get all the running Windows nodes in the cluster
	//		jira story: https://issues.redhat.com/browse/WINC-280
	windowsNodes, err := r.k8sclientset.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
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
	log.Info("replicas", "current", current, "desired", desired)
	var vmCount bool
	if desired < current {
		vmCount = r.removeWorkerNodes(current - desired)
	} else if desired > current {
		vmCount = r.addWorkerNodes(desired - current)
	} else if desired == current {
		return true
	}
	r.tracker.WindowsVMs(r.windowsVMs)
	log.V(1).Info("starting tracker reconciliation")
	err := r.tracker.Reconcile()
	if err != nil {
		log.Error(err, "tracker reconciliation failed")
	}
	log.V(1).Info("completed tracker reconciliation")
	return vmCount
}

// chooseRandomNode chooses one of the windows nodes randomly. The randomization is coming from golang maps since you
// cannot assume the maps to be ordered.
func chooseRandomNode(windowsVMs map[types.WindowsVM]bool) types.WindowsVM {
	for windowsVM := range windowsVMs {
		return windowsVM
	}
	return nil
}

// removeWorkerNode terminates the underlying VM and removes the given vm from the list of VMs
func (r *ReconcileWindowsMachineConfig) removeWorkerNode(vm types.WindowsVM) error {
	// VM is missing credentials, this can occur if there was a failure initially creating it. We can consider the
	// actual VM terminated as there is nothing we can do with it.
	if vm.GetCredentials() == nil || len(vm.GetCredentials().GetInstanceId()) == 0 {
		delete(r.windowsVMs, vm)
		return nil
	}

	// Terminate the instance via its instance id
	id := vm.GetCredentials().GetInstanceId()
	log.V(1).Info("destroying the Windows VM", "ID", id)

	// Delete the Windows VM from cloud provider
	if err := r.cloudProvider.DestroyWindowsVM(id); err != nil {
		return errors.Wrapf(err, "error destroying VM with ID %s", id)
	}

	// Remove VM from our list of tracked VMs
	delete(r.windowsVMs, vm)
	log.Info("Windows worker has been removed from the cluster", "ID", id)

	return nil
}

// removeWorkerNodes removes the required number of Windows VMs from the cluster and returns a bool indicating the
// success. This method will return false if any of the VMs fail to be removed.
// TODO: This method should return a slice of errors that we collected.
//		Jira story: https://issues.redhat.com/browse/WINC-266
func (r *ReconcileWindowsMachineConfig) removeWorkerNodes(count int) bool {
	var errs []error
	// From the list of Windows VMs choose randomly count number of VMs.
	for i := 0; i < count; i++ {
		// Choose of the Windows worker nodes randomly
		vm := chooseRandomNode(r.windowsVMs)
		if vm == nil {
			errs = append(errs, fmt.Errorf("expected VM and got a nil value"))
			continue
		}
		if err := r.removeWorkerNode(vm); err != nil {
			errs = append(errs, err)
		}
	}

	// If any of the Windows VM fails to get removed consider this as a failure and return false
	if len(errs) > 0 {
		return false
	}
	return true
}

// addWorkerNode creates a new Windows VM and configures it, adding it as a node object to the cluster
func (r *ReconcileWindowsMachineConfig) addWorkerNode() (types.WindowsVM, error) {
	// Create Windows VM in the cloud provider
	log.V(1).Info("creating a Windows VM")
	vm, err := r.cloudProvider.CreateWindowsVM()
	if err != nil {
		return nil, errors.Wrap(err, "error creating windows VM")
	}

	log.V(1).Info("configuring the Windows VM", "ID", vm.GetCredentials().GetInstanceId())
	nc := nodeconfig.NewNodeConfig(r.k8sclientset, vm)
	if err := nc.Configure(); err != nil {
		// TODO: Unwrap to extract correct error
		if cleanupErr := r.removeWorkerNode(vm); cleanupErr != nil {
			log.Error(cleanupErr, "failed to cleanup VM", "VM", vm.GetCredentials().GetInstanceId())
		}
		return nil, errors.Wrap(err, "failed to configure Windows VM")
	}

	log.Info("Windows VM has joined the cluster as a worker node", "ID", nc.GetCredentials().GetInstanceId())
	return vm, nil
}

// addWorkerNodes creates the required number of Windows VMs and configures them to make
// them a worker node
// TODO: This method should return a slice of errors that we collected.
//		Jira story: https://issues.redhat.com/browse/WINC-266
func (r *ReconcileWindowsMachineConfig) addWorkerNodes(count int) bool {
	var errs []error
	for i := 0; i < count; i++ {
		// Create and configure a new Windows VM
		vm, err := r.addWorkerNode()
		if err != nil {
			log.Error(err, "error adding a Windows worker node")
			errs = append(errs, errors.Wrap(err, "error adding Windows worker node"))
			continue
		}

		// update the windowsVMs map with the new VM
		if _, ok := r.windowsVMs[vm]; !ok {
			r.windowsVMs[vm] = true
		}
	}

	// If any of the Windows VM fails to get created consider this as a failure and return false
	if len(errs) > 0 {
		return false
	}
	return true
}
