package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

//+kubebuilder:rbac:groups="",resources=nodes,verbs=*
//+kubebuilder:rbac:groups="",resources=secrets,verbs=create;get;list;watch;update

const (
	userDataSecret    = "windows-user-data"
	userDataNamespace = "openshift-machine-api"
	// SecretController is the name of this controller in logs and other outputs.
	SecretController = "secret"
)

// NewSecretReconciler returns a pointer to a SecretReconciler
func NewSecretReconciler(mgr manager.Manager, watchNamespace string) *SecretReconciler {
	reconciler := &SecretReconciler{
		client:         mgr.GetClient(),
		scheme:         mgr.GetScheme(),
		log:            ctrl.Log.WithName("controller").WithName(SecretController),
		watchNamespace: watchNamespace,
		recorder:       mgr.GetEventRecorderFor(SecretController)}
	return reconciler
}

// SetupWithManager sets up a new Secret controller
func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Check that the private key exists, if it doesn't, log a warning
	_, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, mgr.GetClient())
	if err != nil {
		r.log.Error(err, "Unable to retrieve private key, please ensure it is created")
	}

	privateKeyPredicate := builder.WithPredicates(predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isPrivateKeySecret(e.Object, r.watchNamespace)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isPrivateKeySecret(e.Object, r.watchNamespace)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// get update event only when secret data is changed
			if isPrivateKeySecret(e.ObjectNew, r.watchNamespace) {
				if string(e.ObjectOld.(*core.Secret).Data[secrets.PrivateKeySecretKey]) !=
					string(e.ObjectNew.(*core.Secret).Data[secrets.PrivateKeySecretKey]) {
					return true
				}
			}
			return false
		},
	})
	mappingPredicate := builder.WithPredicates(predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isUserDataSecret(e.Object)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isUserDataSecret(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// get update event only when secret data is changed
			if isUserDataSecret(e.ObjectNew) {
				if string(e.ObjectOld.(*core.Secret).Data["userData"][:]) !=
					string(e.ObjectNew.(*core.Secret).Data["userData"][:]) {
					return true
				}
			}
			return false
		},
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&core.Secret{}, privateKeyPredicate).
		Watches(&source.Kind{Type: &core.Secret{}}, handler.EnqueueRequestsFromMapFunc(r.mapToPrivateKeySecret),
			mappingPredicate).
		Complete(r)
}

// isUserDataSecret returns true if the provided object is the userData Secret
func isUserDataSecret(obj client.Object) bool {
	return obj.GetName() == userDataSecret && obj.GetNamespace() == userDataNamespace
}

// isPrivateKeySecret returns true if the provided object is the private key secret
func isPrivateKeySecret(obj client.Object, keyNamespace string) bool {
	return obj.GetName() == secrets.PrivateKeySecret && obj.GetNamespace() == keyNamespace
}

// SecretReconciler is used to create a controller which manages Secret objects
type SecretReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
	log    logr.Logger
	// watchNamespace is the namespace the operator is watching as defined by the operator CSV
	watchNamespace string
	// recorder to generate events
	recorder record.EventRecorder
}

// Reconcile reads that state of the cluster for a Secret object and makes changes based on the state read
// and what is in the Secret.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *SecretReconciler) Reconcile(ctx context.Context,
	request ctrl.Request) (result reconcile.Result, reconcileErr error) {
	log := r.log.WithValues(SecretController, request.NamespacedName)

	// Prevent WMCO upgrades while secret-based resources are being processed
	if err := condition.MarkAsBusy(r.client, r.watchNamespace, r.recorder, SecretController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		reconcileErr = markAsFreeOnSuccess(r.client, r.watchNamespace, r.recorder, SecretController,
			result.Requeue, reconcileErr)
	}()

	keySigner, err := signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Private key secret was not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, errors.Wrapf(err, "unable to get secret %s", request.NamespacedName)
	}
	// Generate expected userData based on the existing private key
	validUserData, err := secrets.GenerateUserData(keySigner.PublicKey())
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "error generating %s secret", userDataSecret)
	}

	userData := &core.Secret{}
	// Fetch UserData instance
	err = r.client.Get(ctx, kubeTypes.NamespacedName{Name: userDataSecret, Namespace: userDataNamespace}, userData)
	if err != nil && k8sapierrors.IsNotFound(err) {
		// Secret is deleted
		log.Info("secret not found, creating the secret", "name", userDataSecret)
		err = r.client.Create(ctx, validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Secret created successfully - don't requeue
		return reconcile.Result{}, nil
	} else if err != nil {
		log.Error(err, "error retrieving the secret", "name", userDataSecret)
		return reconcile.Result{}, err
	} else if string(userData.Data["userData"][:]) == string(validUserData.Data["userData"][:]) {
		// valid userData secret already exists
		return reconcile.Result{}, nil
	} else {
		// userdata secret data does not match what is expected
		nodes := &core.NodeList{}
		err = r.client.List(ctx, nodes, client.MatchingLabels{core.LabelOSStable: "windows"})
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "error getting node list")
		}

		// Modify annotations on nodes configured with the previous private key, if it has changed
		expectedPubKeyAnno := nodeconfig.CreatePubKeyHashAnnotation(keySigner.PublicKey())
		privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
			Name: secrets.PrivateKeySecret}, r.client)
		if err != nil {
			return reconcile.Result{}, err
		}
		for _, node := range nodes.Items {
			// Since the public key hash and username annotations are both dependant on the private key secret as well
			// as applied and updated at the same time, checking one is enough to see if the private key is up to date
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] == expectedPubKeyAnno {
				continue
			}
			annotationsToApply := make(map[string]string)
			if _, present := node.GetLabels()[BYOHLabel]; present {
				// For BYOH nodes, update the username annotation and public key hash annotation using new private key
				expectedUsernameAnnotation, err := r.getEncryptedUsername(ctx, node, privateKeyBytes)
				if err != nil {
					return reconcile.Result{}, errors.Wrapf(err, "unable to retrieve expected username annotation")
				}

				annotationsToApply = map[string]string{
					UsernameAnnotation:              expectedUsernameAnnotation,
					nodeconfig.PubKeyHashAnnotation: expectedPubKeyAnno,
				}
			} else {
				// For Nodes associated with Machines, clear the public key annotation, as the clearing of the
				// annotation is used solely to kick off the deletion and recreation of Machines.
				annotationsToApply = map[string]string{nodeconfig.PubKeyHashAnnotation: ""}
			}

			if err := metadata.ApplyAnnotations(r.client, ctx, node, annotationsToApply); err != nil {
				return reconcile.Result{}, errors.Wrapf(err, "error updating annotations on node %s", node.GetName())
			}
			log.V(1).Info("patched node object", "node", node.GetName(), "patch", annotationsToApply)
		}

		// Set userdata to expected value
		log.Info("updating secret", "name", userDataSecret)
		err = r.client.Update(ctx, validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}

		// Secret updated successfully
		return reconcile.Result{}, nil
	}
}

// getEncryptedUsername retrieves the username associated with a given node and ecrypts it using the given key
func (r *SecretReconciler) getEncryptedUsername(ctx context.Context, node core.Node, key []byte) (string, error) {
	// The instance ConfigMap is the source of truth linking BYOH nodes to their underlying instances
	instancesConfigMap := &core.ConfigMap{}
	if err := r.client.Get(ctx, kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: wiparser.InstanceConfigMap}, instancesConfigMap); err != nil {
		return "", errors.Wrap(err, "unable to get instance configmap")
	}
	instanceUsername, err := wiparser.GetNodeUsername(instancesConfigMap.Data, &node)
	if err != nil {
		return "", err
	}
	encryptedUsername, err := crypto.EncryptToJSONString(instanceUsername, key)
	if err != nil {
		return "", errors.Wrapf(err, "error encrypting node %s username", node.GetName())
	}
	return encryptedUsername, nil
}

// RemoveInvalidAnnotationsFromLinuxNodes makes a best effort to remove annotations applied by previous versions of WMCO.
func (r *SecretReconciler) RemoveInvalidAnnotationsFromLinuxNodes(config *rest.Config) error {
	// create a new clientset as this function will be called before the manager's client is started
	kc, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.Wrap(err, "error creating kubernetes clientset")
	}

	nodes, err := kc.CoreV1().Nodes().List(context.TODO(), meta.ListOptions{LabelSelector: core.LabelOSStable + "=linux"})
	if err != nil {
		return errors.Wrap(err, "error getting Linux node list")
	}
	// The public key hash was accidentally added to Linux nodes in WMCO 2.0 and must be removed.
	// The `/` in the annotation key needs to be escaped in order to not be considered a "directory" in the path.
	patchData, err := metadata.GenerateRemovePatch([]string{}, []string{nodeconfig.PubKeyHashAnnotation})
	if err != nil {
		return errors.Wrapf(err, "error creating public key annotation add request")
	}
	for _, node := range nodes.Items {
		if _, present := node.Annotations[nodeconfig.PubKeyHashAnnotation]; present == true {
			_, err = kc.CoreV1().Nodes().Patch(context.TODO(), node.GetName(), kubeTypes.JSONPatchType,
				patchData, meta.PatchOptions{})
			if err != nil {
				return errors.Wrapf(err, "error removing public key annotation from node %s", node.GetName())
			}
		}
	}
	return nil
}

// mapToPrivateKeySecret is a mapping function that will always return a request for the cloud private key secret
func (r *SecretReconciler) mapToPrivateKeySecret(_ client.Object) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: secrets.PrivateKeySecret}},
	}
}
