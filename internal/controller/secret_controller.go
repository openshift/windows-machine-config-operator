package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	oconfig "github.com/openshift/api/config/v1"
	"golang.org/x/crypto/ssh"
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

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/wiparser"
)

//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;patch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=create;get;list;watch;update

const (
	// SecretController is the name of this controller in logs and other outputs.
	SecretController = "secret"
)

// NewSecretReconciler returns a pointer to a SecretReconciler
func NewSecretReconciler(mgr manager.Manager, platform oconfig.PlatformType, watchNamespace string) *SecretReconciler {
	reconciler := &SecretReconciler{
		client:         mgr.GetClient(),
		scheme:         mgr.GetScheme(),
		log:            ctrl.Log.WithName("controller").WithName(SecretController),
		watchNamespace: watchNamespace,
		recorder:       mgr.GetEventRecorderFor(SecretController),
		platform:       platform,
	}
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
		Watches(&core.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapToPrivateKeySecret),
			mappingPredicate).
		Complete(r)
}

// isUserDataSecret returns true if the provided object is the userData Secret
func isUserDataSecret(obj client.Object) bool {
	return obj.GetName() == secrets.UserDataSecret && obj.GetNamespace() == cluster.MachineAPINamespace
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
	// platform indicates the platform on which the cluster is running
	platform oconfig.PlatformType
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
		return reconcile.Result{}, fmt.Errorf("unable to get secret %s: %w", request.NamespacedName, err)
	}
	// Generate expected userData based on the existing private key
	validUserData, err := secrets.GenerateUserData(r.platform, keySigner.PublicKey())
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("error generating %s secret: %w", secrets.UserDataSecret, err)
	}

	userData := &core.Secret{}
	// Fetch UserData instance
	err = r.client.Get(ctx,
		kubeTypes.NamespacedName{Name: secrets.UserDataSecret, Namespace: cluster.MachineAPINamespace}, userData)
	if err != nil && k8sapierrors.IsNotFound(err) {
		// Secret is deleted
		log.Info("secret not found, creating the secret", "name", secrets.UserDataSecret)
		err = r.client.Create(ctx, validUserData)
		if err != nil {
			return reconcile.Result{}, err
		}
		// Secret created successfully - don't requeue
		return reconcile.Result{}, nil
	} else if err != nil {
		log.Error(err, "error retrieving the secret", "name", secrets.UserDataSecret)
		return reconcile.Result{}, err
	} else if string(userData.Data["userData"][:]) == string(validUserData.Data["userData"][:]) {
		// valid userData secret already exists
		return reconcile.Result{}, nil
	} else {
		// userdata secret data does not match what is expected
		return reconcile.Result{}, r.updateUserData(ctx, keySigner, validUserData)
	}
}

// updateUserData updates the userdata secret to the expected state
func (r *SecretReconciler) updateUserData(ctx context.Context, keySigner ssh.Signer, expected *core.Secret) error {
	nodes := &core.NodeList{}
	err := r.client.List(ctx, nodes, client.MatchingLabels{core.LabelOSStable: "windows"})
	if err != nil {
		return fmt.Errorf("error getting node list: %w", err)
	}

	// Modify annotations on nodes configured with the previous private key, if it has changed
	expectedPubKeyAnno := nodeconfig.CreatePubKeyHashAnnotation(keySigner.PublicKey())
	privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return err
	}
	for _, node := range nodes.Items {
		annotationsToApply := make(map[string]string)
		if _, present := node.GetLabels()[BYOHLabel]; present {
			// Since the public key hash and username annotations are both dependent on the private key secret as well
			// as applied and updated at the same time, checking one is enough to see if the private key is up to date
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] == expectedPubKeyAnno {
				continue
			}
			// For BYOH nodes, update the username annotation and public key hash annotation using new private key
			expectedUsernameAnnotation, err := r.getEncryptedUsername(ctx, node, privateKeyBytes)
			if err != nil {
				return fmt.Errorf("unable to retrieve expected username annotation: %w", err)
			}

			annotationsToApply = map[string]string{
				UsernameAnnotation:              expectedUsernameAnnotation,
				nodeconfig.PubKeyHashAnnotation: expectedPubKeyAnno,
			}
		} else {
			// For Nodes associated with Machines, clear the public key annotation, as the clearing of the
			// annotation is used solely to kick off the deletion and recreation of Machines, causing them to be
			// provisioned with the new userdata
			annotationsToApply = map[string]string{nodeconfig.PubKeyHashAnnotation: ""}
		}

		if err := metadata.ApplyLabelsAndAnnotations(ctx, r.client, node, nil, annotationsToApply); err != nil {
			return fmt.Errorf("error updating annotations on node %s: %w", node.GetName(), err)
		}
		r.log.V(1).Info("patched node object", "node", node.GetName(), "patch", annotationsToApply)
	}

	// Set userdata to expected value
	r.log.Info("updating secret", "name", secrets.UserDataSecret)
	err = r.client.Update(ctx, expected)
	if err != nil {
		return fmt.Errorf("error updating secret: %w", err)
	}
	return nil
}

// getEncryptedUsername retrieves the username associated with a given node and ecrypts it using the given key
func (r *SecretReconciler) getEncryptedUsername(ctx context.Context, node core.Node, key []byte) (string, error) {
	// The instance ConfigMap is the source of truth linking BYOH nodes to their underlying instances
	instancesConfigMap := &core.ConfigMap{}
	if err := r.client.Get(ctx, kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: wiparser.InstanceConfigMap}, instancesConfigMap); err != nil {
		return "", fmt.Errorf("unable to get instance configmap: %w", err)
	}
	instanceUsername, err := wiparser.GetNodeUsername(instancesConfigMap.Data, &node)
	if err != nil {
		return "", err
	}
	encryptedUsername, err := crypto.EncryptToJSONString(instanceUsername, key)
	if err != nil {
		return "", fmt.Errorf("error encrypting node %s username: %w", node.GetName(), err)
	}
	return encryptedUsername, nil
}

// RemoveInvalidAnnotationsFromLinuxNodes makes a best effort to remove annotations applied by previous versions of WMCO.
func (r *SecretReconciler) RemoveInvalidAnnotationsFromLinuxNodes(config *rest.Config) error {
	// create a new clientset as this function will be called before the manager's client is started
	kc, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	nodes, err := kc.CoreV1().Nodes().List(context.TODO(), meta.ListOptions{LabelSelector: core.LabelOSStable + "=linux"})
	if err != nil {
		return fmt.Errorf("error getting Linux node list: %w", err)
	}
	// The public key hash was accidentally added to Linux nodes in WMCO 2.0 and must be removed.
	// The `/` in the annotation key needs to be escaped in order to not be considered a "directory" in the path.
	patchData, err := metadata.GenerateRemovePatch([]string{}, []string{nodeconfig.PubKeyHashAnnotation})
	if err != nil {
		return fmt.Errorf("error creating public key annotation add request: %w", err)
	}
	for _, node := range nodes.Items {
		if _, present := node.Annotations[nodeconfig.PubKeyHashAnnotation]; present == true {
			_, err = kc.CoreV1().Nodes().Patch(context.TODO(), node.GetName(), kubeTypes.JSONPatchType,
				patchData, meta.PatchOptions{})
			if err != nil {
				return fmt.Errorf("error removing public key annotation from node %s: %w", node.GetName(), err)
			}
		}
	}
	return nil
}

// mapToPrivateKeySecret is a mapping function that will always return a request for the cloud private key secret
func (r *SecretReconciler) mapToPrivateKeySecret(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{
		{NamespacedName: kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: secrets.PrivateKeySecret}},
	}
}
