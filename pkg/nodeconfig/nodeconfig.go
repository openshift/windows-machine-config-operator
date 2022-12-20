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
	"github.com/pkg/errors"
	"github.com/vincent-petithory/dataurl"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
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

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/metadata"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeutil"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
	"github.com/openshift/windows-machine-config-operator/version"
)

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
	// DesiredVersionAnnotation is a Node annotation, indicating the Service ConfigMap that should be used to configure it
	DesiredVersionAnnotation = "windowsmachineconfig.openshift.io/desired-version"
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
func NewNodeConfig(c client.Client, clientset *kubernetes.Clientset, clusterServiceCIDR, vxlanPort, wmcoNamespace string,
	instanceInfo *instance.Info, signer ssh.Signer, additionalLabels,
	additionalAnnotations map[string]string, platformType configv1.PlatformType) (*nodeConfig, error) {
	var err error

	if err = cluster.ValidateCIDR(clusterServiceCIDR); err != nil {
		return nil, errors.Wrap(err, "error receiving valid CIDR value for "+
			"creating new node config")
	}

	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return nil, errors.Wrapf(err, "error getting cluster DNS from service CIDR: %s", clusterServiceCIDR)
	}

	log := ctrl.Log.WithName(fmt.Sprintf("nc %s", instanceInfo.Address))
	win, err := windows.New(clusterDNS, vxlanPort, instanceInfo, signer)
	if err != nil {
		return nil, errors.Wrap(err, "error instantiating Windows instance from VM")
	}

	return &nodeConfig{client: c, k8sclientset: clientset, Windows: win, platformType: platformType,
		wmcoNamespace: wmcoNamespace, clusterServiceCIDR: clusterServiceCIDR,
		publicKeyHash: CreatePubKeyHashAnnotation(signer.PublicKey()), log: log, additionalLabels: additionalLabels,
		additionalAnnotations: additionalAnnotations}, nil
}

// Configure configures the Windows VM to make it a Windows worker node
func (nc *nodeConfig) Configure() error {
	drainHelper := nc.newDrainHelper()
	// If we find a node  it implies that we are reconfiguring and we should cordon the node
	if err := nc.setNode(true); err == nil {
		// Make a best effort to cordon the node until it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
			nc.log.Info("unable to cordon", "node", nc.node.GetName(), "error", err)
		}
	}

	if err := nc.createBootstrapFiles(); err != nil {
		return err
	}

	// Start all required services to bootstrap a node object using WICD
	if err := nc.Windows.Bootstrap(version.Get(), nodeConfigCache.apiServerEndpoint, nc.wmcoNamespace,
		nodeConfigCache.credentials); err != nil {
		return errors.Wrap(err, "bootstrapping the Windows instance failed")
	}

	// Perform rest of the configuration with the kubelet running
	err := func() error {
		// populate node object in nodeConfig in the case of a new Windows instance
		if err := nc.setNode(false); err != nil {
			return errors.Wrap(err, "error getting node object")
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
		if err := nc.applyLabelsAndAnnotations(nc.additionalLabels, annotationsToApply); err != nil {
			return errors.Wrapf(err, "error updating public key hash and additional annotations on node %s",
				nc.node.GetName())
		}

		ownedByCCM, err := isCloudControllerOwnedByCCM()
		if err != nil {
			return errors.Wrap(err, "unable to check if cloud controller owned by cloud controller manager")
		}

		if err := nc.Windows.ConfigureWICD(nodeConfigCache.apiServerEndpoint, nc.wmcoNamespace,
			nodeConfigCache.credentials); err != nil {
			return errors.Wrap(err, "configuring WICD failed")
		}
		// Set the desired version annotation, communicating to WICD which Windows services configmap to use
		if err := nc.applyLabelsAndAnnotations(nil, map[string]string{DesiredVersionAnnotation: version.Get()}); err != nil {
			return errors.Wrapf(err, "error updating desired version annotation on node %s", nc.node.GetName())
		}

		// Now that basic kubelet configuration is complete, configure networking in the node
		if err := nc.configureNetwork(); err != nil {
			return errors.Wrap(err, "configuring node network failed")
		}

		// Now that the node has been fully configured, add the version annotation to signify that the node
		// was successfully configured by this version of WMCO
		// populate node object in nodeConfig once more
		if err := nc.setNode(false); err != nil {
			return errors.Wrap(err, "error getting node object")
		}

		// If we deploy on Azure with CCM support, we have to explicitly remove the cloud taint, because cloud node
		// manager running on the node can't do it itself, due to lack of RBAC permissions given by the node
		// kubeconfig it uses.
		if ownedByCCM && nc.platformType == configv1.AzurePlatformType {
			// TODO: The proper long term solution is to run this as a pod and give it the correct permissions
			// via service account. This isn't currently possible as we are unable to build Windows container images
			// due to shortcomings in our build system. Short term solution is to do this taint removal in WICD, when
			// WICD removes the cordon. https://issues.redhat.com/browse/WINC-741
			cloudTaint := &core.Taint{
				Key:    cloudproviderapi.TaintExternalCloudProvider,
				Effect: core.TaintEffectNoSchedule,
			}
			if err := cloudnodeutil.RemoveTaintOffNode(nc.k8sclientset, nc.node.GetName(), nc.node, cloudTaint); err != nil {
				return errors.Wrapf(err, "error excluding cloud taint on node %s", nc.node.GetName())
			}
		}
		// Version annotation is the indicator that the node was fully configured by this version of WMCO, so it should
		// be added at the end of the process.
		if err := nc.applyLabelsAndAnnotations(nil, map[string]string{metadata.VersionAnnotation: version.Get()}); err != nil {
			return errors.Wrapf(err, "error updating version annotation on node %s", nc.node.GetName())
		}

		// Uncordon the node now that it is fully configured
		if err := drain.RunCordonOrUncordon(drainHelper, nc.node, false); err != nil {
			return errors.Wrapf(err, "error uncordoning the node %s", nc.node.GetName())
		}

		nc.log.Info("instance has been configured as a worker node", "version",
			nc.node.Annotations[metadata.VersionAnnotation])
		return nil
	}()

	// Stop the kubelet so that the node is marked NotReady in case of an error in configuration. We are stopping all
	// the required services as they are interdependent and is safer to do so given the node is going to be NotReady.
	if err != nil {
		if err := nc.Windows.EnsureRequiredServicesStopped(); err != nil {
			nc.log.Info("Unable to mark node as NotReady", "error", err)
		}
	}
	return err
}

// createBootstrapFiles creates all prerequisite files on the node required to start kubelet using latest ignition spec
func (nc *nodeConfig) createBootstrapFiles() error {
	filePathsToContents := make(map[string]string)
	filePathsToContents, err := nc.createFilesFromIgnition()
	if err != nil {
		return err
	}
	bootstrapSecret, err := nc.k8sclientset.CoreV1().Secrets(mcoNamespace).Get(context.TODO(), mcoBootstrapSecret,
		meta.GetOptions{})
	if err != nil {
		return err
	}
	filePathsToContents[windows.BootstrapKubeconfigPath], err = createBootstrapKubeconfig(bootstrapSecret)
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

	filesToTransfer := map[string]struct{}{
		ignition.KubeletCACertPath: {},
	}
	if _, ok := kubeletArgs[ignition.CloudConfigOption]; ok {
		filesToTransfer[ignition.CloudConfigPath] = struct{}{}
	}
	filePathsToContents := make(map[string]string)
	// For each new file in the ignition file check if is a file we are interested in and, if so, decode its contents
	for _, ignFile := range ign.GetFiles() {
		if _, ok := filesToTransfer[ignFile.Node.Path]; ok {
			if ignFile.Contents.Source == nil {
				return nil, errors.Errorf("could not process %s: File is empty", ignFile.Node.Path)
			}
			contents, err := dataurl.DecodeString(*ignFile.Contents.Source)
			if err != nil {
				return nil, errors.Wrapf(err, "could not decode %s", ignFile.Node.Path)
			}
			fileName := filepath.Base(ignFile.Node.Path)
			filePathsToContents[windows.K8sDir+fileName] = string(contents.Data)
		}
	}
	return filePathsToContents, nil
}

// createBootstrapKubeconfig returns contents of a kubeconfig for kubelet to initially communicate with the API server
func createBootstrapKubeconfig(bootstrapSecret *core.Secret) (string, error) {
	// extract ca.crt and token data fields
	caCert := bootstrapSecret.Data[core.ServiceAccountRootCAKey]
	if caCert == nil {
		return "", errors.Errorf("unable to find %s CA cert in secret %s", core.ServiceAccountRootCAKey,
			bootstrapSecret.GetName())
	}
	token := bootstrapSecret.Data[core.ServiceAccountTokenKey]
	if token == nil {
		return "", errors.Errorf("unable to find %s token in secret %s", core.ServiceAccountTokenKey,
			bootstrapSecret.GetName())
	}
	kubeconfig, err := generateKubeconfig(&windows.Authentication{CaCert: caCert, Token: token},
		nodeConfigCache.apiServerEndpoint)
	if err != nil {
		return "", err
	}
	kubeconfigData, err := json.Marshal(kubeconfig)
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

// applyLabelsAndAnnotations applies all the given labels and annotations and updates the Node object in NodeConfig
func (nc *nodeConfig) applyLabelsAndAnnotations(labels, annotations map[string]string) error {
	patchData, err := metadata.GenerateAddPatch(labels, annotations)
	if err != nil {
		return err
	}
	node, err := nc.k8sclientset.CoreV1().Nodes().Patch(context.TODO(), nc.node.GetName(), kubeTypes.JSONPatchType,
		patchData, meta.PatchOptions{})
	if err != nil {
		return errors.Wrapf(err, "unable to apply patch data %s", patchData)
	}
	nc.node = node
	return nil
}

// isCloudControllerOwnedByCCM checks if Cloud Controllers are managed by Cloud Controller Manager (CCM)
// instead of Kube Controller Manager.
func isCloudControllerOwnedByCCM() (bool, error) {
	cfg, err := crclientcfg.GetConfig()
	if err != nil {
		return false, errors.Wrap(err, "unable to get config to talk to kubernetes api server")
	}

	client, err := clientset.NewForConfig(cfg)
	if err != nil {
		return false, errors.Wrap(err, "unable to get client from the given config")
	}

	return cluster.IsCloudControllerOwnedByCCM(client)
}

// configureNetwork configures k8s networking in the node
// we are assuming that the WindowsVM and node objects are valid
func (nc *nodeConfig) configureNetwork() error {
	// Wait until the node object has the hybrid overlay subnet annotation. Otherwise the hybrid-overlay will fail to
	// start
	if err := nc.waitForNodeAnnotation(HybridOverlaySubnet); err != nil {
		return errors.Wrapf(err, "error waiting for %s node annotation for %s", HybridOverlaySubnet,
			nc.node.GetName())
	}

	// Wait until the node object has the hybrid overlay MAC annotation. This indicates that hybrid-overlay is running
	// successfully, and is required for the CNI configuration to start.
	if err := nc.waitForNodeAnnotation(HybridOverlayMac); err != nil {
		return errors.Wrapf(err, "error waiting for %s node annotation for %s", HybridOverlayMac,
			nc.node.GetName())
	}
	// Running the hybrid-overlay causes network reconfiguration in the Windows VM which results in the ssh connection
	// being closed, and the client is not smart enough to reconnect.
	if err := nc.Windows.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing VM after running hybrid-overlay")
	}

	// Start the kube-proxy service
	if err := nc.Windows.ConfigureKubeProxy(); err != nil {
		return errors.Wrapf(err, "error starting kube-proxy for %s", nc.node.GetName())
	}
	return nil
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
	err := wait.Poll(retryInterval, retryTimeout, func() (bool, error) {
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
	return errors.Wrapf(err, "unable to find node with address %s", instanceAddress)
}

// waitForNodeAnnotation checks if the node object has the given annotation and waits for retry.Interval seconds and
// returns an error if the annotation does not appear in that time frame.
func (nc *nodeConfig) waitForNodeAnnotation(annotation string) error {
	nodeName := nc.node.GetName()
	var found bool
	err := wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		node, err := nc.k8sclientset.CoreV1().Nodes().Get(context.TODO(), nodeName, meta.GetOptions{})
		if err != nil {
			nc.log.V(1).Error(err, "unable to get associated node object")
			return false, nil
		}
		_, found := node.Annotations[annotation]
		if found {
			// update node to avoid staleness
			nc.node = node
			return true, nil
		}
		return false, nil
	})

	if !found {
		return errors.Wrapf(err, "timeout waiting for %s node annotation", annotation)
	}
	return nil
}

// newDrainHelper returns new drain.Helper instance
func (nc *nodeConfig) newDrainHelper() *drain.Helper {
	return &drain.Helper{
		Ctx:    context.TODO(),
		Client: nc.k8sclientset,
		ErrOut: &ErrWriter{nc.log},
		Out:    &OutWriter{nc.log},
	}
}

// Deconfigure removes the node from the cluster, reverting changes made by the Configure function
func (nc *nodeConfig) Deconfigure() error {
	// Set nc.node to the existing node
	if err := nc.setNode(true); err != nil {
		return err
	}

	// Cordon and drain the Node before we interact with the instance
	drainHelper := nc.newDrainHelper()
	if err := drain.RunCordonOrUncordon(drainHelper, nc.node, true); err != nil {
		return errors.Wrapf(err, "unable to cordon node %s", nc.node.GetName())
	}
	if err := drain.RunNodeDrain(drainHelper, nc.node.GetName()); err != nil {
		return errors.Wrapf(err, "unable to drain node %s", nc.node.GetName())
	}

	// Revert the changes we've made to the instance by removing services and deleting all installed files
	if err := nc.Windows.Deconfigure(); err != nil {
		return errors.Wrap(err, "error deconfiguring instance")
	}

	// Clear the version annotation from the node object to indicate the node is not configured
	patchData, err := metadata.GenerateRemovePatch([]string{}, []string{metadata.VersionAnnotation})
	if err != nil {
		return errors.Wrapf(err, "error creating version annotation remove request")
	}
	_, err = nc.k8sclientset.CoreV1().Nodes().Patch(context.TODO(), nc.node.GetName(), kubeTypes.JSONPatchType,
		patchData, meta.PatchOptions{})
	if err != nil {
		return errors.Wrapf(err, "error removing version annotation from node %s", nc.node.GetName())
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

// generateKubeconfig creates a kubeconfig spec with the certificate and token data from the given secret
func generateKubeconfig(secret *windows.Authentication, apiServerURL string) (clientcmdv1.Config, error) {
	kubeconfig := clientcmdv1.Config{
		Clusters: []clientcmdv1.NamedCluster{{
			Name: "local",
			Cluster: clientcmdv1.Cluster{
				Server:                   apiServerURL,
				CertificateAuthorityData: secret.CaCert,
			}},
		},
		AuthInfos: []clientcmdv1.NamedAuthInfo{{
			Name: "kubelet",
			AuthInfo: clientcmdv1.AuthInfo{
				Token: string(secret.Token),
			},
		}},
		Contexts: []clientcmdv1.NamedContext{{
			Name: "kubelet",
			Context: clientcmdv1.Context{
				Cluster:  "local",
				AuthInfo: "kubelet",
			},
		}},
		CurrentContext: "kubelet",
	}
	return kubeconfig, nil
}

// generateKubeletConfiguration returns the configuration spec for the kubelet Windows service
func generateKubeletConfiguration(clusterDNS string) kubeletconfig.KubeletConfiguration {
	// default numeric values chosen based on the OpenShift kubelet config recommendations for Linux worker nodes
	falseBool := false
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
				ClientCAFile: windows.K8sDir + KubeletClientCAFilename,
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
		FeatureGates: map[string]bool{
			"LegacyNodeRoleBehavior":         false,
			"NodeDisruptionExclusion":        true,
			"RotateKubeletServerCertificate": true,
			"SCTPSupport":                    true,
			"ServiceNodeExclusion":           true,
			"SupportPodPidsLimit":            true,
		},
		ContainerLogMaxSize: "50Mi",
		SystemReserved: map[string]string{
			"cpu":               "500m",
			"ephemeral-storage": "1Gi",
			"memory":            "1Gi",
		},
	}
}

// CreatePubKeyHashAnnotation returns a formatted string which can be used for a public key annotation on a node.
// The annotation is the sha256 of the public key
func CreatePubKeyHashAnnotation(key ssh.PublicKey) string {
	pubKey := string(ssh.MarshalAuthorizedKey(key))
	trimmedKey := strings.TrimSuffix(pubKey, "\n")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(trimmedKey)))
}
