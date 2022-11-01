package controllers

import (
	"context"
	"net"

	"github.com/go-logr/logr"
	config "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/condition"
	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/version"
)

const (
	// machineAPINamespace is the name of the namespace in which machine objects and userData secret is created.
	machineAPINamespace = "openshift-machine-api"
)

// instanceReconciler contains everything needed to perform actions on a Windows instance
type instanceReconciler struct {
	// Client is the cache client
	client client.Client
	log    logr.Logger
	// k8sclientset holds the kube client that is needed for nodeconfig
	k8sclientset *kubernetes.Clientset
	// clusterServiceCIDR holds the cluster network service CIDR
	clusterServiceCIDR string
	// watchNamespace is the namespace that should be watched for configmaps
	watchNamespace string
	// vxlanPort is the custom VXLAN port
	vxlanPort string
	// signer is a signer created from the user's private key
	signer ssh.Signer
	// prometheusNodeConfig stores information required to configure Prometheus
	prometheusNodeConfig *metrics.PrometheusNodeConfig
	// recorder to generate events
	recorder record.EventRecorder
	// platform indicates the cloud on which the cluster is running
	platform config.PlatformType
}

// ensureInstanceIsUpToDate ensures that the given instance is configured as a node and upgraded to the specifications
// defined by the current version of WMCO. If labelsToApply/annotationsToApply is not nil, the node will have the
// specified annotations and/or labels applied to it.
func (r *instanceReconciler) ensureInstanceIsUpToDate(instanceInfo *instance.Info, labelsToApply, annotationsToApply map[string]string) error {
	if instanceInfo == nil {
		return errors.New("instance cannot be nil")
	}

	// Instance is up to date, do nothing
	if instanceInfo.UpToDate() {
		// Instance being up to date indicates that node object is present with the version annotation
		r.log.Info("instance is up to date", "node", instanceInfo.Node.GetName(), "version",
			instanceInfo.Node.GetAnnotations()[metadata.VersionAnnotation])
		return nil
	}

	nc, err := nodeconfig.NewNodeConfig(r.client, r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, instanceInfo,
		r.signer, labelsToApply, annotationsToApply, r.platform)
	if err != nil {
		return errors.Wrap(err, "failed to create new nodeconfig")
	}

	// Check if the instance was configured by a previous version of WMCO and must be deconfigured before being
	// configured again.
	if instanceInfo.UpgradeRequired() {
		// Instance requiring an upgrade indicates that node object is present with the version annotation
		r.log.Info("instance requires upgrade", "node", instanceInfo.Node.GetName(), "version",
			instanceInfo.Node.GetAnnotations()[metadata.VersionAnnotation], "expected version", version.Get())
		if err := nc.Deconfigure(); err != nil {
			return err
		}
	}

	return nc.Configure()
}

// instanceFromNode returns an instance object for the given node. Requires a username that can be used to SSH into the
// instance to be annotated on the node.
func (r *instanceReconciler) instanceFromNode(node *core.Node) (*instance.Info, error) {
	usernameAnnotation := node.Annotations[UsernameAnnotation]
	if usernameAnnotation == "" {
		return nil, errors.New("node is missing valid username annotation")
	}
	addr, err := GetAddress(node.Status.Addresses)
	if err != nil {
		return nil, err
	}

	// Decrypt username annotation to plain text using private key
	privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return nil, err
	}
	username, err := crypto.DecryptFromJSONString(usernameAnnotation, privateKeyBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to decrypt username annotation for node %s", node.Name)
	}

	return instance.NewInfo(addr, username, "", false, node)
}

// updateKubeletCA updates the kubelet CA in the node, by copying the kubelet CA file content to the Windows instance
func (r *instanceReconciler) updateKubeletCA(node core.Node, contents []byte) error {
	winInstance, err := r.instanceFromNode(&node)
	if err != nil {
		return errors.Wrapf(err, "error creating instance for node %s", node.Name)
	}
	nodeConfig, err := nodeconfig.NewNodeConfig(r.client, r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort,
		winInstance, r.signer, nil, nil, r.platform)
	if err != nil {
		return errors.Wrapf(err, "error creating nodeConfig for instance %s", winInstance.Address)
	}
	r.log.Info("updating kubelet CA client certificates in", "node", node.Name)
	return nodeConfig.UpdateKubeletClientCA(contents)
}

// reconcileKubeletClientCA reconciles the kube-apiserver certificate rotation by copying the bundle CA in the updated
// ConfigMap to all Windows nodes. This is required by kubelet to recognize the kube-apiserver client. No drain or
// restart required, the bundle CA file is loaded dynamically by the kubelet service running on the Windows Instance.
func (r *instanceReconciler) reconcileKubeletClientCA(ctx context.Context, bundleCAConfigMap *core.ConfigMap) error {
	// get the ConfigMap that contains the initial CA certificates
	initialCAConfigMap, err := certificates.GetInitialCAConfigMap(ctx, r.client)
	if err != nil {
		return err
	}
	// merge the initial and current CA ConfigMaps for the kube API Server signer, using the specific common-name that
	// matches the signer subject.
	kubeAPIServerServingCABytes, err := certificates.MergeCAsConfigMaps(initialCAConfigMap, bundleCAConfigMap,
		"kube-apiserver-to-kubelet-signer")

	// fetch all Windows nodes (Machine and BYOH instances)
	winNodes := &core.NodeList{}
	if err = r.client.List(ctx, winNodes, client.MatchingLabels{core.LabelOSStable: "windows"}); err != nil {
		return errors.Wrap(err, "error listing Windows nodes")
	}
	r.log.V(1).Info("processing", "node count", len(winNodes.Items))
	// loop Windows nodes and trigger kubelet CA update
	for _, winNode := range winNodes.Items {
		if err := r.updateKubeletCA(winNode, kubeAPIServerServingCABytes); err != nil {
			return errors.Wrapf(err, "error updating kubelet CA certificate in node %s", winNode.Name)
		}
	}
	return nil
}

// GetAddress returns a non-ipv6 address that can be used to reach a Windows node. This can be either an ipv4
// or dns address.
func GetAddress(addresses []core.NodeAddress) (string, error) {
	for _, addr := range addresses {
		if addr.Type == core.NodeInternalIP || addr.Type == core.NodeInternalDNS {
			// filter out ipv6
			if net.ParseIP(addr.Address) != nil && net.ParseIP(addr.Address).To4() == nil {
				continue
			}
			return addr.Address, nil
		}
	}
	return "", errors.New("no usable address")
}

// deconfigureInstance deconfigures the instance associated with the given node, removing the node from the cluster.
func (r *instanceReconciler) deconfigureInstance(node *core.Node) error {
	instance, err := r.instanceFromNode(node)
	if err != nil {
		return errors.Wrap(err, "unable to create instance object from node")
	}

	nc, err := nodeconfig.NewNodeConfig(r.client, r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, instance, r.signer,
		nil, nil, r.platform)
	if err != nil {
		return errors.Wrap(err, "failed to create new nodeconfig")
	}

	if err = nc.Deconfigure(); err != nil {
		return err
	}
	if err = r.client.Delete(context.TODO(), instance.Node); err != nil {
		return errors.Wrapf(err, "error deleting node %s", instance.Node.GetName())
	}
	return nil
}

// windowsNodeVersionChangePredicate returns a predicate whose filter catches Windows nodes that indicate a version
// change either through deletion away from an old version or creation/update to the latest WMCO version
func windowsNodeVersionChangePredicate() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			// catch Machine-backed Windows node upgrades as they are re-created
			return e.Object.GetLabels()[core.LabelOSStable] == "windows" &&
				e.Object.GetAnnotations()[metadata.VersionAnnotation] == version.Get()
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// catch BYOH Windows node upgrades to the current WMCO version as they are re-configured in place
			return e.ObjectNew.GetLabels()[core.LabelOSStable] == "windows" &&
				(e.ObjectOld.GetAnnotations()[metadata.VersionAnnotation] !=
					e.ObjectNew.GetAnnotations()[metadata.VersionAnnotation]) &&
				e.ObjectNew.GetAnnotations()[metadata.VersionAnnotation] == version.Get()
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetLabels()[core.LabelOSStable] == "windows" &&
				e.Object.GetAnnotations()[metadata.VersionAnnotation] == version.Get()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// catch if a node stuck at an older WMCO version is deleted
			return e.Object.GetLabels()[core.LabelOSStable] == "windows" &&
				e.Object.GetAnnotations()[metadata.VersionAnnotation] != version.Get()
		},
	}
}

// outdatedWindowsNodePredicate returns a predicate which filters out all node objects that are not up-to-date Windows
// nodes. Up-to-date refers to the version annotation and public key hash annotations.
// If BYOH is true, only BYOH nodes will be allowed through, else no BYOH nodes will be allowed.
func outdatedWindowsNodePredicate(byoh bool) predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return isValidWindowsNode(e.Object, byoh) &&
				e.Object.GetAnnotations()[metadata.VersionAnnotation] != version.Get()
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			if !isValidWindowsNode(e.ObjectNew, byoh) {
				return false
			}
			if e.ObjectNew.GetAnnotations()[metadata.VersionAnnotation] != version.Get() ||
				e.ObjectNew.GetAnnotations()[nodeconfig.PubKeyHashAnnotation] !=
					e.ObjectOld.GetAnnotations()[nodeconfig.PubKeyHashAnnotation] {
				return true
			}
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return isValidWindowsNode(e.Object, byoh) &&
				e.Object.GetAnnotations()[metadata.VersionAnnotation] != version.Get()
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return isValidWindowsNode(e.Object, byoh)
		},
	}

}

// getVersionAnnotations returns a map whose keys are the WMCO versions that have configured any Windows nodes
func getVersionAnnotations(nodes []core.Node) map[string]struct{} {
	versions := make(map[string]struct{})
	for _, node := range nodes {
		if versionAnnotation, present := node.Annotations[metadata.VersionAnnotation]; present {
			versions[versionAnnotation] = struct{}{}
		}
	}
	return versions
}

// isValidWindowsNode returns true if the node object has the Windows label and the BYOH
// label present for only the BYOH nodes based on the value of the byoh boolean parameter.
func isValidWindowsNode(o client.Object, byoh bool) bool {
	if o.GetLabels()[core.LabelOSStable] != "windows" {
		return false
	}
	if (byoh && o.GetLabels()[BYOHLabel] != "true") ||
		(!byoh && o.GetLabels()[BYOHLabel] == "true") {
		return false
	}
	return true
}

// markAsFreeOnSuccess is called after a controller's Reconcile function returns. If the given controller finished
// reconciling without error or requesting a requeue event, the controller is marked as free.
// When all controllers are free, WMCO upgrades are unblocked.
func markAsFreeOnSuccess(c client.Client, watchNamespace string, recorder record.EventRecorder, controllerName string,
	requeue bool, err error) error {
	if !requeue && err == nil {
		return condition.MarkAsFree(c, watchNamespace, recorder, controllerName)
	}
	return err
}
