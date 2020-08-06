package windowsmachine

import (
	"context"
	"strings"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/signer"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachine/nodeconfig"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	// ControllerName is the name of the WindowsMachine controller
	ControllerName = "windowsmachine-controller"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new WindowsMachine Controller and adds it to the Manager. The Manager will set fields on the Controller
// and start it when the Manager is Started.
func Add(mgr manager.Manager, clusterServiceCIDR string) error {
	reconciler, err := newReconciler(mgr, clusterServiceCIDR)
	if err != nil {
		return errors.Wrapf(err, "could not create %s reconciler", ControllerName)
	}
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, clusterServiceCIDR string) (reconcile.Reconciler, error) {
	// The default client serves read requests from the cache which
	// could be stale and result in a get call to return an older version
	// of the object. Hence we are using a non-default-client referenced
	// by operator-sdk.
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	client, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, errors.Wrap(err, "error creating kubernetes clientset")
	}

	signer, err := signer.Create()
	if err != nil {
		return nil, errors.Wrapf(err, "error creating signer using private key: %v", wkl.PrivateKeyPath)
	}

	return &ReconcileWindowsMachine{client: client,
			scheme:             mgr.GetScheme(),
			k8sclientset:       clientset,
			clusterServiceCIDR: clusterServiceCIDR,
			signer:             signer,
			recorder:           mgr.GetEventRecorderFor(ControllerName),
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
	// Watch for the Machine objects with label defined by windowsOSLabel
	windowsOSLabel := "machine.openshift.io/os-id"
	predicateFilter := predicate.Funcs{
		// ignore create event for all Machines as WMCO should for Machine getting provisioned
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			labels := e.MetaNew.GetLabels()
			if value, ok := labels[windowsOSLabel]; ok {
				if value == "Windows" {
					return true
				}
			}
			return false
		},
		// ignore delete event for all Machines as WMCO does not react to node getting deleted
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
	}

	err = c.Watch(&source.Kind{Type: &mapi.Machine{
		ObjectMeta: meta.ObjectMeta{Namespace: "openshift-machine-api"},
	}}, &handler.EnqueueRequestForObject{}, predicateFilter)
	if err != nil {
		return errors.Wrap(err, "could not create watch on Machine objects")
	}

	return nil
}

// blank assignment to verify that ReconcileWindowsMachine implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileWindowsMachine{}

// ReconcileWindowsMachine reconciles a Windows Machine object
type ReconcileWindowsMachine struct {
	// client is the client initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	// scheme is the scheme used to resolve runtime.Objects to resources
	scheme *runtime.Scheme
	// k8sclientset holds the kube client that we can re-use for all kube objects other than custom resources.
	k8sclientset *kubernetes.Clientset
	// clusterServiceCIDR holds the cluster network service CIDR
	clusterServiceCIDR string
	// signer is a signer created from the user's private key
	signer ssh.Signer
	// recorder to generate events
	recorder record.EventRecorder
}

// Reconcile reads that state of the cluster for a Windows Machine object and makes changes based on the state read
// and what is in the Machine.Spec
// Note: The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileWindowsMachine) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	log.Info("reconciling", "namespace", request.Namespace, "name", request.Name)
	// validate userData secret
	if err := r.validateUserData(); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "error validating userData secret")
	}
	// Fetch the Machine instance
	machine := &mapi.Machine{}
	if err := r.client.Get(context.TODO(), request.NamespacedName, machine); err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	// provisionedPhase is the status of the machine when it is in the `Provisioned` state
	provisionedPhase := "Provisioned"
	if machine.Status.Phase == nil || *machine.Status.Phase != provisionedPhase {
		// Phase can be nil and should be ignored by WMCO
		// If the Machine is not in provisioned state, WMCO shouldn't care about it
		return reconcile.Result{}, nil
	}

	// Get the IP address associated with the Windows machine.
	if len(machine.Status.Addresses) == 0 {
		return reconcile.Result{}, nil
	}
	ipAddress := ""
	for _, address := range machine.Status.Addresses {
		if address.Type == core.NodeInternalIP {
			ipAddress = address.Address
		}
	}
	if len(ipAddress) == 0 {
		return reconcile.Result{}, nil
	}

	// Get the instance ID associated with the Windows machine.
	providerID := *machine.Spec.ProviderID
	if len(providerID) == 0 {
		return reconcile.Result{}, nil
	}
	// Ex: aws:///us-east-1e/i-078285fdadccb2eaa. We always want the last entry which is the instanceID
	providerTokens := strings.Split(providerID, "/")
	instanceID := providerTokens[len(providerTokens)-1]
	if len(instanceID) == 0 {
		return reconcile.Result{}, nil
	}

	// Make the Machine a Windows Worker node
	if err := r.addWorkerNode(ipAddress, instanceID); err != nil {
		r.recorder.Eventf(machine, core.EventTypeWarning, "WMCO SetupFailure",
			"Machine %s failed to be configured", machine.Name)
		return reconcile.Result{}, err
	}
	r.recorder.Eventf(machine, core.EventTypeNormal, "WMCO Setup",
		"Machine %s Configured Successfully", machine.Name)

	return reconcile.Result{}, nil
}

// addWorkerNode configures the given Windows VM, adding it as a node object to the cluster
func (r *ReconcileWindowsMachine) addWorkerNode(ipAddress, instanceID string) error {
	log.V(1).Info("configuring the Windows VM", "ID", instanceID)
	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, ipAddress, instanceID, r.clusterServiceCIDR, r.signer)
	if err != nil {
		return errors.Wrapf(err, "failed to configure Windows VM %s", instanceID)
	}
	if err := nc.Configure(); err != nil {
		// TODO: Unwrap to extract correct error
		return errors.Wrapf(err, "failed to configure Windows VM %s", instanceID)
	}

	log.Info("Windows VM has joined the cluster as a worker node", "ID", nc.ID())
	return nil
}

// validateUserData validates userData secret. It returns error if the secret doesn`t
// contain expected public key bytes.
func (r *ReconcileWindowsMachine) validateUserData() error {
	userDataSecret := &core.Secret{}
	err := r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: "windows-user-data", Namespace: "openshift-machine-api"}, userDataSecret)

	if err != nil {
		return errors.Errorf("could not find Windows userData secret in required namespace: %v", err)
	}

	secretData := string(userDataSecret.Data["userData"][:])
	desiredUserDataSecret, err := secrets.GenerateUserData()
	if err != nil {
		return err
	}
	if string(desiredUserDataSecret.Data["userData"][:]) != secretData {
		return errors.Errorf("invalid content for userData secret")
	}
	return nil
}
