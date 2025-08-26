package nodeconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	ignCfgTypes "github.com/coreos/ignition/v2/config/v3_5/types"
	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	mcfg "github.com/openshift/api/machineconfiguration/v1"
	"github.com/vincent-petithory/dataurl"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	k8sapierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/kubectl/pkg/drain"
	kubeletconfigv1 "k8s.io/kubelet/config/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
	"github.com/openshift/windows-machine-config-operator/pkg/registries"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
)

//+kubebuilder:rbac:groups="apps",resources=daemonsets,verbs=get

const (
	// HybridOverlaySubnet is an annotation applied by the cluster network operator which is used by the hybrid overlay
	HybridOverlaySubnet = "k8s.ovn.org/hybrid-overlay-node-subnet"
	// HybridOverlayMac is an annotation applied by the hybrid-overlay
	HybridOverlayMac = "k8s.ovn.org/hybrid-overlay-distributed-router-gateway-mac"
	// WindowsOSLabel is the label applied when kubelet is ran to identify Windows nodes
	WindowsOSLabel = "node.openshift.io/os_id=Windows"
	// WorkerLabel is the label that needs to be applied to the Windows node to make it worker node
	WorkerLabel = "node-role.kubernetes.io/worker"
	// PubKeyHashAnnotation corresponds to the public key present on the VM
	PubKeyHashAnnotation = "windowsmachineconfig.openshift.io/pub-key-hash"
	// KubeletClientCAFilename is the name of the CA certificate file required by kubelet to interact
	// with the kube-apiserver client
	KubeletClientCAFilename = "kubelet-ca.crt"
	// mcoNamespace is the namespace the Machine Config Server is deployed in, which manages the node bootsrapper secret
	mcoNamespace = "openshift-machine-config-operator"
	// mcoBootstrapSecret is the resource name that holds the cert and token required to create the bootstrap kubeconfig
	mcoBootstrapSecret = "node-bootstrapper-token"
	// MccName is the name of the Machine Config Controller object
	MccName = "machine-config-controller"
)

// NodeConfig holds the information to make the given VM a kubernetes node. As of now, it holds the information
// related to kubeclient and the windowsVM.
type NodeConfig struct {
	client client.Client
	// k8sclientset holds the information related to kubernetes clientset
	k8sclientset *kubernetes.Clientset
	// Windows holds the information related to the windows VM
	windows.Windows
	// Node holds the information related to node object
	node *core.Node
	// publicKeyHash is the hash of the public key present on the VM
	publicKeyHash string
	// clusterServiceCIDR holds the service CIDR for cluster
	clusterServiceCIDR string
	log                logr.Logger
	// additionalAnnotations are extra annotations that should be applied to configured nodes
	additionalAnnotations map[string]string
	// additionalLabels are extra labels that should be applied to configured nodes
	additionalLabels map[string]string
	// platformType holds the name of the platform where cluster is deployed
	platformType configv1.PlatformType
	// wmcoNamespace is the namespace WMCO is deployed to
	wmcoNamespace string
}

// ErrWriter is a wrapper to enable error-level logging inside kubectl drainer implementation
type ErrWriter struct {
	log logr.Logger
}

func (ew ErrWriter) Write(p []byte) (n int, err error) {
	// log error
	ew.log.Error(err, string(p))
	return len(p), nil
}

// OutWriter is a wrapper to enable info-level logging inside kubectl drainer implementation
type OutWriter struct {
	log logr.Logger
}

func (ow OutWriter) Write(p []byte) (n int, err error) {
	// log info
	ow.log.Info(string(p))
	return len(p), nil
}

// NewNodeConfig creates a new instance of NodeConfig to be used by the caller.
// hostName having a value will result in the VM's hostname being changed to the given value.
func NewNodeConfig(c client.Client, clientset *kubernetes.Clientset, clusterServiceCIDR, wmcoNamespace string,
	instanceInfo *instance.Info, signer ssh.Signer, additionalLabels,
	additionalAnnotations map[string]string, platformType configv1.PlatformType) (*NodeConfig, error) {

	if err := cluster.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, fmt.Errorf("error receiving valid CIDR value for "+
			"creating new node config: %w", err)
	}

	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster DNS from service CIDR: %s: %w", clusterServiceCIDR, err)
	}

	log := ctrl.Log.WithName(fmt.Sprintf("nc %s", instanceInfo.Address))
	win, err := windows.New(clusterDNS, instanceInfo, signer, &platformType)
	if err != nil {
		return nil, fmt.Errorf("error instantiating Windows instance from VM: %w", err)
	}

	return &NodeConfig{client: c, k8sclientset: clientset, Windows: win, node: instanceInfo.Node,
		platformType: platformType, wmcoNamespace: wmcoNamespace, clusterServiceCIDR: clusterServiceCIDR,
		publicKeyHash: CreatePubKeyHashAnnotation(signer.PublicKey()), log: log, additionalLabels: additionalLabels,
		additionalAnnotations: additionalAnnotations}, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *NodeConfig) Configure(ctx context.Context) error {
	drainHelper := nc.newDrainHelper(ctx)
	// If a Node object exists already, it implies that we are reconfiguring and we should cordon the node
	if nc.node != nil {
		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}
	}

	if err := nc.createBootstrapFiles(ctx); err != nil {
		return err
	}
	if err := nc.createTLSCerts(ctx); err != nil {
		return err
	}
	if err := nc.createRegistryConfigFiles(ctx); err != nil {
		return err
	}
	if err := nc.SyncTrustedCABundle(ctx); err != nil {
		return err
	}
	wicdKC, err := nc.generateWICDKubeconfig(ctx)
	if err != nil {
		return err
	}

	wmcoVersion := version.Get()
	// Start all required services to bootstrap a node object using WICD
	if err := nc.Windows.Bootstrap(ctx, wmcoVersion, nc.wmcoNamespace, wicdKC); err != nil {
		return fmt.Errorf("bootstrapping the Windows instance failed: %w", err)
	}

	// Perform rest of the configuration with the kubelet running
	err = func() error {
		if nc.node == nil {
			// populate node object in NodeConfig in the case of a new Windows instance
			if err := nc.setNode(ctx, false); err != nil {
				return fmt.Errorf("error setting node object: %w", err)
			}
		}

		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}

		// Ensure we are labeling and annotating the node as soon as the Node object is created, so that we can identify
		// which controller should be watching it
		annotationsToApply := map[string]string{PubKeyHashAnnotation: nc.publicKeyHash}
		for key, value := range nc.additionalAnnotations {
			annotationsToApply[key] = value
		}
		if err := metadata.ApplyLabelsAndAnnotations(ctx, nc.client, *nc.node, nc.additionalLabels,
			annotationsToApply); err != nil {
			return fmt.Errorf("error updating public key hash and additional annotations on node %s: %w",
				nc.node.GetName(), err)
		}

		if err := nc.Windows.ConfigureWICD(nc.wmcoNamespace, wicdKC); err != nil {
			return fmt.Errorf("configuring WICD failed: %w", err)
		}
		// Set the desired version annotation, communicating to WICD which Windows services configmap to use
		if err := metadata.ApplyDesiredVersionAnnotation(ctx, nc.client, *nc.node, wmcoVersion); err != nil {
			return fmt.Errorf("error updating desired version annotation on node %s: %w", nc.node.GetName(), err)
		}

		// Wait for version annotation. This prevents uncordoning the node until all node services and networks are up
		if err := metadata.WaitForVersionAnnotation(ctx, nc.client, nc.node.Name); err != nil {
			return fmt.Errorf("error waiting for proper %s annotation for node %s: %w", metadata.VersionAnnotation,
				nc.node.GetName(), err)
		}

		// Now that the node has been fully configured, update the node object in NodeConfig once more
		if err := nc.setNode(ctx, false); err != nil {
			return fmt.Errorf("error getting node object: %w", err)
		}

		// Uncordon the node now that it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, false); err != nil {
			return fmt.Errorf("error uncordoning the node %s: %w", nc.node.GetName(), err)
		}

		if err := metadata.RemoveUpgradingLabel(ctx, nc.client, nc.node); err != nil {
			return fmt.Errorf("error removing upgrading label from node %s: %w", nc.node.GetName(), err)
		}

		nc.log.Info("instance has been configured as a worker node", "version",
			nc.node.Annotations[metadata.VersionAnnotation])
		return nil
	}()

	// Stop the kubelet so that the node is marked NotReady in case of an error in configuration. We are stopping all
	// the required services as they are interdependent and is safer to do so given the node is going to be NotReady.
	if err != nil {
		if err := nc.Windows.RunWICDCleanup(nc.wmcoNamespace, wicdKC); err != nil {
			nc.log.Info("Unable to mark node as NotReady", "error", err)
		}
	}
	return err
}

// safeReboot safely restarts the underlying instance, first cordoning and draining the associated node.
// Waits for reboot to take effect before uncordoning the node.
func (nc *NodeConfig) SafeReboot(ctx context.Context) error {
	if nc.node == nil {
		return fmt.Errorf("safe reboot of the instance requires an associated node")
	}

	drainer := nc.newDrainHelper(ctx)
	if err := drain.RunCordonOrUncordon(drainer, nc.node, true); err != nil {
		return fmt.Errorf("unable to cordon node %s: %w", nc.node.Name, err)
	}
	if err := drain.RunNodeDrain(drainer, nc.node.Name); err != nil {
		return fmt.Errorf("unable to drain node %s: %w", nc.node.Name, err)
	}

	if err := nc.Windows.RebootAndReinitialize(ctx); err != nil {
		return err
	}
	// Remove the reboot annotation after we can re-init an SSH connection so we know the reboot occurred successfully
	if err := metadata.RemoveRebootAnnotation(ctx, nc.client, *nc.node); err != nil {
		return err
	}

	if err := drain.RunCordonOrUncordon(drainer, nc.node, false); err != nil {
		return fmt.Errorf("unable to uncordon node %s: %w", nc.node.Name, err)
	}
	return nil
}

// getWICDServiceAccountSecret returns the secret which holds the credentials for the WICD ServiceAccount, creating one
// if necessary
func (nc *NodeConfig) getWICDServiceAccountSecret(ctx context.Context) (*core.Secret, error) {
	var tokenSecret core.Secret
	err := nc.client.Get(ctx,
		types.NamespacedName{Namespace: nc.wmcoNamespace, Name: windows.WicdServiceName}, &tokenSecret)
	if err != nil {
		if k8sapierrors.IsNotFound(err) {
			return nc.createWICDServiceAccountTokenSecret(ctx)
		}
		return nil, err
	}
	if validWICDServiceAccountTokenSecret(tokenSecret) {
		return &tokenSecret, nil
	}

	// If the secret is invalid, a new one should be created
	if err = nc.client.Delete(ctx, &tokenSecret); err != nil {
		return nil, fmt.Errorf("error deleting invalid WICD service account token secret: %w", err)
	}
	return nc.createWICDServiceAccountTokenSecret(ctx)
}

// createWICDServiceAccountTokenSecret creates a secret with a long-lived API token for the WICD ServiceAccount and
// waits for the secret data to be populated
func (nc *NodeConfig) createWICDServiceAccountTokenSecret(ctx context.Context) (*core.Secret, error) {
	err := nc.client.Create(ctx, secrets.GenerateServiceAccountTokenSecret(nc.wmcoNamespace, windows.WicdServiceName))
	if err != nil {
		return nil, fmt.Errorf("error creating secret for WICD ServiceAccount: %w", err)
	}
	secret := &core.Secret{}
	// wait for the secret data to be populated
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true,
		func(ctx context.Context) (done bool, err error) {
			secret, err = nc.k8sclientset.CoreV1().Secrets(nc.wmcoNamespace).Get(ctx, windows.WicdServiceName,
				meta.GetOptions{})
			if err != nil {
				return false, nil
			}
			caCert := secret.Data[core.ServiceAccountRootCAKey]
			if caCert == nil {
				return false, nil
			}
			token := secret.Data[core.ServiceAccountTokenKey]
			if token == nil {
				return false, nil
			}
			return true, nil
		})
	return secret, err
}

// createBootstrapFiles creates all prerequisite files on the node required to start kubelet using latest ignition spec
func (nc *NodeConfig) createBootstrapFiles(ctx context.Context) error {
	filePathsToContents := make(map[string]string)
	filePathsToContents, err := nc.createFilesFromIgnition(ctx)
	if err != nil {
		return err
	}
	filePathsToContents[windows.BootstrapKubeconfigPath], err = nc.generateBootstrapKubeconfig(ctx)
	if err != nil {
		return err
	}
	filePathsToContents[windows.KubeletConfigPath], err = createKubeletConf(nc.clusterServiceCIDR)
	if err != nil {
		return err
	}
	return nc.write(filePathsToContents)
}

// write outputs the data to the path on the underlying Windows instance for each given pair. Creates files if needed.
func (nc *NodeConfig) write(pathToData map[string]string) error {
	for path, data := range pathToData {
		dir, fileName := windows.SplitPath(path)
		if err := nc.Windows.EnsureFileContent([]byte(data), fileName, dir); err != nil {
			return err
		}
	}
	return nil
}

// createRegistryConfigFiles creates all files on the node required for containerd to mirror images
func (nc *NodeConfig) createRegistryConfigFiles(ctx context.Context) error {
	configFiles, err := registries.GenerateConfigFiles(ctx, nc.client)
	if err != nil {
		return err
	}
	return nc.Windows.ReplaceDir(configFiles, windows.ContainerdConfigDir)
}

// createFilesFromIgnition returns the contents and write locations on the instance for any file it can create from
// ignition spec: kubelet CA cert, cloud-config file
func (nc *NodeConfig) createFilesFromIgnition(ctx context.Context) (map[string]string, error) {
	ign, err := ignition.New(ctx, nc.client)
	if err != nil {
		return nil, err
	}
	kubeletArgs, err := ign.GetKubeletArgs()
	if err != nil {
		return nil, err
	}

	// create a map of 'ignition files':'desired path on a Windows instance'
	filesToTransfer := map[string]string{}
	if _, ok := kubeletArgs[ignition.CloudConfigOption]; ok {
		filesToTransfer[ignition.CloudConfigPath] = windows.K8sDir + "\\" + filepath.Base(ignition.CloudConfigPath)
	}
	filesToTransfer[ignition.ECRCredentialProviderPath] = windows.CredentialProviderConfig

	filePathsToContents, err := translateIgnitionFilesForWindows(filesToTransfer, ign.GetFiles())
	if err != nil {
		return nil, fmt.Errorf("error processing ignition files: %w", err)
	}

	filePathsToContents[windows.K8sDir+"\\"+KubeletClientCAFilename] = string(ign.GetKubeletCAData())
	return filePathsToContents, nil
}

// generateBootstrapKubeconfig returns contents of a kubeconfig for kubelet to initially communicate with the API server
func (nc *NodeConfig) generateBootstrapKubeconfig(ctx context.Context) (string, error) {
	bootstrapSecret, err := nc.k8sclientset.CoreV1().Secrets(mcoNamespace).Get(ctx, mcoBootstrapSecret,
		meta.GetOptions{})
	if err != nil {
		return "", err
	}
	return newKubeconfigFromSecret(bootstrapSecret, "kubelet")
}

// generateWICDKubeconfig returns the contents of a kubeconfig created from the WICD ServiceAccount
func (nc *NodeConfig) generateWICDKubeconfig(ctx context.Context) (string, error) {
	wicdSASecret, err := nc.getWICDServiceAccountSecret(ctx)
	if err != nil {
		return "", err
	}
	return newKubeconfigFromSecret(wicdSASecret, "wicd")
}

// newKubeconfigFromSecret returns the contents of a kubeconfig generated from the given service account token secret
func newKubeconfigFromSecret(saSecret *core.Secret, username string) (string, error) {
	// extract ca.crt and token data fields
	caCert := saSecret.Data[core.ServiceAccountRootCAKey]
	if caCert == nil {
		return "", fmt.Errorf("unable to find %s CA cert in secret %s", core.ServiceAccountRootCAKey,
			saSecret.GetName())
	}
	token := saSecret.Data[core.ServiceAccountTokenKey]
	if token == nil {
		return "", fmt.Errorf("unable to find %s token in secret %s", core.ServiceAccountTokenKey,
			saSecret.GetName())
	}
	kc := generateKubeconfig(caCert, string(token), nodeConfigCache.apiServerEndpoint,
		username)
	kubeconfigData, err := json.Marshal(kc)
	if err != nil {
		return "", err
	}
	return string(kubeconfigData), nil
}

// createKubeletConf returns contents of the config file for kubelet, with Windows specific configuration
func createKubeletConf(clusterServiceCIDR string) (string, error) {
	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return "", err
	}
	kubeletConfig := generateKubeletConfiguration(clusterDNS)
	kubeletConfigData, err := json.Marshal(kubeletConfig)
	if err != nil {
		return "", err
	}
	return string(kubeletConfigData), nil
}

// setNode finds the Node associated with the VM that has been configured, and sets the node field of the
// NodeConfig object. If quickCheck is set, the function does a quicker check for the node which is useful in the node
// reconfiguration case.
func (nc *NodeConfig) setNode(ctx context.Context, quickCheck bool) error {
	retryInterval := retry.Interval
	retryTimeout := retry.Timeout
	if quickCheck {
		retryInterval = 10 * time.Second
		retryTimeout = 30 * time.Second
	}

	instanceAddress := nc.GetIPv4Address()
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		nodes, err := nc.k8sclientset.CoreV1().Nodes().List(ctx,
			meta.ListOptions{LabelSelector: WindowsOSLabel})
		if err != nil {
			nc.log.V(1).Error(err, "node listing failed")
			return false, nil
		}
		if len(nodes.Items) == 0 {
			return false, nil
		}
		// get the node with IP address used to configure it
		if node := nodeutil.FindByAddress(instanceAddress, nodes); node != nil {
			nc.node = node
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("unable to find node with address %s: %w", instanceAddress, err)
	}
	return nil
}

// newDrainHelper returns new drain.Helper instance
func (nc *NodeConfig) newDrainHelper(ctx context.Context) *drain.Helper {
	return &drain.Helper{
		Ctx:    ctx,
		Client: nc.k8sclientset,
		ErrOut: &ErrWriter{nc.log},
		// Evict all pods regardless of their controller and orphan status
		Force: true,
		// Prevents erroring out in case a DaemonSet's pod is on the node
		IgnoreAllDaemonSets: true,
		// Prevents erroring out in case there is a workload with emptydir data
		DeleteEmptyDirData: true,
		Out:                &OutWriter{nc.log},
	}
}

// Deconfigure removes the node from the cluster, reverting changes made by the Configure function
func (nc *NodeConfig) Deconfigure(ctx context.Context) error {
	if nc.node == nil {
		return fmt.Errorf("instance does not a have an associated node to deconfigure")
	}
	nc.log.Info("deconfiguring")
	// Cordon and drain the Node before we interact with the instance
	drainHelper := nc.newDrainHelper(ctx)
	if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
		return fmt.Errorf("unable to cordon node %s: %w", nc.node.GetName(), err)
	}
	if err := drain.RunNodeDrain(drainHelper, nc.node.GetName()); err != nil {
		return fmt.Errorf("unable to drain node %s: %w", nc.node.GetName(), err)
	}

	// Revert all changes we've made to the instance by removing installed services, files, and the version annotation
	if err := nc.cleanupWithWICD(ctx); err != nil {
		return err
	}
	if err := nc.Windows.RemoveFilesAndNetworks(); err != nil {
		return fmt.Errorf("error deconfiguring instance: %w", err)
	}

	nc.log.Info("instance has been deconfigured", "node", nc.node.GetName())
	return nil
}

// cleanupWithWICD runs WICD cleanup and waits until the cleanup effects are fully complete
func (nc *NodeConfig) cleanupWithWICD(ctx context.Context) error {
	wicdKC, err := nc.generateWICDKubeconfig(ctx)
	if err != nil {
		return err
	}
	if err := nc.Windows.RunWICDCleanup(nc.wmcoNamespace, wicdKC); err != nil {
		return fmt.Errorf("unable to cleanup the Windows instance: %w", err)
	}
	// Wait for reboot annotation removal. This prevents deleting the node until the node no longer needs reboot.
	return metadata.WaitForRebootAnnotationRemoval(ctx, nc.client, nc.node.Name)
}

// UpdateKubeletClientCA updates the kubelet client CA certificate file in the Windows node. No service restart or
// reboot required, kubelet detects the changes in the file system and use the new CA certificate. The file is replaced
// if and only if it does not exist or there is a checksum mismatch.
func (nc *NodeConfig) UpdateKubeletClientCA(contents []byte) error {
	// check CA bundle contents
	if len(contents) == 0 {
		// nothing do to, return
		return nil
	}
	err := nc.Windows.EnsureFileContent(contents, KubeletClientCAFilename, windows.GetK8sDir())
	if err != nil {
		return err
	}
	return nil
}

// SyncTrustedCABundle builds the trusted CA ConfigMap from image registry certificates and the proxy trust bundle
// and ensures the cert bundle on the instance has up-to-date data
func (nc *NodeConfig) SyncTrustedCABundle(ctx context.Context) error {
	caBundle := ""
	var cc mcfg.ControllerConfig
	if err := nc.client.Get(ctx, types.NamespacedName{Namespace: nc.wmcoNamespace,
		Name: MccName}, &cc); err != nil {
		return err
	}
	for _, bundle := range cc.Spec.ImageRegistryBundleUserData {
		caBundle += appendToCABundle(bundle)
	}
	for _, bundle := range cc.Spec.ImageRegistryBundleData {
		caBundle += appendToCABundle(bundle)
	}
	if cluster.IsProxyEnabled() {
		proxyCA := &core.ConfigMap{}
		if err := nc.client.Get(ctx, types.NamespacedName{Namespace: nc.wmcoNamespace,
			Name: certificates.ProxyCertsConfigMap}, proxyCA); err != nil {
			return fmt.Errorf("unable to get ConfigMap %s: %w", certificates.ProxyCertsConfigMap, err)
		}
		caBundle += proxyCA.Data[certificates.CABundleKey]
	}
	return nc.UpdateTrustedCABundleFile(caBundle)
}

// UpdateTrustedCABundleFile updates the file containing the trusted CA bundle in the Windows node, if needed
func (nc *NodeConfig) UpdateTrustedCABundleFile(data string) error {
	dir, fileName := windows.SplitPath(windows.TrustedCABundlePath)
	return nc.Windows.EnsureFileContent([]byte(data), fileName, dir)
}

// createTLSCerts creates cert files containing the TLS cert and the key on the Windows node
func (nc *NodeConfig) createTLSCerts(ctx context.Context) error {
	tlsSecret := &core.Secret{}
	if err := nc.client.Get(ctx, types.NamespacedName{Name: secrets.TLSSecret,
		Namespace: nc.wmcoNamespace}, tlsSecret); err != nil {
		return fmt.Errorf("unable to get secret %s: %w", secrets.TLSSecret, err)
	}
	tlsData := tlsSecret.Data
	// certFiles is a map from file path on the Windows node to the file content
	certFiles := make(map[string][]byte)

	certFiles["tls.crt"] = tlsData["tls.crt"]
	certFiles["tls.key"] = tlsData["tls.key"]

	return nc.Windows.ReplaceDir(certFiles, windows.TLSCertsPath)
}

func (nc *NodeConfig) Close() error {
	return nc.Windows.Close()
}

// generateKubeconfig creates a kubeconfig spec with the certificate and token data from the given secret
func generateKubeconfig(caCert []byte, token, apiServerURL, username string) clientcmdv1.Config {
	kubeconfig := clientcmdv1.Config{
		Clusters: []clientcmdv1.NamedCluster{{
			Name: "local",
			Cluster: clientcmdv1.Cluster{
				Server:                   apiServerURL,
				CertificateAuthorityData: caCert,
			}},
		},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name: username,
			AuthInfo: clientcmdv1.AuthInfo{
				Token: token,
			},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name: username,
			Context: clientcmdv1.Context{
				Cluster:  "local",
				AuthInfo: username,
			},
		}},
		CurrentContext: username,
	}
	return kubeconfig
}

// generateKubeletConfiguration returns the configuration spec for the kubelet Windows service
func generateKubeletConfiguration(clusterDNS string) kubeletconfig.KubeletConfiguration {
	// default numeric values chosen based on the OpenShift kubelet config recommendations for Linux worker nodes
	falseBool := false
	trueBool := true
	kubeAPIQPS := int32(50)
	emptyString := ""
	return kubeletconfig.KubeletConfiguration{
		TypeMeta: meta.TypeMeta{
			Kind:       "KubeletConfiguration",
			APIVersion: "kubelet.config.k8s.io/v1beta1",
		},
		RotateCertificates: true,
		ServerTLSBootstrap: true,
		Authentication: kubeletconfig.KubeletAuthentication{
			X509: kubeletconfig.KubeletX509Authentication{
				ClientCAFile: windows.K8sDir + "\\" + KubeletClientCAFilename,
			},
			Anonymous: kubeletconfig.KubeletAnonymousAuthentication{
				Enabled: &falseBool,
			},
		},
		ClusterDomain:         "cluster.local",
		ClusterDNS:            []string{clusterDNS},
		CgroupsPerQOS:         &falseBool,
		RuntimeRequestTimeout: meta.Duration{Duration: 10 * time.Minute},
		MaxPods:               250,
		KubeAPIQPS:            &kubeAPIQPS,
		KubeAPIBurst:          100,
		SerializeImagePulls:   &falseBool,
		EnableSystemLogQuery:  &trueBool,
		FeatureGates: map[string]bool{
			"RotateKubeletServerCertificate": true,
		},
		ContainerLogMaxSize: "50Mi",
		EnforceNodeAllocatable: []string{
			"none",
		},
		// hard-eviction is not yet fully supported on Windows, but the values passed are subtracted from Capacity
		// to calculate the node allocatable. The recommendation is to explicitly set the supported signals available
		// for Windows.
		// See https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#eviction-signals
		EvictionHard: map[string]string{
			"nodefs.available":  "10%",
			"imagefs.available": "15%",
			// containerfs.available works for Windows, but does not support overrides for thresholds
		},
		SystemReserved: map[string]string{
			string(core.ResourceCPU):              "500m",
			string(core.ResourceEphemeralStorage): "1Gi",
			// Reserve at least 2GiB of memory
			// See https://kubernetes.io/docs/concepts/configuration/windows-resource-management/#resource-reservation
			string(core.ResourceMemory): "2Gi",
		},
		ContainerRuntimeEndpoint: "npipe://./pipe/containerd-containerd",
		// Registers the Kubelet with Windows specific taints so that linux pods won't get scheduled onto
		// Windows nodes. Explicitly set RegisterNode to ensure RegisterWithTaints takes effect.
		RegisterNode: &trueBool,
		RegisterWithTaints: []core.Taint{
			{Key: "os", Value: "Windows", Effect: core.TaintEffectNoSchedule},
		},
		// Set to empty string to override the default. Network configuration in Windows is stored in the
		// registry database rather than files like in Linux.
		ResolverConfig: &emptyString,
	}
}

// translateIgnitionFilesForWindows returns a mapping of Windows file paths and contents, as specified by the given
// ignition file entries. The argument ignToWindowsPaths should be a mapping of the ignition files the caller is
// interested in, and the desired path for the file on Windows instances.
func translateIgnitionFilesForWindows(ignToWindowsPaths map[string]string, ignitionFiles []ignCfgTypes.File) (map[string]string, error) {
	filePathsToContents := make(map[string]string)
	for _, ignFile := range ignitionFiles {
		if dest, ok := ignToWindowsPaths[ignFile.Node.Path]; ok {
			if ignFile.Contents.Source == nil {
				return nil, fmt.Errorf("could not process %s: File is empty", ignFile.Node.Path)
			}
			contents, err := dataurl.DecodeString(*ignFile.Contents.Source)
			if err != nil {
				return nil, fmt.Errorf("could not decode %s: %w", ignFile.Node.Path, err)
			}
			// Special casing for the ECRCredentialProviderConfig, as the contents needs to be modified for Windows
			if ignFile.Node.Path == ignition.ECRCredentialProviderPath {
				contents.Data, err = modifyCredentialProviderConfig(contents.Data)
				if err != nil {
					return nil, err
				}
			}
			filePathsToContents[dest] = string(contents.Data)
		}
	}
	return filePathsToContents, nil
}

// modifyCredentialProviderConfig takes the contents of a CredentialProviderConfig yaml file, and returns one which
// points to '*.exe' files, instead of binaries without extensions. This is needed for the referenced files to be
// properly run on Windows.
func modifyCredentialProviderConfig(fileContents []byte) ([]byte, error) {
	providerConf := kubeletconfigv1.CredentialProviderConfig{}
	err := yaml.Unmarshal(fileContents, &providerConf)
	if err != nil {
		return []byte{}, fmt.Errorf("could not unmarshal provider config: %w", err)
	}
	for i := range providerConf.Providers {
		if !strings.HasSuffix(providerConf.Providers[i].Name, ".exe") {
			providerConf.Providers[i].Name += ".exe"
		}
	}
	fileContents, err = yaml.Marshal(&providerConf)
	if err != nil {
		return []byte{}, fmt.Errorf("error marshalling provider config: %w", err)
	}
	return fileContents, nil
}

// CreatePubKeyHashAnnotation returns a formatted string which can be used for a public key annotation on a node.
// The annotation is the sha256 of the public key
func CreatePubKeyHashAnnotation(key ssh.PublicKey) string {
	pubKey := string(ssh.MarshalAuthorizedKey(key))
	trimmedKey := strings.TrimSuffix(pubKey, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmedKey)))
}

// appendToCABundle returns a formatted string containing CA bundle's file name and data, the output is appended to an
// existing CA bundle string
func appendToCABundle(bundle mcfg.ImageRegistryBundle) string {
	return fmt.Sprintf("# %s\n%s\n\n", strings.ReplaceAll(bundle.File, "..", ":"), bundle.Data)
}

// validWICDServiceAccountTokenSecret returns true if the given secret provides a token for the WICD SA
func validWICDServiceAccountTokenSecret(secret core.Secret) bool {
	if secret.Type != core.SecretTypeServiceAccountToken {
		return false
	}
	if secret.Annotations[core.ServiceAccountNameKey] != windows.WicdServiceName {
		return false
	}
	return true
}
