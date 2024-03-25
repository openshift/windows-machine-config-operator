package controllers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	oconfig "github.com/openshift/api/config/v1"
	mapi "github.com/openshift/api/machine/v1beta1"
	mclient "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
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
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/signer"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
)

//+kubebuilder:rbac:groups=config.openshift.io,resources=clusteroperators,verbs=get;list;watch
//+kubebuilder:rbac:groups=machine.openshift.io,resources=machines,verbs=get;list;watch;delete
//+kubebuilder:rbac:groups=machine.openshift.io,resources=machinesets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;patch;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;
//+kubebuilder:rbac:groups="",resources=events,verbs=*

const (
	// maxUnhealthyCount is the maximum number of nodes that are not ready to serve at a given time.
	// TODO: https://issues.redhat.com/browse/WINC-524
	maxUnhealthyCount = 1
	// MachineOSLabel is the label used to identify the Windows Machines.
	MachineOSLabel = "machine.openshift.io/os-id"
	// WindowsMachineController is the name of this controller in logs and other outputs.
	WindowsMachineController = "windowsmachine"
	// IgnoreLabel is a label that will cause machines to be ignored by the Windows Machine controller
	IgnoreLabel = "windowsmachineconfig.openshift.io/ignore"
)

// WindowsMachineReconciler is used to create a controller which manages Windows Machine objects
type WindowsMachineReconciler struct {
	instanceReconciler
	// machineClient holds the information for machine client
	machineClient *mclient.MachineV1beta1Client
}

// NewWindowsMachineReconciler returns a pointer to a WindowsMachineReconciler
func NewWindowsMachineReconciler(mgr manager.Manager, clusterConfig cluster.Config, watchNamespace string) (*WindowsMachineReconciler, error) {
	// The client provided by the GetClient() method of the manager is a split client that will always hit the API
	// server when writing. When reading, the client will either use a cache populated by the informers backing the
	// controllers, or in certain cases read directly from the API server. It will read from the server both for
	// unstructured types, as well as exceptions specified when initializing the manager. All other times it will read
	// from the cache. Read operations using the default client should only be done against resources that are
	// specifically being watched by controllers in the operator.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	machineClient, err := mclient.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("error creating machine client: %w", err)
	}

	// Initialize prometheus configuration
	pc, err := metrics.NewPrometheusNodeConfig(clientset, watchNamespace)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize Prometheus configuration: %w", err)
	}

	return &WindowsMachineReconciler{
		instanceReconciler: instanceReconciler{
			client:               mgr.GetClient(),
			log:                  ctrl.Log.WithName("controller").WithName(WindowsMachineController),
			k8sclientset:         clientset,
			clusterServiceCIDR:   clusterConfig.Network().GetServiceCIDR(),
			recorder:             mgr.GetEventRecorderFor(WindowsMachineController),
			watchNamespace:       watchNamespace,
			prometheusNodeConfig: pc,
			platform:             clusterConfig.Platform(),
		},
		machineClient: machineClient,
	}, nil
}

// SetupWithManager sets up a new Secret controller
func (r *WindowsMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Watch for the Machine objects with label defined by MachineOSLabel
	machinePredicate := predicate.Funcs{
		// We need the create event to account for Machines that are in provisioned state but were created
		// before WMCO started running
		CreateFunc: func(e event.CreateEvent) bool {
			return r.isValidMachine(e.Object) && isWindowsMachine(e.Object.GetLabels())
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isValidMachine(e.ObjectNew) && isWindowsMachine(e.ObjectNew.GetLabels())
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return r.isValidMachine(e.Object) && isWindowsMachine(e.Object.GetLabels())
		},
		// process delete event
		DeleteFunc: func(e event.DeleteEvent) bool {
			// for Windows machines only
			return isWindowsMachine(e.Object.GetLabels())
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mapi.Machine{}, builder.WithPredicates(machinePredicate)).
		Watches(&core.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapNodeToMachine),
			builder.WithPredicates(outdatedWindowsNodePredicate(false))).
		Complete(r)
}

// mapNodeToMachine maps the given Windows node to its associated Machine
func (r *WindowsMachineReconciler) mapNodeToMachine(_ context.Context, object client.Object) []reconcile.Request {
	if !isWindowsNode(object) {
		return nil
	}

	// Map the Node to the associated Machine through the Node's UID
	machines, err := r.machineClient.Machines(cluster.MachineAPINamespace).List(context.TODO(),
		meta.ListOptions{LabelSelector: MachineOSLabel + "=Windows," + IgnoreLabel + "!=true"})
	if err != nil {
		r.log.Error(err, "could not get a list of machines")
	}
	for _, machine := range machines.Items {
		ok := machine.Status.Phase != nil &&
			len(machine.Status.Addresses) > 0 &&
			machine.Status.NodeRef != nil &&
			machine.Status.NodeRef.UID == object.GetUID()
		if ok {
			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Namespace: machine.GetNamespace(),
						Name:      machine.GetName(),
					},
				},
			}
		}
	}

	// Node doesn't match a machine, return
	return nil
}

// isWindowsMachine checks if the machine is a Windows machine or not
func isWindowsMachine(labels map[string]string) bool {
	if value, ok := labels[MachineOSLabel]; ok {
		if value == "Windows" {
			return true
		}
	}
	return false
}

// isValidMachine returns true if the Machine given object is a Machine with a properly populated status
func (r *WindowsMachineReconciler) isValidMachine(obj client.Object) bool {
	if value := obj.GetLabels()[IgnoreLabel]; value == "true" {
		return false
	}
	machine := &mapi.Machine{}

	// If this function is called on an object that equals nil, return false
	if obj == nil {
		r.log.Error(fmt.Errorf("machine object cannot be nil"), "invalid Machine", "object", obj)
		return false
	}

	var ok bool
	machine, ok = obj.(*mapi.Machine)
	if !ok {
		r.log.Error(fmt.Errorf("unable to typecast object to Machine"), "invalid Machine", "object", obj)
		return false
	}
	if machine.Status.Phase == nil {
		r.log.V(1).Info("Machine has no phase associated with it", "name", machine.Name)
		return false
	}

	_, err := getInternalIPAddress(machine.Status.Addresses)
	if err != nil {
		r.log.V(1).Info("invalid Machine", "name", machine.Name, "error", err)
		return false
	}

	return true
}

// Reconcile reads that state of the cluster for a Windows Machine object and makes changes based on the state read
// and what is in the Machine.Spec
// Note: The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *WindowsMachineReconciler) Reconcile(ctx context.Context,
	request ctrl.Request) (result ctrl.Result, reconcileErr error) {
	log := r.log.WithValues(WindowsMachineController, request.NamespacedName)
	log.V(1).Info("reconciling")

	// Prevent WMCO upgrades while Machine nodes are being processed
	if err := condition.MarkAsBusy(r.client, r.watchNamespace, r.recorder, WindowsMachineController); err != nil {
		return ctrl.Result{}, err
	}
	defer func() {
		reconcileErr = markAsFreeOnSuccess(r.client, r.watchNamespace, r.recorder, WindowsMachineController,
			result.Requeue, reconcileErr)
	}()

	// Create a new signer from the private key the instances will be configured with
	// Doing this before fetching the machine allows us to warn the user better about the missing private key
	var err error
	r.signer, err = signer.Create(kubeTypes.NamespacedName{Namespace: r.watchNamespace, Name: secrets.PrivateKeySecret},
		r.client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to get signer from secret %s: %w", request.NamespacedName, err)
	}

	// Fetch the Machine instance
	machine, err := r.machineClient.Machines(cluster.MachineAPINamespace).Get(ctx, request.Name, meta.GetOptions{})
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// In the case the machine was deleted, ensure that the metrics subsets are configured properly, so that
			// the current Windows nodes are properly reflected there.
			log.V(1).Info("not found")
			return ctrl.Result{}, r.prometheusNodeConfig.Configure()
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}
	// provisionedPhase is the status of the machine when it is in the `Provisioned` state
	provisionedPhase := "Provisioned"
	// runningPhase is the status of the machine when it is in the `Running` state, indicating that it is configured into a node
	runningPhase := "Running"
	// node is the Node object associated with the machine being reconciled, if any exists
	var node *core.Node
	if machine.Status.Phase == nil {
		// This condition should never be true as machine objects without a phase will be filtered out via the predicate functions
		return ctrl.Result{}, fmt.Errorf("could not get the phase associated with machine %s", machine.Name)
	} else if *machine.Status.Phase == runningPhase {
		// Machine has been configured into a node, we need to ensure that the version annotation exists. If it doesn't
		// the machine was not fully configured and needs to be configured properly.
		if machine.Status.NodeRef == nil {
			// NodeRef missing. Requeue and hope it is created. It never being created indicates an issue with the
			// machine api operator
			return ctrl.Result{}, fmt.Errorf("ready Windows machine %s missing NodeRef", machine.GetName())
		}
		// Populate the running Machine's Node object
		node = &core.Node{}
		err := r.client.Get(ctx, kubeTypes.NamespacedName{Namespace: machine.Status.NodeRef.Namespace,
			Name: machine.Status.NodeRef.Name}, node)
		if err != nil {
			// Do not requeue if associated node cannot be found (i.e. deleted) for a running machine
			if k8sapierrors.IsNotFound(err) {
				log.Info("the node associated with this machine does not exist, no-op", "name", machine.GetName())
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("could not get node associated with machine %s: %w", machine.GetName(),
				err)
		}

		if _, present := node.Annotations[metadata.VersionAnnotation]; present {
			// If the private key used to configure the machine is out of date, the machine should be deleted
			if node.Annotations[nodeconfig.PubKeyHashAnnotation] !=
				nodeconfig.CreatePubKeyHashAnnotation(r.signer.PublicKey()) {
				log.Info("deleting machine")
				deletionAllowed, err := r.isAllowedDeletion(machine)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("unable to determine if Machine can be deleted: %w", err)
				}
				if !deletionAllowed {
					log.Info("machine deletion restricted", "maxUnhealthyCount", maxUnhealthyCount)
					r.recorder.Eventf(machine, core.EventTypeWarning, "MachineDeletionRestricted",
						"Machine %v deletion restricted as the maximum unhealthy machines can`t exceed %v count",
						machine.Name, maxUnhealthyCount)
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, r.deleteMachine(machine)
			}
			if node.Annotations[metadata.VersionAnnotation] == version.Get() {
				// version annotation exists with a valid value, node is fully configured.
				// configure Prometheus when we have already configured Windows Nodes. This is required to update
				// Endpoints object if it gets reverted when the operator pod restarts.
				if err := r.prometheusNodeConfig.Configure(); err != nil {
					return ctrl.Result{}, fmt.Errorf("unable to configure Prometheus: %w", err)
				}
				return ctrl.Result{}, nil
			}
		}
	} else if *machine.Status.Phase != provisionedPhase {
		log.V(1).Info("machine not provisioned", "phase", *machine.Status.Phase)
		// configure Prometheus when a machine is not in `Running` or `Provisioned` phase. This configuration is
		// required to update Endpoints object when Windows machines are being deleted.
		if err := r.prometheusNodeConfig.Configure(); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to configure Prometheus: %w", err)
		}
		// Machine is not in provisioned or running state, nothing we should do as of now
		return ctrl.Result{}, nil
	}

	// validate userData secret
	if err := r.validateUserData(); err != nil {
		return ctrl.Result{}, fmt.Errorf("error validating userData secret: %w", err)
	}

	// Get the IP address associated with the Windows machine, if not error out to requeue again
	ipAddress, err := getInternalIPAddress(machine.Status.Addresses)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid machine %s: %w", machine.Name, err)
	}

	// Get the instance ID associated with the Windows machine.
	providerID := *machine.Spec.ProviderID
	if len(providerID) == 0 {
		return ctrl.Result{}, fmt.Errorf("empty provider ID associated with machine %s", machine.Name)
	}
	// Ex: aws:///us-east-1e/i-078285fdadccb2eaa
	// We always want the last entry which is the instanceID, and the first which is the provider name.
	providerTokens := strings.Split(providerID, "/")
	instanceID := providerTokens[len(providerTokens)-1]
	if len(instanceID) == 0 {
		return ctrl.Result{}, fmt.Errorf("unable to get instance ID from provider ID for machine %s", machine.Name)
	}

	log.Info("processing", "address", ipAddress)
	// Configure the Machine as an up-to-date Windows Worker node
	if err := r.configureMachine(ipAddress, instanceID, machine.Name, node); err != nil {
		var authErr *windows.AuthErr
		if errors.As(err, &authErr) {
			// SSH authentication errors with the Machine are non recoverable, stemming from a mismatch with the
			// userdata used to provision the machine and the current private key secret. The machine must be deleted and
			// re-provisioned.
			r.recorder.Eventf(machine, core.EventTypeWarning, "MachineSetupFailure",
				"Machine %s authentication failure", machine.Name)
			return ctrl.Result{}, r.deleteMachine(machine)
		}
		r.recorder.Eventf(machine, core.EventTypeWarning, "MachineSetupFailure",
			"Machine %s configuration failure", machine.Name)
		return ctrl.Result{}, err
	}
	r.recorder.Eventf(machine, core.EventTypeNormal, "MachineSetup",
		"Machine %s configured successfully", machine.Name)
	// configure Prometheus after a Windows machine is configured as a Node.
	if err := r.prometheusNodeConfig.Configure(); err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to configure Prometheus: %w", err)
	}
	return ctrl.Result{}, nil
}

// deleteMachine deletes the specified Machine
func (r *WindowsMachineReconciler) deleteMachine(machine *mapi.Machine) error {
	if !machine.GetDeletionTimestamp().IsZero() {
		// Delete already initiated
		return nil
	}

	if err := r.client.Delete(context.TODO(), machine); err != nil {
		r.recorder.Eventf(machine, core.EventTypeWarning, "MachineDeletionFailed",
			"Machine %v deletion failed: %v", machine.Name, err)
		return err
	}
	r.log.Info("machine has been remediated by deletion", "name", machine.GetName())
	r.recorder.Eventf(machine, core.EventTypeNormal, "MachineDeleted",
		"Machine %v has been remediated by deleting the Machine object", machine.Name)
	return nil
}

// getDefaultUsername returns the default username for a Windows instance
func (r *WindowsMachineReconciler) getDefaultUsername() string {
	// TODO: This should be changed so that the "core" user is used on all platforms for SSH connections.
	// https://issues.redhat.com/browse/WINC-430
	if r.platform == oconfig.AzurePlatformType {
		return "capi"
	}
	return "Administrator"
}

// configureMachine configures the given Windows VM, adding it as a node object to the cluster or upgrading it in place.
func (r *WindowsMachineReconciler) configureMachine(ipAddress, instanceID, machineName string, node *core.Node) error {
	// The name of the Machine must be the same as the hostname of the associated VM. This is currently not true in the
	// case of vSphere VMs provisioned by MAPI. In case of Linux, ignition was handling it. As we don't have an
	// equivalent of ignition in Windows, WMCO must correct this by changing the VM's hostname.
	// TODO: Remove this once we figure out how to do this via guestInfo in vSphere
	// 		https://bugzilla.redhat.com/show_bug.cgi?id=1876987
	// Windows Hostname could be changed in initial customizing, however Nutanix is using the same workflow as with vSphere
	hostname := ""
	if r.platform == oconfig.VSpherePlatformType || r.platform == oconfig.NutanixPlatformType {
		hostname = machineName
	}
	username := r.getDefaultUsername()
	instanceInfo, err := instance.NewInfo(ipAddress, username, hostname, false, node)
	if err != nil {
		return err
	}
	// Get private key to encrypt instance usernames
	privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return err
	}
	encryptedUsername, err := crypto.EncryptToJSONString(username, privateKeyBytes)
	if err != nil {
		return fmt.Errorf("unable to encrypt username for instance %s: %w", instanceInfo.Address, err)
	}

	if err := r.ensureInstanceIsUpToDate(instanceInfo, nil,
		map[string]string{UsernameAnnotation: encryptedUsername}); err != nil {
		return fmt.Errorf("unable to configure instance %s: %w", instanceID, err)
	}

	return nil
}

// validateUserData validates the userData secret. It returns error if the secret doesn`t contain the expected public
// key bytes.
func (r *WindowsMachineReconciler) validateUserData() error {
	if r.signer == nil {
		return fmt.Errorf("signer must not be nil")
	}

	userDataSecret := &core.Secret{}
	err := r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: secrets.UserDataSecret,
		Namespace: cluster.MachineAPINamespace}, userDataSecret)
	if err != nil {
		return fmt.Errorf("could not find Windows userData secret in required namespace: %w", err)
	}

	secretData := string(userDataSecret.Data["userData"][:])
	desiredUserDataSecret, err := secrets.GenerateUserData(r.platform, r.signer.PublicKey())
	if err != nil {
		return err
	}
	if string(desiredUserDataSecret.Data["userData"][:]) != secretData {
		return fmt.Errorf("invalid content for userData secret")
	}
	return nil
}

// isAllowedDeletion determines if the number of machines after deletion of the given machine doesn`t fall below the
// minHealthyCount
func (r *WindowsMachineReconciler) isAllowedDeletion(machine *mapi.Machine) (bool, error) {
	if len(machine.OwnerReferences) == 0 {
		return false, fmt.Errorf("machine has no owner reference")
	}
	machinesetName := machine.OwnerReferences[0].Name

	machines, err := r.machineClient.Machines(cluster.MachineAPINamespace).List(context.TODO(),
		meta.ListOptions{LabelSelector: MachineOSLabel + "=Windows"})
	if err != nil {
		return false, fmt.Errorf("cannot list Machines: %w", err)
	}

	// get Windows MachineSet
	windowsMachineSet, err := r.machineClient.MachineSets(cluster.MachineAPINamespace).Get(context.TODO(),
		machinesetName, meta.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("cannot get MachineSet: %w", err)
	}

	// Allow deletion if there is only one machine in the Windows MachineSet
	totalWindowsMachineCount := *windowsMachineSet.Spec.Replicas
	if maxUnhealthyCount == totalWindowsMachineCount {
		return true, nil
	}

	totalHealthy := 0
	for _, ma := range machines.Items {
		// Increment the count if the machine is identified as healthy and is a part of given Windows MachineSet and
		// on which deletion is not already initiated.
		if len(machine.OwnerReferences) != 0 && ma.OwnerReferences[0].Name == machinesetName &&
			r.isWindowsMachineHealthy(&ma) && ma.DeletionTimestamp.IsZero() {
			totalHealthy += 1
		}
	}

	unhealthyMachineCount := totalWindowsMachineCount - int32(totalHealthy)
	r.log.Info("unhealthy machine count for machineset", "name", machinesetName, "total", totalWindowsMachineCount,
		"unhealthy", unhealthyMachineCount)

	return unhealthyMachineCount < maxUnhealthyCount, nil
}

// isWindowsMachineHealthy determines if the given Machine object is healthy. A Windows machine is considered
// unhealthy if -
// 1. Machine is not in a 'Running' phase
// 2. Machine is not associated with a Node object
// 3. Associated Node object doesn't have a Version annotation
func (r *WindowsMachineReconciler) isWindowsMachineHealthy(machine *mapi.Machine) bool {
	if (machine.Status.Phase == nil || *machine.Status.Phase != "Running") &&
		machine.Status.NodeRef == nil {
		return false
	}

	// Get node associated with the machine
	node, err := r.k8sclientset.CoreV1().Nodes().Get(context.TODO(), machine.Status.NodeRef.Name, meta.GetOptions{})
	if err != nil {
		return false
	}
	_, present := node.Annotations[metadata.VersionAnnotation]
	if !present {
		return false
	}

	return true
}

// getInternalIPAddress returns the internal IP address of the Machine
func getInternalIPAddress(addresses []core.NodeAddress) (string, error) {
	// Get the IP address associated with the Windows machine, if not error out to requeue again
	if len(addresses) == 0 {
		return "", fmt.Errorf("no IP addresses defined")
	}
	for _, address := range addresses {
		// Only return the IPv4 address
		if address.Type == core.NodeInternalIP && net.ParseIP(address.Address).To4() != nil {
			return address.Address, nil
		}
	}
	return "", fmt.Errorf("no internal IP address associated")
}
