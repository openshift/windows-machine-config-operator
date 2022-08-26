package services

import (
	"fmt"
	"strings"

	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

// GenerateManifest returns the expected state of the Windows service configmap. If debug is true, debug logging
// will be enabled for services that support it.
func GenerateManifest(vxlanPort string, debug bool) (*servicescm.Data, error) {
	services := &[]servicescm.Service{{
		Name:                         windows.WindowsExporterServiceName,
		Command:                      windows.WindowsExporterServiceCommand,
		NodeVariablesInCommand:       nil,
		PowershellVariablesInCommand: nil,
		Dependencies:                 nil,
		Bootstrap:                    false,
		Priority:                     1,
	},
		hybridOverlayConfiguration(vxlanPort, debug),
		kubeProxyConfiguration(debug),
	}
	// TODO: All payload filenames and checksums must be added here https://issues.redhat.com/browse/WINC-847
	files := &[]servicescm.FileInfo{}
	return servicescm.NewData(services, files)
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
	// Set log level. See doc link for explanation of log levels:
	// https://docs.openshift.com/container-platform/latest/rest_api/editing-kubelet-log-level-verbosity.html#log-verbosity-descriptions_editing-kubelet-log-level-verbosity
	if debug {
		cmd = fmt.Sprintf("%s --v=4", cmd)
	} else {
		cmd = fmt.Sprintf("%s --v=2", cmd)
	}
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
