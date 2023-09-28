package nodeconfig

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	configv1 "github.com/openshift/api/config/v1"
	clientset "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/vincent-petithory/dataurl"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	cloudproviderapi "k8s.io/cloud-provider/api"
	cloudnodeutil "k8s.io/cloud-provider/node/helpers"
	"k8s.io/kubectl/pkg/drain"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crclientcfg "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/openshift/windows-machine-config-operator/pkg/certificates"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
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
)

// nodeConfig holds the information to make the given VM a kubernetes node. As of now, it holds the information
// related to kubeclient and the windowsVM.
type nodeConfig struct {
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

// NewNodeConfig creates a new instance of nodeConfig to be used by the caller.
// hostName having a value will result in the VM's hostname being changed to the given value.
func NewNodeConfig(c client.Client, clientset *kubernetes.Clientset, clusterServiceCIDR, wmcoNamespace string,
	instanceInfo *instance.Info, signer ssh.Signer, additionalLabels,
	additionalAnnotations map[string]string, platformType configv1.PlatformType) (*nodeConfig, error) {

	if err := cluster.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, fmt.Errorf("error receiving valid CIDR value for "+
			"creating new node config: %w", err)
	}

	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return nil, fmt.Errorf("error getting cluster DNS from service CIDR: %s: %w", clusterServiceCIDR, err)
	}

	log := ctrl.Log.WithName(fmt.Sprintf("nc %s", instanceInfo.Address))
	win, err := windows.New(clusterDNS, instanceInfo, signer)
	if err != nil {
		return nil, fmt.Errorf("error instantiating Windows instance from VM: %w", err)
	}

	return &nodeConfig{client: c, k8sclientset: clientset, Windows: win, node: instanceInfo.Node,
		platformType: platformType, wmcoNamespace: wmcoNamespace, clusterServiceCIDR: clusterServiceCIDR,
		publicKeyHash: CreatePubKeyHashAnnotation(signer.PublicKey()), log: log, additionalLabels: additionalLabels,
		additionalAnnotations: additionalAnnotations}, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	drainHelper := nc.newDrainHelper()
	// If a Node object exists already, it implies that we are reconfiguring and we should cordon the node
	if nc.node != nil {
		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}
	}

	if err := nc.createBootstrapFiles(); err != nil {
		return err
	}
	if cluster.IsProxyEnabled() {
		if err := nc.ensureTrustedCABundle(); err != nil {
			return err
		}
	}
	wicdKC, err := nc.generateWICDKubeconfig()
	if err != nil {
		return err
	}

	wmcoVersion := version.Get()
	// Start all required services to bootstrap a node object using WICD
	if err := nc.Windows.Bootstrap(wmcoVersion, nc.wmcoNamespace, wicdKC); err != nil {
		return fmt.Errorf("bootstrapping the Windows instance failed: %w", err)
	}

	// Perform rest of the configuration with the kubelet running
	err = func() error {
		if nc.node == nil {
			// populate node object in nodeConfig in the case of a new Windows instance
			if err := nc.setNode(false); err != nil {
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
		if err := metadata.ApplyLabelsAndAnnotations(context.TODO(), nc.client, *nc.node, nc.additionalLabels,
			annotationsToApply); err != nil {
			return fmt.Errorf("error updating public key hash and additional annotations on node %s: %w",
				nc.node.GetName(), err)
		}

		ownedByCCM, err := isCloudControllerOwnedByCCM()
		if err != nil {
			return fmt.Errorf("unable to check if cloud controller owned by cloud controller manager: %w", err)
		}

		if err := nc.Windows.ConfigureWICD(nc.wmcoNamespace, wicdKC); err != nil {
			return fmt.Errorf("configuring WICD failed: %w", err)
		}
		// Set the desired version annotation, communicating to WICD which Windows services configmap to use
		if err := metadata.ApplyDesiredVersionAnnotation(context.TODO(), nc.client, *nc.node, wmcoVersion); err != nil {
			return fmt.Errorf("error updating desired version annotation on node %s: %w", nc.node.GetName(), err)
		}

		// Wait for version annotation. This prevents uncordoning the node until all node services and networks are up
		if err := metadata.WaitForVersionAnnotation(context.TODO(), nc.client, nc.node.Name); err != nil {
			return fmt.Errorf("error waiting for proper %s annotation for node %s: %w", metadata.VersionAnnotation,
				nc.node.GetName(), err)
		}

		// Now that the node has been fully configured, update the node object in nodeConfig once more
		if err := nc.setNode(false); err != nil {
			return fmt.Errorf("error getting node object: %w", err)
		}

		// If we deploy on Azure with CCM support, we have to explicitly remove the cloud taint, because cloud node
		// manager running on the node can't do it itself, due to lack of RBAC permissions given by the node
		// kubeconfig it uses.
		if ownedByCCM && nc.platformType == configv1.AzurePlatformType {
			// TODO: The proper long term solution is to run this as a pod and give it the correct permissions
			// via service account. This isn't currently possible as we are unable to build Windows container images
			// due to shortcomings in our build system.
			cloudTaint := &core.Taint{
				Key:    cloudproviderapi.TaintExternalCloudProvider,
				Effect: core.TaintEffectNoSchedule,
			}
			if err := cloudnodeutil.RemoveTaintOffNode(nc.k8sclientset, nc.node.GetName(), nc.node, cloudTaint); err != nil {
				return fmt.Errorf("error excluding cloud taint on node %s: %w", nc.node.GetName(), err)
			}
		}

		// Uncordon the node now that it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, false); err != nil {
			return fmt.Errorf("error uncordoning the node %s: %w", nc.node.GetName(), err)
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
func (nc *nodeConfig) SafeReboot(ctx context.Context) error {
	if nc.node == nil {
		return fmt.Errorf("safe reboot of the instance requires an associated node")
	}

	drainer := nc.newDrainHelper()
	if err := drain.RunCordonOrUncordon(drainer, nc.node, true); err != nil {
		return fmt.Errorf("unable to cordon node %s: %w", nc.node.Name, err)
	}
	if err := drain.RunNodeDrain(drainer, nc.node.Name); err != nil {
		return fmt.Errorf("unable to drain node %s: %w", nc.node.Name, err)
	}

	if err := nc.Windows.RebootAndReinitialize(); err != nil {
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

// getWICDServiceAccountSecret returns the secret which holds the credentials for the WICD ServiceAccount
func (nc *nodeConfig) getWICDServiceAccountSecret() (*core.Secret, error) {
	var secrets core.SecretList
	err := nc.client.List(context.TODO(), &secrets, client.InNamespace(nc.wmcoNamespace))
	if err != nil {
		return nil, err
	}
	// Go through all the secrets in the WMCO namespace, and find the token secret which contains the auth credentials
	// for the WICD ServiceAccount. This secret's name will always have the form:
	// ${service_account_name}-token-${random_string}.
	tokenSecretPrefix := "windows-instance-config-daemon-token-"
	var filteredSecrets []core.Secret
	for _, secret := range secrets.Items {
		if strings.HasPrefix(secret.Name, tokenSecretPrefix) {
			filteredSecrets = append(filteredSecrets, secret)
		}
	}
	if len(filteredSecrets) != 1 {
		return nil, fmt.Errorf("expected 1 secret with '%s' prefix, found %d", tokenSecretPrefix, len(filteredSecrets))
	}
	return &filteredSecrets[0], nil

}

// createBootstrapFiles creates all prerequisite files on the node required to start kubelet using latest ignition spec
func (nc *nodeConfig) createBootstrapFiles() error {
	filePathsToContents := make(map[string]string)
	filePathsToContents, err := nc.createFilesFromIgnition()
	if err != nil {
		return err
	}
	filePathsToContents[windows.BootstrapKubeconfigPath], err = nc.generateBootstrapKubeconfig()
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
func (nc *nodeConfig) write(pathToData map[string]string) error {
	for path, data := range pathToData {
		dir, fileName := windows.SplitPath(path)
		if err := nc.Windows.EnsureFileContent([]byte(data), fileName, dir); err != nil {
			return err
		}
	}
	return nil
}

// createFilesFromIgnition returns the contents and write locations on the instance for any file it can create from
// ignition spec: kubelet CA cert, cloud-config file
func (nc *nodeConfig) createFilesFromIgnition() (map[string]string, error) {
	ign, err := ignition.New(nc.client)
	if err != nil {
		return nil, err
	}
	kubeletArgs, err := ign.GetKubeletArgs()
	if err != nil {
		return nil, err
	}

	filesToTransfer := map[string]struct{}{}
	if _, ok := kubeletArgs[ignition.CloudConfigOption]; ok {
		filesToTransfer[ignition.CloudConfigPath] = struct{}{}
	}
	filePathsToContents := make(map[string]string)
	// process kubelet-ca
	filePathsToContents[windows.K8sDir+"\\"+KubeletClientCAFilename] = string(ign.GetKubeletCAData())
	// loop through all the files in the ignition if they are files to transfer
	if len(filesToTransfer) == 0 {
		return filePathsToContents, nil
	}
	// For each new file in the ignition file check if is a file we are interested in and, if so, decode its contents
	for _, ignFile := range ign.GetFiles() {
		if _, ok := filesToTransfer[ignFile.Node.Path]; ok {
			if ignFile.Contents.Source == nil {
				return nil, fmt.Errorf("could not process %s: File is empty", ignFile.Node.Path)
			}
			contents, err := dataurl.DecodeString(*ignFile.Contents.Source)
			if err != nil {
				return nil, fmt.Errorf("could not decode %s: %w", ignFile.Node.Path, err)
			}
			fileName := filepath.Base(ignFile.Node.Path)
			filePathsToContents[windows.K8sDir+"\\"+fileName] = string(contents.Data)
		}
	}
	return filePathsToContents, nil
}

// generateBootstrapKubeconfig returns contents of a kubeconfig for kubelet to initially communicate with the API server
func (nc *nodeConfig) generateBootstrapKubeconfig() (string, error) {
	bootstrapSecret, err := nc.k8sclientset.CoreV1().Secrets(mcoNamespace).Get(context.TODO(), mcoBootstrapSecret,
		meta.GetOptions{})
	if err != nil {
		return "", err
	}
	return newKubeconfigFromSecret(bootstrapSecret, "kubelet")
}

// generateWICDKubeconfig returns the contents of a kubeconfig created from the WICD ServiceAccount
func (nc *nodeConfig) generateWICDKubeconfig() (string, error) {
	wicdSASecret, err := nc.getWICDServiceAccountSecret()
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
	// Replace last character ('}') with comma
	kubeletConfigData[len(kubeletConfigData)-1] = ','
	// Appending this option is needed here instead of in the kubelet configuration object. Otherwise, when marshalling,
	// the empty value will be omitted, so it would end up being incorrectly populated at service start time.
	// Can be moved to kubelet configuration object with https://issues.redhat.com/browse/WINC-926
	enforceNodeAllocatable := []byte("\"enforceNodeAllocatable\":[]}")
	kubeletConfigData = append(kubeletConfigData, enforceNodeAllocatable...)

	return string(kubeletConfigData), nil
}

// isCloudControllerOwnedByCCM checks if Cloud Controllers are managed by Cloud Controller Manager (CCM)
// instead of Kube Controller Manager.
func isCloudControllerOwnedByCCM() (bool, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return false, fmt.Errorf("unable to get config to talk to kubernetes api server: %w", err)
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return false, fmt.Errorf("unable to get client from the given config: %w", err)
	}

	return cluster.IsCloudControllerOwnedByCCM(client)
}

// setNode finds the Node associated with the VM that has been configured, and sets the node field of the
// nodeConfig object. If quickCheck is set, the function does a quicker check for the node which is useful in the node
// reconfiguration case.
func (nc *nodeConfig) setNode(quickCheck bool) error {
	retryInterval := retry.Interval
	retryTimeout := retry.Timeout
	if quickCheck {
		retryInterval = 10 * time.Second
		retryTimeout = 30 * time.Second
	}

	instanceAddress := nc.GetIPv4Address()
	err := wait.PollImmediate(retryInterval, retryTimeout, func() (bool, error) {
		nodes, err := nc.k8sclientset.CoreV1().Nodes().List(context.TODO(),
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
func (nc *nodeConfig) newDrainHelper() *drain.Helper {
	return &drain.Helper{
		Ctx:    context.TODO(),
		Client: nc.k8sclientset,
		ErrOut: &ErrWriter{nc.log},
		// Evict all pods regardless of their controller and orphan status
		Force: true,
		// Prevents erroring out in case a DaemonSet's pod is on the node
		IgnoreAllDaemonSets: true,
		Out:                 &OutWriter{nc.log},
	}
}

// Deconfigure removes the node from the cluster, reverting changes made by the Configure function
func (nc *nodeConfig) Deconfigure() error {
	if nc.node == nil {
		return fmt.Errorf("instance does not a have an associated node to deconfigure")
	}
	// Cordon and drain the Node before we interact with the instance
	drainHelper := nc.newDrainHelper()
	if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
		return fmt.Errorf("unable to cordon node %s: %w", nc.node.GetName(), err)
	}
	if err := drain.RunNodeDrain(drainHelper, nc.node.GetName()); err != nil {
		return fmt.Errorf("unable to drain node %s: %w", nc.node.GetName(), err)
	}

	// Revert all changes we've made to the instance by removing installed services, files, and the version annotation
	wicdKC, err := nc.generateWICDKubeconfig()
	if err != nil {
		return err
	}
	if err := nc.Windows.Deconfigure(nc.wmcoNamespace, wicdKC); err != nil {
		return fmt.Errorf("error deconfiguring instance: %w", err)
	}
	// Wait for reboot annotation removal. This prevents deleting the node until the node no longer needs reboot.
	if err := metadata.WaitForRebootAnnotationRemoval(context.TODO(), nc.client, nc.node.Name); err != nil {
		return err
	}
	nc.log.Info("instance has been deconfigured", "node", nc.node.GetName())
	return nil
}

// UpdateKubeletClientCA updates the kubelet client CA certificate file in the Windows node. No service restart or
// reboot required, kubelet detects the changes in the file system and use the new CA certificate. The file is replaced
// if and only if it does not exist or there is a checksum mismatch.
func (nc *nodeConfig) UpdateKubeletClientCA(contents []byte) error {
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

// ensureTrustedCABundle gets the trusted CA ConfigMap and ensures the cert bundle on the instance has up-to-date data
func (nc *nodeConfig) ensureTrustedCABundle() error {
	trustedCA := &core.ConfigMap{}
	if err := nc.client.Get(context.TODO(), types.NamespacedName{Namespace: nc.wmcoNamespace,
		Name: certificates.ProxyCertsConfigMap}, trustedCA); err != nil {
		return fmt.Errorf("unable to get ConfigMap %s: %w", certificates.ProxyCertsConfigMap, err)
	}
	return nc.UpdateTrustedCABundleFile(trustedCA.Data)
}

// UpdateTrustedCABundleFile updates the file containing the trusted CA bundle in the Windows node, if needed
func (nc *nodeConfig) UpdateTrustedCABundleFile(data map[string]string) error {
	dir, fileName := windows.SplitPath(windows.TrustedCABundlePath)
	return nc.Windows.EnsureFileContent([]byte(data[certificates.CABundleKey]), fileName, dir)
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
		SystemReserved: map[string]string{
			"cpu":               "500m",
			"ephemeral-storage": "1Gi",
			"memory":            "1Gi",
		},
		ContainerRuntimeEndpoint: "npipe://./pipe/containerd-containerd",
	}
}

// CreatePubKeyHashAnnotation returns a formatted string which can be used for a public key annotation on a node.
// The annotation is the sha256 of the public key
func CreatePubKeyHashAnnotation(key ssh.PublicKey) string {
	pubKey := string(ssh.MarshalAuthorizedKey(key))
	trimmedKey := strings.TrimSuffix(pubKey, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmedKey)))
}
