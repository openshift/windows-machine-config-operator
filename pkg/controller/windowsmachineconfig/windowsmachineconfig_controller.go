package windowsmachineconfig

import (
	"context"
	"fmt"
	"io/ioutil"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	wmcapi "github.com/openshift/windows-machine-config-operator/pkg/apis/wmc/v1alpha1"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/windowsmachineconfig/nodeconfig"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// ControllerName is the name of the WMC controller
	ControllerName = "windowsmachineconfig-controller"
)

var log = logf.Log.WithName(ControllerName)

// Add creates a new WindowsMachineConfig Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, clusterServiceCIDR string) error {
	reconciler, err := newReconciler(mgr, clusterServiceCIDR)
	if err != nil {
		return errors.Wrapf(err, "could not create %s reconciler", ControllerName)
	}
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, clusterServiceCIDR string) (reconcile.Reconciler, error) {
	// TODO: This should be moved out to validation for reconciler struct.
	// 		Jira story: https://issues.redhat.com/browse/WINC-277
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

	windowsVMs := make(map[types.WindowsVM]bool)
	vmTracker, err := newTracker(clientset)
	if err != nil {
		return nil, errors.Wrap(err, "failed to instantiate tracker")
	}

	signer, err := createSigner()
	if err != nil {
		return nil, errors.Wrapf(err, "error creating signer using private key: %v", wkl.PrivateKeyPath)
	}

	return &ReconcileWindowsMachineConfig{client: client,
			scheme:             mgr.GetScheme(),
			k8sclientset:       clientset,
			tracker:            vmTracker,
			windowsVMs:         windowsVMs,
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
	// TODO: Add a predicate here. As of now, we get event notifications for all the WindowsMachineConfig objects, we
	//		want the predicate to filter the WMC object called `instance`
	//		Jira Story: https://issues.redhat.com/browse/WINC-282
	// Watch for changes to primary resource WindowsMachineConfig
	err = c.Watch(&source.Kind{Type: &wmcapi.WindowsMachineConfig{}}, &handler.EnqueueRequestForObject{},
		// prevent reconciling due to a status update
		predicate.GenerationChangedPredicate{})
	if err != nil {
		return errors.Wrap(err, "could not create watch on WindowsMachineConfig objects")
	}
	// Watch for the Machine objects
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
		ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-machine-api"},
	}}, &handler.EnqueueRequestForObject{}, predicateFilter)
	if err != nil {
		return errors.Wrap(err, "could not create watch on Machine objects")
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
	tracker *tracker
	// statusMgr is used to keep track of and update the WMC status
	statusMgr *StatusManager
	// clusterServiceCIDR holds the cluster network service CIDR
	clusterServiceCIDR string
	// signer is a signer created from the user's private key
	signer ssh.Signer
	// recorder to generate events
	recorder record.EventRecorder
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
	r.cloudProvider, err = cloudprovider.CloudProviderFactory("",
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
	wmcoObject := false
	// Fetch the Machine instance
	machine := &mapi.Machine{}
	provisionedPhase := "Provisioned"
	if err := r.client.Get(context.TODO(), request.NamespacedName, machine); err != nil {
		wmcoObject = true
		// TODO: Uncomment this when we switch to Machine-api completely in WINC-429
		//if k8sapierrors.IsNotFound(err) {
		//	// Request object not found, could have been deleted after reconcile request.
		//	// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
		//	// Return and don't requeue
		//
		//	return reconcile.Result{}, nil
		//
		//}
		//// Error reading the object - requeue the request.
		//return reconcile.Result{}, err
	}
	if !wmcoObject {
		if machine.Status.Phase == nil {
			// Phase can be nil and should be ignored by WMCO
			return reconcile.Result{}, nil
		}
		if *machine.Status.Phase != provisionedPhase {
			// If the Machine is not in provisioned state, WMCO shouldn't care about it
			return reconcile.Result{}, nil
		}
		r.recorder.Eventf(machine, corev1.EventTypeNormal, "WMCO Setup",
			"Machine %s Provisioned Successfully", machine.Name)
		return reconcile.Result{}, nil
	}

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

	r.statusMgr = NewStatusManager(r.client, request.NamespacedName)
	if err := r.getCloudProvider(instance); err != nil {
		// Failed to get cloud provider so make sure we reflect that in the CR status
		r.statusMgr.setStatusConditions([]wmcapi.WindowsMachineConfigCondition{
			*wmcapi.NewWindowsMachineConfigCondition(wmcapi.Degraded, corev1.ConditionTrue,
				wmcapi.CloudProviderAPIFailureReason, fmt.Sprintf("could not get cloud provider: %s", err))})
		if err = r.statusMgr.updateStatus(); err != nil {
			log.Error(err, "error updating status")
		}
		// Not going to requeue as an issue here indicates a problem with the provided credentials
		log.Error(err, "could not get cloud provider")
		return reconcile.Result{}, nil
	}

	if err := r.createUserDataSecret(); err != nil {
		log.Error(err, "error creating user data secret")
	}

	// Get the current number of Windows VMs created by WMCO.
	// TODO: Get all the running Windows nodes in the cluster
	//		jira story: https://issues.redhat.com/browse/WINC-280
	windowsNodes, err := r.k8sclientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{LabelSelector: nodeconfig.WindowsOSLabel})
	if err != nil {
		// This is most likely a permission error
		return reconcile.Result{}, errors.Wrap(err, "unable to get count of Windows nodes")
	}

	// Get the current count of required number of Windows VMs
	currentCountOfWindowsVMs := len(windowsNodes.Items)

	// Add or remove nodes
	nodeCount, nodeReconcileErrs := r.reconcileWindowsNodes(instance.Spec.Replicas, currentCountOfWindowsVMs)

	// Update all conditions and node count
	r.statusMgr.joinedVMCount = nodeCount
	r.statusMgr.setDegradedCondition(nodeReconcileErrs)
	r.statusMgr.setStatusConditions([]wmcapi.WindowsMachineConfigCondition{
		*wmcapi.NewWindowsMachineConfigCondition(wmcapi.Reconciling, corev1.ConditionFalse, "", "")})
	if err = r.statusMgr.updateStatus(); err != nil {
		// Its important that we update status after reconciliation. Log out any reconcile errors and requeue
		log.Error(fmt.Errorf("%v", nodeReconcileErrs), "error reconciling")
		return reconcile.Result{}, errors.Wrap(err, "error updating status")
	}

	// Now that we've updated the status we can return a possible reconcile error
	if nodeReconcileErrs != nil {
		return reconcile.Result{}, fmt.Errorf("reconcile error: %v", nodeReconcileErrs)
	}
	return reconcile.Result{}, nil
}

// reconcileWindowsNodes reconciles the Windows nodes so that required number of the Windows nodes are present in the
// cluster. Returns the new node count and any errors that occurred
func (r *ReconcileWindowsMachineConfig) reconcileWindowsNodes(desired, current int) (int, []ReconcileError) {
	var errs []ReconcileError
	var successCount int
	var newNodeCount int
	log.Info("replicas", "current", current, "desired", desired)
	if current != desired {
		// Update status to reflect that operator is reconciling
		r.statusMgr.setStatusConditions([]wmcapi.WindowsMachineConfigCondition{
			*wmcapi.NewWindowsMachineConfigCondition(wmcapi.Reconciling, corev1.ConditionTrue, "", "")})
		if err := r.statusMgr.updateStatus(); err != nil {
			errs = append(errs, newReconcileError(wmcapi.StatusFailureReason, err))
		}
	}
	if desired < current {
		successCount, errs = r.removeWorkerNodes(current - desired)
		newNodeCount = current - successCount
	} else if desired > current {
		successCount, errs = r.addWorkerNodes(desired - current)
		newNodeCount = current + successCount
	} else if desired == current {
		return current, nil
	}

	log.V(1).Info("starting tracker reconciliation")
	if err := r.tracker.Reconcile(); err != nil {
		errs = append(errs, newReconcileError(wmcapi.TrackerFailureReason, err))
	}
	log.V(1).Info("completed tracker reconciliation")

	return newNodeCount, errs
}

// removeWorkerNode terminates the underlying VM and removes the given vm from the list of VMs
func (r *ReconcileWindowsMachineConfig) removeWorkerNode(vm types.WindowsVM) ReconcileError {
	// VM is missing credentials, this can occur if there was a failure initially creating it. We can consider the
	// actual VM terminated as there is nothing we can do with it.
	if vm.GetCredentials() == nil || len(vm.GetCredentials().GetInstanceId()) == 0 {
		r.tracker.deleteWindowsVM(vm)
		return nil
	}

	// Terminate the instance via its instance id
	id := vm.GetCredentials().GetInstanceId()
	log.V(1).Info("destroying the Windows VM", "ID", id)

	// Delete the Windows VM from cloud provider
	if err := r.cloudProvider.DestroyWindowsVM(id); err != nil {
		return newReconcileError(wmcapi.VMTerminationFailureReason,
			errors.Wrapf(err, "error destroying VM with ID %s", id))
	}

	// Remove VM from our list of tracked VMs
	r.tracker.deleteWindowsVM(vm)
	log.Info("Windows worker has been removed from the cluster", "ID", id)
	return nil
}

// removeWorkerNodes removes the required number of Windows VMs from the cluster and returns a bool indicating the
// success. Returns the actual number of removed nodes and any associated errors.
func (r *ReconcileWindowsMachineConfig) removeWorkerNodes(count int) (int, []ReconcileError) {
	var errs []ReconcileError
	// From the list of Windows VMs choose randomly count number of VMs.
	for i := 0; i < count; i++ {
		// Choose of the Windows worker nodes randomly
		vm := r.tracker.chooseRandomNode()
		if vm == nil {
			errs = append(errs, newReconcileError(wmcapi.VMTerminationFailureReason,
				fmt.Errorf("expected VM and got a nil value")))
			continue
		}
		if err := r.removeWorkerNode(vm); err != nil {
			errs = append(errs, err)
		}
	}

	// If any of the Windows VM fails to get removed consider this as a failure and return false
	if len(errs) > 0 {
		return count - len(errs), errs
	}
	return count, nil
}

// addWorkerNode creates a new Windows VM and configures it, adding it as a node object to the cluster
func (r *ReconcileWindowsMachineConfig) addWorkerNode() (types.WindowsVM, ReconcileError) {
	// Create Windows VM in the cloud provider
	log.V(1).Info("creating a Windows VM")
	vm, err := r.cloudProvider.CreateWindowsVMWithPrivateSubnet()
	if err != nil {
		return nil, newReconcileError(wmcapi.VMCreationFailureReason, errors.Wrap(err, "error creating windows VM"))
	}

	log.V(1).Info("configuring the Windows VM", "ID", vm.GetCredentials().GetInstanceId())

	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, vm, r.clusterServiceCIDR)
	if err != nil {
		// TODO: Unwrap to extract correct error
		if cleanupErr := r.removeWorkerNode(vm); cleanupErr != nil {
			log.Error(cleanupErr, "failed to cleanup VM", "VM", vm.GetCredentials().GetInstanceId())
		}

		return nil, newReconcileError(wmcapi.VMConfigurationFailureReason,
			errors.Wrap(err, "failed to configure Windows VM"))
	}
	if err := nc.Configure(); err != nil {
		// TODO: Unwrap to extract correct error
		if cleanupErr := r.removeWorkerNode(vm); cleanupErr != nil {
			log.Error(cleanupErr, "failed to cleanup VM", "VM", vm.GetCredentials().GetInstanceId())
		}

		return nil, newReconcileError(wmcapi.VMConfigurationFailureReason,
			errors.Wrap(err, "failed to configure Windows VM"))
	}

	log.Info("Windows VM has joined the cluster as a worker node", "ID", nc.GetCredentials().GetInstanceId())
	return vm, nil
}

// addWorkerNodes creates the required number of Windows VMs and configures them to make
// them a worker node. Returns the number of nodes added and the associated errors.
func (r *ReconcileWindowsMachineConfig) addWorkerNodes(count int) (int, []ReconcileError) {
	var errs []ReconcileError
	for i := 0; i < count; i++ {
		// Create and configure a new Windows VM
		vm, err := r.addWorkerNode()
		if err != nil {
			log.Error(err, "error adding a Windows worker node")
			errs = append(errs, err)
			continue
		}

		// update the windowsVMs map with the new VM
		r.tracker.addWindowsVM(vm)
	}

	// If any of the Windows VM fails to get created consider this as a failure and return false
	if len(errs) > 0 {
		return count - len(errs), errs
	}
	return count, nil
}

// createUserDataSecret creates a secret 'windows-user-data' in 'openshift-machine-api'
// namespace. This secret will be used to inject cloud provider user data for creating
// windows machines
func (r *ReconcileWindowsMachineConfig) createUserDataSecret() error {
	if r.signer == nil {
		return errors.Errorf("failed to retrieve signer for private key: %v", wkl.PrivateKeyPath)
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(r.signer.PublicKey())
	if pubKeyBytes == nil {
		return errors.Errorf("failed to retrieve public key using signer for private key: %v", wkl.PrivateKeyPath)
	}

	// sshd service is started to create the default sshd_config file. This file is modified
	// for enabling publicKey auth and the service is restarted for the changes to take effect.
	userDataSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "windows-user-data",
			Namespace: "openshift-machine-api",
		},
		Data: map[string][]byte{
			"userData": []byte(`<powershell>
			Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
			$firewallRuleName = "ContainerLogsPort"
			$containerLogsPort = "10250"
			New-NetFirewallRule -DisplayName $firewallRuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $containerLogsPort -EdgeTraversalPolicy Allow
			Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force
			Install-Module -Force OpenSSHUtils
			Set-Service -Name ssh-agent -StartupType ‘Automatic’
			Set-Service -Name sshd -StartupType ‘Automatic’
			Start-Service ssh-agent
			Start-Service sshd
			$pubKeyConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
			$pubKeyConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
 			$passwordConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
			$passwordConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$authFileConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys','#AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys'
			$authFileConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$pubKeyLocationConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'Match Group administrators','#Match Group administrators'
			$pubKeyLocationConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			Restart-Service sshd
			New-item -Path $env:USERPROFILE -Name .ssh -ItemType Directory -force
			echo "` + string(pubKeyBytes[:]) + `"| Out-File $env:USERPROFILE\.ssh\authorized_keys -Encoding ascii
			</powershell>
			<persist>true</persist>`),
		},
	}

	// check if the userDataSecret already exists
	err := r.client.Get(context.TODO(), kubeTypes.NamespacedName{Name: userDataSecret.Name, Namespace: userDataSecret.Namespace}, &corev1.Secret{})
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			log.Info("Creating a new Secret", "Secret.Namespace", userDataSecret.Namespace, "Secret.Name", userDataSecret.Name)
			err = r.client.Create(context.TODO(), userDataSecret)
			if err != nil {
				return errors.Wrap(err, "error creating windows user data secret")
			}
		}
		return errors.Wrap(err, "error creating windows user data secret")
	}
	return nil
}

// createSigner creates a signer using the private key from the privateKeyPath
func createSigner() (ssh.Signer, error) {
	privateKeyBytes, err := ioutil.ReadFile(wkl.PrivateKeyPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find private key from path: %v", wkl.PrivateKeyPath)
	}

	signer, err := ssh.ParsePrivateKey(privateKeyBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse private key: %v", wkl.PrivateKeyPath)
	}
	return signer, nil
}

// ReconcileError fulfils the error interface while also including a human readable reason for the error
type ReconcileError interface {
	error
	// reason returns a computer readable reason for the error
	reason() string
}

// reconcileError is an implementation of the ReconcileError interface
type reconcileError struct {
	// degradationReason is a computer readable reason for the error
	degradationReason string
	// err is a human readable error
	err error
}

func (e *reconcileError) Error() string {
	return e.degradationReason + ": " + e.err.Error()
}

func (e *reconcileError) reason() string {
	return e.degradationReason
}

// newReconcileError returns a pointer to a new reconcileError
func newReconcileError(reason string, err error) *reconcileError {
	return &reconcileError{degradationReason: reason, err: err}
}
