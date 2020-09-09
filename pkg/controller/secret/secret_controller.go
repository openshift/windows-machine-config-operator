package secret

import (
	"context"

	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeTypes "k8s.io/apimachinery/pkg/types"
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

	"github.com/openshift/windows-machine-config-operator/pkg/clusternetwork"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
)

const (
	ControllerName    = "secret_controller"
	userDataSecret    = "windows-user-data"
	userDataNamespace = "openshift-machine-api"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new Secret Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, _ clusternetwork.ClusterNetworkConfig, watchNamespace string) error {
	reconciler, err := newReconciler(mgr)
	if err != nil {
		return errors.Wrapf(err, "could not create %s reconciler", ControllerName)
	}
	return add(mgr, reconciler, watchNamespace)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	client, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return nil, err
	}

	reconciler := &ReconcileSecret{client: client, scheme: mgr.GetScheme()}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler, watchNamespace string) error {
	// Create a new controller
	c, err := controller.New(ControllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return errors.Wrapf(err, "could not create the controller: %v", ControllerName)
	}

	// Check that the private key exists, if it doesn't, log a warning
	_, err = secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: watchNamespace, Name: secrets.PrivateKeySecret}, mgr.GetClient())
	if err != nil {
		log.Error(err, "Unable to retrieve private key, please ensure it is created")
	}

	// Watch for changes to the userData secret and enqueue the cloud-private-key if changed
	// Name and namespace cannot be used to watch for specific secrets, so we filter out all the other secrets we
	// dont care about.
	// https://github.com/kubernetes-sigs/controller-runtime/issues/244
	err = c.Watch(&source.Kind{Type: &core.Secret{}},
		&handler.EnqueueRequestsFromMapFunc{ToRequests: newUserDataMapper(watchNamespace)},
		predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return isPrivateKeySecret(e.Meta, watchNamespace) || isUserDataSecret(e.Meta)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return isPrivateKeySecret(e.Meta, watchNamespace) || isUserDataSecret(e.Meta)
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				// get update event only when secret data is changed
				if isPrivateKeySecret(e.MetaNew, watchNamespace) {
					if string(e.ObjectOld.(*core.Secret).Data[secrets.PrivateKeySecretKey]) !=
						string(e.ObjectNew.(*core.Secret).Data[secrets.PrivateKeySecretKey]) {
						return true
					}
				} else if isUserDataSecret(e.MetaNew) {
					if string(e.ObjectOld.(*core.Secret).Data["userData"][:]) != string(e.ObjectNew.(*core.Secret).Data["userData"][:]) {
						return true
					}
				}
				return false
			},
		})
	if err != nil {
		return err
	}
	return nil
}

// isUserDataSecret returns true if the object meta indicates that the object is the userData Secret
func isUserDataSecret(meta meta.Object) bool {
	return meta.GetName() == userDataSecret && meta.GetNamespace() == userDataNamespace
}

// isPrivateKeySecret returns true if the object meta indicates that the object is the private key secret
func isPrivateKeySecret(meta meta.Object, keyNamespace string) bool {
	return meta.GetName() == secrets.PrivateKeySecret && meta.GetNamespace() == keyNamespace
}

// blank assignment to verify that ReconcileSecret implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileSecret{}

// ReconcileSecret reconciles a Secret object
type ReconcileSecret struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a Secret object and makes changes based on the state read
// and what is in the Secret.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileSecret) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)

	privateKey, err := secrets.GetPrivateKey(request.NamespacedName, r.client)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "unable to get secret %s", request.NamespacedName)
	}
	// Generate expected userData based on the existing private key
	validUserData, err := secrets.GenerateUserData(privateKey)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "error generating userData secret")
	}

	userData := &core.Secret{}
	// Fetch UserData instance
	err = r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: userDataSecret, Namespace: userDataNamespace}, userData)
	if err != nil && k8sapierrors.IsNotFound(err) {
		// Secret is deleted
		reqLogger.Info("UserData secret not found, creating the secret.")
		err = r.client.Create(context.TODO(), validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Secret created successfully - don't requeue
		return reconcile.Result{}, nil
	} else if err != nil {
		reqLogger.Error(err, "error retrieving the userData secret")
		return reconcile.Result{}, err
	} else if string(userData.Data["userData"][:]) == string(validUserData.Data["userData"][:]) {
		// valid userData secret already exists
		return reconcile.Result{}, nil
	} else {
		// secret is updated
		reqLogger.Info("updating userdata secret")
		err = r.client.Update(context.TODO(), validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Secret updated successfully
		return reconcile.Result{}, nil
	}
}

// userDataMapper is a simple implementation of the Mapper interface allowing for the mapping from the userData secret
// to the private key secret
type userDataMapper struct {
	// watchNamespace is the namespace the operator is watching as defined by the CSV
	watchNamespace string
}

// newUserDataMapper returns a pointer to a new userDataMapper
func newUserDataMapper(namespace string) *userDataMapper {
	return &userDataMapper{watchNamespace: namespace}
}

func (m *userDataMapper) Map(_ handler.MapObject) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: kubeTypes.NamespacedName{Namespace: m.watchNamespace, Name: secrets.PrivateKeySecret}},
	}
}
