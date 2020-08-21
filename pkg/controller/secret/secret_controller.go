package secret

import (
	"context"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/secrets"
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
)

const (
	ControllerName    = "secret_controller"
	userDataSecret    = "windows-user-data"
	userDataNamespace = "openshift-machine-api"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new Secret Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, _ string) error {
	reconciler, err := newReconciler(mgr)
	if err != nil {
		return errors.Wrapf(err, "could not create %s reconciler", ControllerName)
	}
	return add(mgr, reconciler)
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

	// Create userData secret
	if err := reconciler.createUserData(); err != nil {
		return nil, errors.Wrapf(err, "error creating primary resource : %v", userDataSecret)
	}
	return reconciler, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(ControllerName, mgr, controller.Options{Reconciler: r})
	if err != nil {
		return errors.Wrapf(err, "could not create the controller: %v", ControllerName)
	}
	predicateFilter := predicate.Funcs{
		// get update event only when secret data is changed
		UpdateFunc: func(e event.UpdateEvent) bool {
			if string(e.ObjectOld.(*core.Secret).Data["userData"][:]) != string(e.ObjectNew.(*core.Secret).Data["userData"][:]) {
				return true
			}
			return false
		},
	}

	// Watch for changes to primary resource UserDataSecret
	err = c.Watch(&source.Kind{Type: &core.Secret{ObjectMeta: meta.ObjectMeta{Namespace: userDataNamespace,
		Name: userDataSecret}}}, &handler.EnqueueRequestForObject{}, predicateFilter)
	if err != nil {
		return err
	}
	return nil
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
	reqLogger.Info("Reconciling " + userDataSecret)

	validUserData, err := secrets.GenerateUserData()
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "error generating userData secret")
	}

	userData := &core.Secret{}
	// Fetch UserData instance
	err = r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: userDataSecret, Namespace: userDataNamespace}, userData)
	if err != nil && k8sapierrors.IsNotFound(err) {
		// Secret is deleted
		reqLogger.Info("UserData secret not found, recreating the secret.")
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
		reqLogger.Info("Restoring the userData secret")
		err = r.client.Update(context.TODO(), validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Secret updated successfully
		return reconcile.Result{}, nil
	}
}

// createUserData creates userData secret if it is not present in the required namespace.
func (r *ReconcileSecret) createUserData() error {
	// generate valid userData secret
	validUserDataSecret, err := secrets.GenerateUserData()
	if err != nil {
		return errors.Wrapf(err, "error generating valid userData secret")
	}

	err = r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: userDataSecret,
		Namespace: userDataNamespace}, &core.Secret{})
	if err != nil && k8sapierrors.IsNotFound(err) {
		// Create valid userData secret
		if err = r.client.Create(context.TODO(), validUserDataSecret); err != nil {
			return errors.Wrapf(err, "error creating userData secret")
		}
	} else if err != nil {
		return errors.Wrapf(err, "error retrieving userData secret")
	}
	return nil
}
