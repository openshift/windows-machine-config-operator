package ignition

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	ignCfg "github.com/coreos/ignition/v2/config/v3_2"
	ignCfgTypes "github.com/coreos/ignition/v2/config/v3_2/types"
	mcfg "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/pkg/errors"
	"github.com/vincent-petithory/dataurl"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//+kubebuilder:rbac:groups="machineconfiguration.openshift.io",resources=machineconfigs,verbs=list;watch

const (
	// k8sDir is the remote kubernetes executable directory
	k8sDir = "C:\\k\\"
	// kubeletSystemdName is the name of the systemd service that the kubelet runs under,
	// this is used to parse the kubelet args
	kubeletSystemdName = "kubelet.service"
	// CertDirectory is where the kubelet will look for certificates
	CertDirectory = "c:\\var\\lib\\kubelet\\pki\\"
	// CloudConfigOption is kubelet CLI option for cloud configuration
	CloudConfigOption = "cloud-config"
	// WindowsTaints defines the taints that need to be applied on the Windows nodes.
	WindowsTaints = "os=Windows:NoSchedule"
	// ContainerdEndpointValue is the default value for containerd endpoint required to be updated in kubelet arguments
	ContainerdEndpointValue = "npipe://./pipe/containerd-containerd"
)

// These are global, so that we only need to compile them once
var (
	// CloudProviderRegex searches for the cloud provider option given to the kubelet
	CloudProviderRegex = regexp.MustCompile(`--cloud-provider=(\w*)`)
	// CloudConfigRegex searches for the cloud config option given to kubelet. We assume the file has a conf extension
	CloudConfigRegex = regexp.MustCompile(`--` + CloudConfigOption + `=(\/.*conf)`)
	// VerbosityRegex searches for the verbosity option given to the kubelet
	VerbosityRegex = regexp.MustCompile(`--v=(\w*)`)
	// RenderedWorker holds the contents of the cluster's worker node ignition file
	RenderedWorker Data
	//go:embed templates/kubelet_config.json
	baseConfig string
)

// Data holds the fully contents of an ignition file
type Data struct {
	ignCfgTypes.Config
	// KubeletUnit is the kubelet systemd service spec in ignition file
	KubeletUnit ignCfgTypes.Unit
	// BootstrapFiles holds paths to all the files required to start the kubelet service
	BootstrapFiles []string
}

// kubeletConf holds the values to populate needed fields in the the base kubelet config file
type kubeletConf struct {
	// ClientCAFile specifies location to client certificate
	ClientCAFile string
	// ClusterDNS is the IP address of the DNS server used for all containers
	ClusterDNS string
}

// Initialize populates the RenderedWorker global variable with the worker ignition file data. It also generates the
// bootstrap files in the payload, which are needed to eventually start kubelet on the VM
func Initialize(c client.Client, clusterServiceCIDR string) error {
	log := ctrl.Log.WithName("ignition")

	mcList := &mcfg.MachineConfigList{}
	err := c.List(context.TODO(), mcList)
	if err != nil {
		return err
	}
	renderedWorkerPrefix := "rendered-worker-"
	var rawIgnition []byte
	for _, mc := range mcList.Items {
		if strings.HasPrefix(mc.Name, renderedWorkerPrefix) {
			rawIgnition = mc.Spec.Config.Raw
			log.Info("Received ignition spec", "MachineConfig", mc.Name)
			break
		}
	}

	configuration, report, err := ignCfg.Parse(rawIgnition)
	if err != nil || report.IsFatal() {
		return errors.Errorf("failed to parse Ign spec config: %v\nReport: %v", err, report)
	}
	RenderedWorker.Config = configuration
	log.Info("parsed", "version", RenderedWorker.Config.Ignition.Version)

	// Find the kubelet systemd service specified in the ignition file and grab the variable arguments
	// TODO: Refactor this to handle environment variables in argument values
	for _, unit := range configuration.Systemd.Units {
		if unit.Name == kubeletSystemdName {
			RenderedWorker.KubeletUnit = unit
			break
		}
	}
	if RenderedWorker.KubeletUnit.Contents == nil {
		return errors.Errorf("ignition missing kubelet systemd unit file")
	}
	log.Info("Extracted", "kubelet systemd unit", RenderedWorker.KubeletUnit)
	RenderedWorker.BootstrapFiles, err = setupBootstrapFiles(clusterServiceCIDR)
	return err
}

// setupBootstrapFiles creates all prerequisite files required to start kubelet and returns their paths
func setupBootstrapFiles(clusterServiceCIDR string) ([]string, error) {
	log := ctrl.Log.WithName("setupBootstrapFiles")
	log.Info("in setupBootstrapFiles")
	bootstrapFiles, err := createFilesFromIgnition()
	if err != nil {
		return nil, err
	}
	log.Info("executed GetFilesFromIgnition", "files", bootstrapFiles)

	clusterDNS, err := cluster.GetDNS(clusterServiceCIDR)
	if err != nil {
		return nil, err
	}
	initKubeletConfigPath, err := createKubeletConf(clusterDNS)
	if err != nil {
		return nil, err
	}
	log.Info("created kubelet.conf", "path", initKubeletConfigPath)
	bootstrapFiles = append(bootstrapFiles, initKubeletConfigPath)
	log.Info("added bootstrap files", "files to transfer", bootstrapFiles)
	return bootstrapFiles, nil
}

// createFilesFromIgnition creates files any it can from ignition: bootstrap kubeconfig, cloud-config, kubelet cert
func createFilesFromIgnition() ([]string, error) {
	// For each new file in the ignition file check if is a file we are interested in, if so, decode it
	// and write the contents to a temporary destination path
	filesToTranslate := map[string]struct{}{
		payload.BootstrapKubeconfig: {},
		payload.KubeletCACert:       {},
	}
	results := CloudConfigRegex.FindStringSubmatch(*RenderedWorker.KubeletUnit.Contents)
	if len(results) == 2 {
		filesToTranslate[payload.CloudConfigPath] = struct{}{}
	}

	if _, err := os.Stat(payload.GeneratedDir); errors.Is(err, os.ErrNotExist) {
		err := os.Mkdir(payload.GeneratedDir, os.ModePerm)
		if err != nil {
			return nil, err
		}
	}
	filesFromIgnition := []string{}
	for _, ignFile := range RenderedWorker.Config.Storage.Files {
		if _, ok := filesToTranslate[ignFile.Node.Path]; ok {
			if ignFile.Contents.Source == nil {
				return nil, errors.Errorf("could not process %s: File is empty", ignFile.Node.Path)
			}
			contents, err := dataurl.DecodeString(*ignFile.Contents.Source)
			if err != nil {
				return nil, errors.Wrapf(err, "could not decode %s", ignFile.Node.Path)
			}
			newContents := contents.Data
			dest := filepath.Join(payload.GeneratedDir, filepath.Base(ignFile.Node.Path))
			if err = os.WriteFile(dest, newContents, 0644); err != nil {
				return nil, fmt.Errorf("could not write to %s: %s", dest, err)
			}
			filesFromIgnition = append(filesFromIgnition, dest)
		}
	}
	return filesFromIgnition, nil
}

// createKubeletConf creates config file for kubelet, with Windows specific configuration
// Add values in kubelet_config.json files, for additional static fields.
// Add fields in kubeletConf struct for variable fields
func createKubeletConf(clusterDNS string) (string, error) {
	log := ctrl.Log.WithName("createKubeletConf")
	kubeletConfTmpl := template.New("kubeletconf")
	// Parse the template
	kubeletConfTmpl, err := kubeletConfTmpl.Parse(baseConfig)
	if err != nil {
		return "", err
	}
	// Fill up the config file, using kubeletConf struct
	variableFields := kubeletConf{
		ClientCAFile: strings.Join(append(strings.Split(k8sDir, `\`), `kubelet-ca.crt`), `\\`),
	}
	// check clusterDNS
	if clusterDNS != "" {
		// surround with double-quotes for valid JSON format
		variableFields.ClusterDNS = "\"" + clusterDNS + "\""
	}
	// Create kubelet.conf file
	if _, err := os.Stat(payload.GeneratedDir); errors.Is(err, os.ErrNotExist) {
		err := os.Mkdir(payload.GeneratedDir, os.ModePerm)
		if err != nil {
			return "", err
		}
	}
	kubeletConfFile, err := os.Create(payload.KubeletConfigPath)
	defer kubeletConfFile.Close()
	if err != nil {
		return "", fmt.Errorf("error creating %s: %v", payload.KubeletConfigPath, err)
	}
	log.Info("created", "file", payload.KubeletConfigPath)
	err = kubeletConfTmpl.Execute(kubeletConfFile, variableFields)
	if err != nil {
		return "", fmt.Errorf("error writing data to %v file: %v", payload.KubeletConfigPath, err)
	}
	return kubeletConfFile.Name(), nil
}
