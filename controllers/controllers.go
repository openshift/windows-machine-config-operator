package controllers

import (
	"context"
	"net"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/openshift/windows-machine-config-operator/pkg/crypto"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/version"
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

	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, instanceInfo, r.signer,
		labelsToApply, annotationsToApply)
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

	return instance.NewInfo(addr, username, "", node), nil
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

	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, instance, r.signer,
		nil, nil)
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

// windowsNodePredicate returns a predicate which filters out all node objects that are not Windows nodes.
// If BYOH is true, only BYOH nodes will be allowed through, else no BYOH nodes will be allowed.
func windowsNodePredicate(byoh bool) predicate.Funcs {
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
