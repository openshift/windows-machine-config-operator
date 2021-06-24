package controllers

import (
	"net"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
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

// configureInstance adds the specified instance to the cluster. if hostname is not empty, the instance's hostname will be
// changed to the passed in value. If annotations is not nil, the node will have the specified annotations applied to
// it.
func (r *instanceReconciler) configureInstance(instance *instances.InstanceInfo, annotations map[string]string) error {
	nc, err := nodeconfig.NewNodeConfig(r.k8sclientset, r.clusterServiceCIDR, r.vxlanPort, instance, r.signer,
		annotations)
	if err != nil {
		return errors.Wrap(err, "failed to create new nodeconfig")
	}
	if err := nc.Configure(); err != nil {
		return errors.Wrap(err, "failed to configure Windows instance")
	}
	return nil
}

// instanceFromNode returns an instance object for the given node. Requires a username that can be used to SSH into the
// instance to be annotated on the node.
func (r *instanceReconciler) instanceFromNode(node *core.Node) (*instances.InstanceInfo, error) {
	if node.Annotations[UsernameAnnotation] == "" {
		return nil, errors.New("node is missing valid username annotation")
	}
	addr, err := GetAddress(node.Status.Addresses)
	if err != nil {
		return nil, err
	}
	return instances.NewInstanceInfo(addr, node.Annotations[UsernameAnnotation], ""), nil
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
		nil)
	if err != nil {
		return errors.Wrap(err, "failed to create new nodeconfig")
	}
	return nc.Deconfigure()
}
