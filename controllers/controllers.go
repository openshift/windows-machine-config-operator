package controllers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"github.com/openshift/windows-machine-config-operator/pkg/metrics"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
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

// publicKeyRSA retrieves the public key used to access Windows instances in RSA form
func (r *instanceReconciler) publicKeyRSA() *rsa.PublicKey {
	// upgrade SSH public key to CryptoPublicKey interface, extract key data, then convert to RSA object
	sshPubKey := r.signer.PublicKey()
	cryptoKey := sshPubKey.(ssh.CryptoPublicKey)
	pubCrypto := cryptoKey.CryptoPublicKey()
	return pubCrypto.(*rsa.PublicKey)
}

// ecrypt takes in a string and proceeds to encrypt it using the SSH public key data
func (r *instanceReconciler) encrypt(plainText string) (string, error) {
	rsaPubKey := r.publicKeyRSA()
	encryptedBytes, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, rsaPubKey, []byte(plainText), nil)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(encryptedBytes), nil
}

// privateKeyRSA retrieves the private key used to access Windows instances in RSA form
func (r *instanceReconciler) privateKeyRSA() (*rsa.PrivateKey, error) {
	privateKeyBytes, err := secrets.GetPrivateKey(kubeTypes.NamespacedName{Namespace: r.watchNamespace,
		Name: secrets.PrivateKeySecret}, r.client)
	if err != nil {
		return nil, err
	}

	decodedBlock, _ := pem.Decode(privateKeyBytes)
	rsaPrivateKey, err := x509.ParsePKCS1PrivateKey(decodedBlock.Bytes)
	if err != nil {
		return nil, errors.Wrap(err, "unable to convert SSH private key to RSA standard")
	}

	return rsaPrivateKey, nil
}

// decrypt takes in encrypted text and attempts to decrypt the string using the SSH private key data
func (r *instanceReconciler) decrypt(cipherText string) (string, error) {
	usernameData, err := base64.StdEncoding.DecodeString(cipherText)
	if err != nil {
		return "", err
	}

	rsaPrivateKey, err := r.privateKeyRSA()
	decryptedBytes, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, rsaPrivateKey, usernameData, nil)
	if err != nil {
		return "", err
	}

	return string(decryptedBytes), nil
}

// instanceFromNode returns an instance object for the given node. Requires a username that can be used to SSH into the
// instance to be annotated on the node.
func (r *instanceReconciler) instanceFromNode(node *core.Node) (*instances.InstanceInfo, error) {
	usernameAnnotation := node.Annotations[UsernameAnnotation]
	if usernameAnnotation == "" {
		return nil, errors.New("node is missing valid username annotation")
	}
	addr, err := GetAddress(node.Status.Addresses)
	if err != nil {
		return nil, err
	}

	// Decypt username annotation to plain text
	decryptedUsername, err := r.decrypt(usernameAnnotation)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to decrypt username annotation for node %s", node.Name)
	}

	return instances.NewInstanceInfo(addr, decryptedUsername, ""), nil
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
