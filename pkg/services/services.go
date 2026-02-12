package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	config "github.com/openshift/api/config/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

const (
	// See doc link for explanation of log levels:\
	// https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/api_overview/editing-kubelet-log-level-verbosity#log-verbosity-descriptions_editing-kubelet-log-level-verbosity
	debugLogLevel    = "4"
	standardLogLevel = "2"
	// hostnameOverrideVar is the variable that should be replaced with the value of the desired instance hostname
	hostnameOverrideVar = "HOSTNAME_OVERRIDE"
	NodeIPVar           = "NODE_IP"

	// logFileSizeEnvVar is the environment variable name for log file size limit
	logFileSizeEnvVar = "SERVICES_LOG_FILE_SIZE"
	// logFileAgeEnvVar is the environment variable name for log file age retention
	logFileAgeEnvVar = "SERVICES_LOG_FILE_AGE"
	// logFlushIntervalEnvVar is the environment variable name for log flush interval
	logFlushIntervalEnvVar = "SERVICES_LOG_FLUSH_INTERVAL"
	// defaultLogFileSize is the default log file size limit before rotation (100MB)
	defaultLogFileSize = "100M"
	// defaultLogFileAge is the default log file age retention (7 days)
	defaultLogFileAge = "168h"
	// defaultFlushInterval is the default interval for flushing logs to disk
	defaultFlushInterval = "5s"
)

// GenerateManifest returns the expected state of the Windows service configmap. If debug is true, debug logging
// will be enabled for services that support it.
func GenerateManifest(kubeletArgsFromIgnition map[string]string, apiServerEndpoint string, vxlanPort string,
	platform config.PlatformType, debug bool) (*servicescm.Data, error) {
	windowsExporterServiceCommand := fmt.Sprintf("%s --collectors.enabled "+
		"cpu,cs,logical_disk,net,os,service,system,container,memory,cpu_info --web.config.file %s",
		windows.WindowsExporterPath, windows.TLSConfPath)
	kubeletConfiguration, err := getKubeletServiceConfiguration(kubeletArgsFromIgnition, debug, platform)
	if err != nil {
		return nil, fmt.Errorf("could not determine kubelet service configuration spec: %w", err)
	}
	services := &[]servicescm.Service{{
		Name:                   windows.WindowsExporterServiceName,
		Command:                windowsExporterServiceCommand,
		NodeVariablesInCommand: nil,
		PowershellPreScripts:   nil,
		Dependencies:           nil,
		Bootstrap:              false,
		Priority:               2,
	},
		containerdConfiguration(debug),
		kubeletConfiguration,
		hybridOverlayConfiguration(apiServerEndpoint, vxlanPort, debug),
		kubeProxyConfiguration(debug),
		csiProxyConfiguration(debug),
	}
	if platform == config.AzurePlatformType {
		*services = append(*services, azureCloudNodeManagerConfiguration())
	}
	// TODO: All payload filenames and checksums must be added here https://issues.redhat.com/browse/WINC-847
	files := &[]servicescm.FileInfo{}
	var watchedEnvVars []string
	for _, envVar := range cluster.WatchedEnvironmentVars {
		watchedEnvVars = append(watchedEnvVars, envVar)
	}
	return servicescm.NewData(services, files, cluster.GetProxyVars(), watchedEnvVars)
}

// containerdConfiguration returns the service specification for the Windows containerd service
func containerdConfiguration(debug bool) servicescm.Service {
	containerdServiceCmd := fmt.Sprintf("%s --config %s --log-file %s --run-service",
		windows.ContainerdPath, windows.ContainerdConfPath, windows.ContainerdLogPath)
	if debug {
		containerdServiceCmd = containerdServiceCmd + " --log-level debug"
	} else {
		containerdServiceCmd = containerdServiceCmd + " --log-level info"
	}
	return servicescm.Service{
		Name:                   windows.ContainerdServiceName,
		Command:                containerdServiceCmd,
		NodeVariablesInCommand: nil,
		PowershellPreScripts: []servicescm.PowershellPreScript{{
			Path: fmt.Sprintf("%s -BinPath %s", windows.WinDefenderExclusionScriptRemotePath, windows.ContainerdPath),
		}},
		Dependencies: nil,
		Bootstrap:    true,
		Priority:     0,
	}
}

// azureCloudNodeManagerConfiguration returns the service specification for azure-cloud-node-manager.exe
func azureCloudNodeManagerConfiguration() servicescm.Service {
	cmd := fmt.Sprintf("%s --windows-service --node-name=NODE_NAME --wait-routes=false --kubeconfig=%s",
		windows.AzureCloudNodeManagerPath, windows.WICDKubeconfigPath)

	return servicescm.Service{
		Name:    windows.AzureCloudNodeManagerServiceName,
		Command: cmd,
		NodeVariablesInCommand: []servicescm.NodeCmdArg{{
			Name:               "NODE_NAME",
			NodeObjectJsonPath: "{.metadata.name}",
		}},
		PowershellPreScripts: nil,
		Dependencies:         nil,
		Bootstrap:            false,
		Priority:             3,
	}
}

// hybridOverlayConfiguration returns the Service definition for hybrid-overlay
func hybridOverlayConfiguration(apiServerEndpoint, vxlanPort string, debug bool) servicescm.Service {
	hybridOverlayServiceCmd := fmt.Sprintf("%s --node NODE_NAME --bootstrap-kubeconfig=%s --cert-dir=%s --cert-duration=24h "+
		"--windows-service --logfile "+"%s\\hybrid-overlay.log", windows.HybridOverlayPath, windows.KubeconfigPath, windows.CniConfDir,
		windows.HybridOverlayLogDir)

	// append k8s apiserver option pointing to the api server URL
	hybridOverlayServiceCmd = fmt.Sprintf("%s --k8s-apiserver=%s", hybridOverlayServiceCmd, apiServerEndpoint)

	// append cacert option pointing to the trusted CA bundle path
	hybridOverlayServiceCmd = fmt.Sprintf("%s --k8s-cacert %s", hybridOverlayServiceCmd, windows.BootstrapCaCertPath)

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
		PowershellPreScripts: nil,
		Dependencies:         []string{windows.KubeletServiceName},
		Bootstrap:            false,
		Priority:             2,
	}
}

// kubeProxyConfiguration returns the Service definition for kube-proxy
func kubeProxyConfiguration(debug bool) servicescm.Service {
	cmd := fmt.Sprintf("%s -log-file=%s %s --config %s --windows-service", windows.KubeLogRunnerPath, windows.KubeProxyLog,
		windows.KubeProxyPath, windows.KubeProxyConfigPath)

	verbosity := "0"
	if debug {
		verbosity = "4"
	}
	sanitizedSubnetAnnotation := strings.ReplaceAll(nodeconfig.HybridOverlaySubnet, ".", "\\.")
	return servicescm.Service{
		Name:         windows.KubeProxyServiceName,
		Command:      cmd,
		Dependencies: []string{windows.HybridOverlayServiceName},
		PowershellPreScripts: []servicescm.PowershellPreScript{{
			Path: windows.NetworkConfScriptPath + " -hostnameOverride NODE_NAME -clusterCIDR NODE_SUBNET -kubeConfigPath KUBE_CONFIG_PATH -kubeProxyConfigPath KUBE_PROXY_CONFIG_PATH -verbosity VERBOSITY",
			NodeArgs: []servicescm.NodeCmdArg{
				{
					Name:               "NODE_NAME",
					NodeObjectJsonPath: "{.metadata.name}",
				},
				{
					Name:               "NODE_SUBNET",
					NodeObjectJsonPath: fmt.Sprintf("{.metadata.annotations.%s}", sanitizedSubnetAnnotation),
				},
				{
					Name:               "KUBE_CONFIG_PATH",
					NodeObjectJsonPath: windows.KubeconfigPath,
				},
				{
					Name:               "KUBE_PROXY_CONFIG_PATH",
					NodeObjectJsonPath: windows.KubeProxyConfigPath,
				},
				{
					Name:               "VERBOSITY",
					NodeObjectJsonPath: verbosity,
				},
			},
		}},
		Bootstrap: false,
		Priority:  3,
	}
}

// csiProxyConfiguration returns the Service definition for csi-proxy
func csiProxyConfiguration(debug bool) servicescm.Service {
	serviceCmd := fmt.Sprintf("%s -log_file=%s -logtostderr=false -windows-service", windows.CSIProxyPath,
		windows.CSIProxyLog)
	// Set log level
	serviceCmd = fmt.Sprintf("%s %s", serviceCmd, klogVerbosityArg(debug))
	return servicescm.Service{
		Name:                   "csi-proxy",
		Command:                serviceCmd,
		NodeVariablesInCommand: nil,
		PowershellPreScripts:   nil,
		Dependencies:           nil,
		Bootstrap:              false,
		Priority:               2,
	}
}

// getKubeletServiceConfiguration returns the Service definition for the kubelet
func getKubeletServiceConfiguration(argsFromIginition map[string]string, debug bool,
	platform config.PlatformType) (servicescm.Service, error) {
	kubeletArgs, err := generateKubeletArgs(argsFromIginition, debug)
	if err != nil {
		return servicescm.Service{}, err
	}
	var preScripts []servicescm.PowershellPreScript

	hostnameOverrideCmd := getHostnameCmd(platform)
	if hostnameOverrideCmd != "" {
		hostnameOverrideArg := "--hostname-override=" + hostnameOverrideVar
		hostnameOverridePowershellVar := servicescm.PowershellPreScript{
			VariableName: hostnameOverrideVar,
			Path:         hostnameOverrideCmd,
		}
		kubeletArgs = append(kubeletArgs, hostnameOverrideArg)
		preScripts = append(preScripts, hostnameOverridePowershellVar)
	}

	kubeletServiceCmd := getLogRunnerForCmd(windows.KubeletPath, windows.KubeletLog)

	for _, arg := range kubeletArgs {
		kubeletServiceCmd += fmt.Sprintf(" %s", arg)
	}

	// explicitly set node ip and resolves to the first IPv4 address of the default gateway
	kubeletServiceCmd = fmt.Sprintf("%s --node-ip=%s", kubeletServiceCmd, NodeIPVar)
	if platform == config.AWSPlatformType {
		kubeletServiceCmd = fmt.Sprintf("%s --image-credential-provider-bin-dir=%s --image-credential-provider-config=%s",
			kubeletServiceCmd, windows.K8sDir, windows.CredentialProviderConfig)
	}
	preScripts = append(preScripts, servicescm.PowershellPreScript{
		VariableName: NodeIPVar,
		Path: "(Get-NetRoute -DestinationPrefix '0.0.0.0/0' | " +
			"Get-NetIpAddress -AddressFamily IPv4 -ifIndex {$_.ifIndex}[0]).IPAddress",
	})
	return servicescm.Service{
		Name:                   windows.KubeletServiceName,
		Command:                kubeletServiceCmd,
		Priority:               1,
		Bootstrap:              true,
		Dependencies:           []string{windows.ContainerdServiceName},
		PowershellPreScripts:   preScripts,
		NodeVariablesInCommand: nil,
	}, nil
}

// generateKubeletArgs returns the kubelet args required during initial kubelet start up
func generateKubeletArgs(argsFromIgnition map[string]string, debug bool) ([]string, error) {
	certDirectory := "c:\\var\\lib\\kubelet\\pki\\"
	windowsPriorityClass := "ABOVE_NORMAL_PRIORITY_CLASS"
	// TODO: Removal of deprecated flags to be done in https://issues.redhat.com/browse/WINC-924
	kubeletArgs := []string{
		"--config=" + windows.KubeletConfigPath,
		"--bootstrap-kubeconfig=" + windows.BootstrapKubeconfigPath,
		"--kubeconfig=" + windows.KubeconfigPath,
		"--cert-dir=" + certDirectory,
		"--windows-service",
		"--node-labels=" + nodeconfig.WindowsOSLabel,
		// Allows the kubelet process to get more CPU time slices when compared to other processes running on the
		// Windows host.
		// See: https://kubernetes.io/docs/concepts/configuration/windows-resource-management/#resource-management-cpu
		"--windows-priorityclass=" + windowsPriorityClass,
	}

	kubeletArgs = append(kubeletArgs, klogVerbosityArg(debug))
	if cloudProvider, ok := argsFromIgnition[ignition.CloudProviderOption]; ok {
		kubeletArgs = append(kubeletArgs, fmt.Sprintf("--%s=%s", ignition.CloudProviderOption, cloudProvider))
	}
	if cloudConfigValue, ok := argsFromIgnition[ignition.CloudConfigOption]; ok {
		// cloud config is placed by WMCO in the c:\k directory with the same file name
		cloudConfigPath := windows.K8sDir + "\\" + filepath.Base(cloudConfigValue)
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

// getHostnameCmd returns the hostname override command for the given platform as needed
func getHostnameCmd(platformType config.PlatformType) string {
	switch platformType {
	case config.AWSPlatformType:
		// Use the Instance Metadata Service Version 1 (IMDSv1) to fetch the hostname. IMDSv1 will continue to be
		// supported indefinitely as per AWS docs. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
		return "Invoke-RestMethod -UseBasicParsing -Uri http://169.254.169.254/latest/meta-data/local-hostname"
	case config.GCPPlatformType:
		return windows.GcpGetHostnameScriptRemotePath
	case config.VSpherePlatformType:
		return windows.GetHostnameFQDNCommand
	default:
		// by default do not override the hostname, the cloud provider determines the name of the node
		return ""
	}
}

// getLogRunnerForCmd returns the command string to run the given commandPath with kube-log-runner
// logging to the given logfilePath. Log rotation parameters can be configured via environment variables.
func getLogRunnerForCmd(commandPath, logfilePath string) string {
	cmdBuilder := strings.Builder{}
	// log runner path must be first
	cmdBuilder.WriteString(windows.KubeLogRunnerPath)

	// add log file option
	cmdBuilder.WriteString(" -log-file=")
	cmdBuilder.WriteString(logfilePath)

	// log file size limit before creating a backup
	cmdBuilder.WriteString(" -log-file-size=")
	cmdBuilder.WriteString(logFileSize)

	// log retention for backup files created after the size limit is reached
	cmdBuilder.WriteString(" -log-file-age=")
	cmdBuilder.WriteString(logFileAge)

	// explicit flush to ensure recent log entries are written to disk in near real-time
	cmdBuilder.WriteString(" -flush-interval=")
	cmdBuilder.WriteString(flushInterval)

	// last, add the target command to be run
	cmdBuilder.WriteString(" " + commandPath)

	return cmdBuilder.String()
}

// getEnvQuantityOrDefault returns the value of the environment variable with the given key
// if it represents a valid quantity, otherwise returns the default value
func getEnvQuantityOrDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	value = strings.TrimSpace(value)
	if value != "" {
		if _, err := resource.ParseQuantity(value); err == nil {
			return value
		}
	}
	return defaultValue
}

// getEnvDurationOrDefault returns the value of the environment variable with the given key
// if it represents a valid duration, otherwise returns the default value
func getEnvDurationOrDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	value = strings.TrimSpace(value)
	if value != "" {
		if _, err := time.ParseDuration(value); err == nil {
			return value
		}
	}
	return defaultValue
}
