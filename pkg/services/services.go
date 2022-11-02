package services

import (
	"fmt"
	"path/filepath"
	"strings"

	config "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"

	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	// See doc link for explanation of log levels:
	// https://docs.openshift.com/container-platform/latest/rest_api/editing-kubelet-log-level-verbosity.html#log-verbosity-descriptions_editing-kubelet-log-level-verbosity
	debugLogLevel    = "4"
	standardLogLevel = "2"
	// ec2HostnameVar is a variable that should be replaced with the value of `local-hostname` in the ec2 instance
	// metadata
	ec2HostnameVar = "EC2_HOSTNAME"
	NodeIPVar      = "NODE_IP"
)

// GenerateManifest returns the expected state of the Windows service configmap. If debug is true, debug logging
// will be enabled for services that support it.
func GenerateManifest(kubeletArgsFromIgnition map[string]string, vxlanPort string, platform config.PlatformType,
	ccmEnabled, debug bool) (*servicescm.Data, error) {
	kubeletConfiguration, err := getKubeletServiceConfiguration(kubeletArgsFromIgnition, debug, platform)
	if err != nil {
		return nil, errors.Wrap(err, "could not determine kubelet service configuration spec")
	}
	services := &[]servicescm.Service{{
		Name:                         windows.WindowsExporterServiceName,
		Command:                      windows.WindowsExporterServiceCommand,
		NodeVariablesInCommand:       nil,
		PowershellVariablesInCommand: nil,
		Dependencies:                 nil,
		Bootstrap:                    false,
		Priority:                     1,
	},
		kubeletConfiguration,
		hybridOverlayConfiguration(vxlanPort, debug),
		kubeProxyConfiguration(debug),
	}
	if platform == config.AzurePlatformType && ccmEnabled {
		*services = append(*services, azureCloudNodeManagerConfiguration())
	}
	// TODO: All payload filenames and checksums must be added here https://issues.redhat.com/browse/WINC-847
	files := &[]servicescm.FileInfo{}
	return servicescm.NewData(services, files)
}

// azureCloudNodeManagerConfiguration returns the service specification for azure-cloud-node-manager.exe
func azureCloudNodeManagerConfiguration() servicescm.Service {
	cmd := fmt.Sprintf("%s --windows-service --node-name=NODE_NAME --wait-routes=false --kubeconfig=%s",
		windows.AzureCloudNodeManagerPath, windows.KubeconfigPath)

	return servicescm.Service{
		Name:    windows.AzureCloudNodeManagerServiceName,
		Command: cmd,
		NodeVariablesInCommand: []servicescm.NodeCmdArg{{
			Name:               "NODE_NAME",
			NodeObjectJsonPath: "{.metadata.name}",
		}},
		PowershellVariablesInCommand: nil,
		Dependencies:                 nil,
		Bootstrap:                    false,
		Priority:                     2,
	}
}

// hybridOverlayConfiguration returns the Service definition for hybrid-overlay
func hybridOverlayConfiguration(vxlanPort string, debug bool) servicescm.Service {
	hybridOverlayServiceCmd := fmt.Sprintf("%s --node NODE_NAME --k8s-kubeconfig %s --windows-service "+
		"--logfile "+"%shybrid-overlay.log", windows.HybridOverlayPath, windows.KubeconfigPath,
		windows.HybridOverlayLogDir)
	if len(vxlanPort) > 0 {
		hybridOverlayServiceCmd = fmt.Sprintf("%s --hybrid-overlay-vxlan-port %s", hybridOverlayServiceCmd, vxlanPort)
	}

	// check log level and increase hybrid-overlay verbosity if needed
	if debug {
		// append loglevel param using 5 for debug (default: 4)
		// See https://github.com/openshift/ovn-kubernetes/blob/master/go-controller/pkg/config/config.go#L736
		hybridOverlayServiceCmd = hybridOverlayServiceCmd + " --loglevel 5"
	}
	return servicescm.Service{
		Name:    windows.HybridOverlayServiceName,
		Command: hybridOverlayServiceCmd,
		NodeVariablesInCommand: []servicescm.NodeCmdArg{
			{
				Name:               "NODE_NAME",
				NodeObjectJsonPath: "{.metadata.name}",
			},
		},
		PowershellVariablesInCommand: nil,
		Dependencies:                 []string{windows.KubeletServiceName},
		Bootstrap:                    false,
		Priority:                     1,
	}
}

// kubeProxyConfiguration returns the Service definition for kube-proxy
func kubeProxyConfiguration(debug bool) servicescm.Service {
	sanitizedSubnetAnnotation := strings.ReplaceAll(nodeconfig.HybridOverlaySubnet, ".", "\\.")
	cmd := fmt.Sprintf("%s --windows-service --proxy-mode=kernelspace --feature-gates=WinOverlay=true "+
		"--hostname-override=NODE_NAME --kubeconfig=%s --cluster-cidr=NODE_SUBNET --log-dir=%s --logtostderr=false "+
		"--network-name=%s --source-vip=ENDPOINT_IP --enable-dsr=false", windows.KubeProxyPath, windows.KubeconfigPath,
		windows.KubeProxyLogDir, windows.OVNKubeOverlayNetwork)
	// Set log level
	cmd = fmt.Sprintf("%s %s", cmd, klogVerbosityArg(debug))
	return servicescm.Service{
		Name:    windows.KubeProxyServiceName,
		Command: cmd,
		NodeVariablesInCommand: []servicescm.NodeCmdArg{
			{
				Name:               "NODE_NAME",
				NodeObjectJsonPath: "{.metadata.name}",
			},
			{
				Name:               "NODE_SUBNET",
				NodeObjectJsonPath: fmt.Sprintf("{.metadata.annotations.%s}", sanitizedSubnetAnnotation),
			},
		},
		PowershellVariablesInCommand: []servicescm.PowershellCmdArg{{
			Name: "ENDPOINT_IP",
			Path: windows.NetworkConfScriptPath,
		}},
		Dependencies: []string{windows.HybridOverlayServiceName},
		Bootstrap:    false,
		Priority:     2,
	}
}

// getKubeletServiceConfiguration returns the Service definition for the kubelet
func getKubeletServiceConfiguration(argsFromIginition map[string]string, debug bool,
	platform config.PlatformType) (servicescm.Service, error) {
	kubeletArgs, err := generateKubeletArgs(argsFromIginition, debug)
	if err != nil {
		return servicescm.Service{}, err
	}
	var powershellVars []servicescm.PowershellCmdArg
	hostnameArg := getKubeletHostnameOverride(platform)
	if hostnameArg != "" {
		kubeletArgs = append(kubeletArgs, hostnameArg)
		powershellVars = append(powershellVars, servicescm.PowershellCmdArg{
			Name: ec2HostnameVar,
			Path: "Get-EC2InstanceMetadata -Category LocalHostname",
		})
	}

	kubeletServiceCmd := windows.KubeletPath
	for _, arg := range kubeletArgs {
		kubeletServiceCmd += fmt.Sprintf(" %s", arg)
	}
	if platform == config.NonePlatformType {
		// special case substitution handled in WICD itself
		kubeletServiceCmd = fmt.Sprintf("%s --node-ip=%s", kubeletServiceCmd, NodeIPVar)
	}
	return servicescm.Service{
		Name:                         windows.KubeletServiceName,
		Command:                      kubeletServiceCmd,
		Priority:                     0,
		Bootstrap:                    true,
		Dependencies:                 []string{windows.ContainerdServiceName},
		PowershellVariablesInCommand: powershellVars,
		NodeVariablesInCommand:       nil,
	}, nil
}

// generateKubeletArgs returns the kubelet args required during initial kubelet start up
func generateKubeletArgs(argsFromIgnition map[string]string, debug bool) ([]string, error) {
	containerdEndpointValue := "npipe://./pipe/containerd-containerd"
	certDirectory := "c:\\var\\lib\\kubelet\\pki\\"
	windowsTaints := "os=Windows:NoSchedule"
	kubeletArgs := []string{
		"--config=" + windows.KubeletConfigPath,
		"--bootstrap-kubeconfig=" + windows.BootstrapKubeconfig,
		"--kubeconfig=" + windows.KubeconfigPath,
		"--cert-dir=" + certDirectory,
		"--windows-service",
		"--logtostderr=false",
		"--log-file=" + windows.KubeletLog,
		// Registers the Kubelet with Windows specific taints so that linux pods won't get scheduled onto
		// Windows nodes.
		"--register-with-taints=" + windowsTaints,
		"--node-labels=" + nodeconfig.WindowsOSLabel,
		"--container-runtime=remote",
		"--container-runtime-endpoint=" + containerdEndpointValue,
		"--resolv-conf=",
	}

	kubeletArgs = append(kubeletArgs, klogVerbosityArg(debug))
	if cloudProvider, ok := argsFromIgnition[ignition.CloudProviderOption]; ok {
		kubeletArgs = append(kubeletArgs, fmt.Sprintf("--%s=%s", ignition.CloudProviderOption, cloudProvider))
	}
	if cloudConfigValue, ok := argsFromIgnition[ignition.CloudConfigOption]; ok {
		// cloud config is placed by WMCB in the c:\k directory with the same file name
		cloudConfigPath := windows.K8sDir + filepath.Base(cloudConfigValue)
		kubeletArgs = append(kubeletArgs, fmt.Sprintf("--%s=%s", ignition.CloudConfigOption, cloudConfigPath))
	}

	return kubeletArgs, nil
}

// klogVerbosityArg returns an argument to set the verbosity for any service that uses klog to log
func klogVerbosityArg(debug bool) string {
	if debug {
		return "--v=" + debugLogLevel
	} else {
		return "--v=" + standardLogLevel
	}
}

// getKubeletHostnameOverride returns the hostname override arg that should be used
func getKubeletHostnameOverride(platformType config.PlatformType) string {
	switch platformType {
	case config.AWSPlatformType:
		return "--hostname-override=" + ec2HostnameVar
	default:
		return ""
	}
}
