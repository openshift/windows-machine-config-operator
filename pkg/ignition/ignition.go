package ignition

import (
	"context"
	"regexp"
	"strings"

	ignCfg "github.com/coreos/ignition/v2/config/v3_2"
	ignCfgTypes "github.com/coreos/ignition/v2/config/v3_2/types"
	mcfg "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//+kubebuilder:rbac:groups="machineconfiguration.openshift.io",resources=machineconfigs,verbs=list;watch

const (
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
)

// Data holds the fully contents of an ignition file
type Data struct {
	ignCfgTypes.Config
	// KubeletUnit is the kubelet systemd service spec in ignition file
	KubeletUnit ignCfgTypes.Unit
}

// Initialize populates the RenderedWorker global variable with the worker ignition file data.
func Initialize(c client.Client) error {
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
	return nil
}
