package services

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	ignCfgTypes "github.com/coreos/ignition/v2/config/v3_2/types"
	config "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift/windows-machine-config-operator/pkg/ignition"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig"
	"github.com/openshift/windows-machine-config-operator/pkg/servicescm"
	"github.com/openshift/windows-machine-config-operator/pkg/windows"
)

// GenerateManifest returns the expected state of the Windows service configmap. If debug is true, debug logging
// will be enabled for services that support it.
func GenerateManifest(clusterDNS, vxlanPort, platform string, debug bool) (*servicescm.Data, error) {
	kubeletConfiguration, err := getKubeletConfiguration(clusterDNS, platform)
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
		hybridOverlayConfiguration(vxlanPort, debug),
		*kubeletConfiguration,
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

// getKubeletConfiguration returns the Service definition for the kubelet
func getKubeletConfiguration(clusterDNS, platform string) (*servicescm.Service, error) {
	log := ctrl.Log.WithName("services.getKubeletConfiguration()")

	kubeletArgs, err := gatherKubeletArgs(platform)
	if err != nil {
		return nil, err
	}
	kubeletServiceCmd := windows.KubeletPath
	for _, arg := range kubeletArgs {
		kubeletServiceCmd += fmt.Sprintf(" %s", arg)
	}

	log.Info("kubelet service args", "cmd", kubeletServiceCmd)

	return &servicescm.Service{
		Name:                         windows.KubeletServiceName,
		Command:                      kubeletServiceCmd,
		Priority:                     0,
		Bootstrap:                    true,
		Dependencies:                 []string{windows.ContainerdServiceName},
		PowershellVariablesInCommand: nil,
		NodeVariablesInCommand:       nil,
	}, nil
}

// gatherKubeletArgs gathers the required kubelet args from the ignition config
func gatherKubeletArgs(platform string) ([]string, error) {
	argsFromIgnition, err := parseKubeletArgs(ignition.RenderedWorker.KubeletUnit)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing kubelet systemd unit args")
	}

	// Generate the full list of kubelet arguments from the arguments present in the ignition file
	kubeletArgs, err := generateInitialKubeletArgs(platform, argsFromIgnition)
	if err != nil {
		return nil, fmt.Errorf("cannot generate initial kubelet args: %w", err)
	}
	return kubeletArgs, nil
}

// parseKubeletArgs returns args we are interested in from the kubelet systemd unit file
func parseKubeletArgs(unit ignCfgTypes.Unit) (map[string]string, error) {
	if unit.Contents == nil {
		return nil, fmt.Errorf("could not process %s: Unit is empty", unit.Name)
	}

	kubeletArgs := make(map[string]string)
	results := ignition.CloudProviderRegex.FindStringSubmatch(*unit.Contents)
	if len(results) == 2 {
		kubeletArgs["cloud-provider"] = results[1]
	}

	// Check for the presence of "--cloud-config" option
	results = ignition.CloudConfigRegex.FindStringSubmatch(*unit.Contents)
	if len(results) == 2 {
		cloudConfigFilename := filepath.Base(results[1])
		// Check if we were able to get a valid filename. Read filepath.Base() godoc for explanation.
		if cloudConfigFilename == "." || os.IsPathSeparator(cloudConfigFilename[0]) {
			return nil, fmt.Errorf("could not get cloud config filename from %s", results[1])
		}
		// As the cloud-config option is a path it must be changed to point to the local file
		localCloudConfigDestination := filepath.Join(windows.GetK8sDir(), cloudConfigFilename)
		kubeletArgs[ignition.CloudConfigOption] = localCloudConfigDestination
	}

	results = ignition.VerbosityRegex.FindStringSubmatch(*unit.Contents)
	if len(results) == 2 {
		kubeletArgs["v"] = results[1]
	}
	return kubeletArgs, nil
}

// generateInitialKubeletArgs returns the kubelet args required during initial kubelet start up
func generateInitialKubeletArgs(platform string, args map[string]string) ([]string, error) {
	kubeletArgs := []string{
		"--config=" + windows.KubeletConfigPath,
		"--bootstrap-kubeconfig=" + windows.BootstrapKubeconfig,
		"--kubeconfig=" + windows.KubeconfigPath,
		"--cert-dir=" + ignition.CertDirectory,
		"--windows-service",
		"--logtostderr=false",
		"--log-file=" + windows.KubeletLog,
		// Registers the Kubelet with Windows specific taints so that linux pods won't get scheduled onto
		// Windows nodes.
		// TODO: Write a `against the cluster` e2e test which checks for the Windows node object created
		// and check for taint.
		"--register-with-taints=" + ignition.WindowsTaints,
		// Label that WMCB uses
		"--node-labels=" + nodeconfig.WindowsOSLabel,
		"--container-runtime=remote",
		"--container-runtime-endpoint=" + ignition.ContainerdEndpointValue,
		"--resolv-conf=",
	}
	if cloudProvider, ok := args["cloud-provider"]; ok {
		kubeletArgs = append(kubeletArgs, "--cloud-provider="+cloudProvider)
	}
	if v, ok := args["v"]; ok && v != "" {
		kubeletArgs = append(kubeletArgs, "--v="+v)
	} else {
		// In case the verbosity argument is missing, use a default value
		kubeletArgs = append(kubeletArgs, "--v="+"3")
	}
	if cloudConfigValue, ok := args[ignition.CloudConfigOption]; ok {
		kubeletArgs = append(kubeletArgs, "--"+ignition.CloudConfigOption+"="+cloudConfigValue)
	}

	hostname, err := getKubeletHostnameOverride(platform)
	if err != nil {
		return nil, err
	}
	if hostname != "" {
		kubeletArgs = append(kubeletArgs, "--hostname-override="+hostname)
	}

	return kubeletArgs, nil
}

// getKubeletHostnameOverride returns correct hostname for kubelet if it should
// be overridden, or an empty string otherwise.
func getKubeletHostnameOverride(platformType string) (string, error) {
	platformType = strings.ToUpper(platformType)
	switch platformType {
	case string(config.AWSPlatformType):
		return getAWSMetadataHostname()
	default:
		return "", nil
	}
}

// getAWSMetadataHostname returns name of the AWS host from metadata service
func getAWSMetadataHostname() (string, error) {
	cfg, err := awsConfig.LoadDefaultConfig(context.TODO())
	if err != nil {
		return "", fmt.Errorf("unable to load config: %w", err)
	}

	client := imds.NewFromConfig(cfg)

	// For compatibility with the AWS in-tree provider
	// Set node name to be instance name instead of the default FQDN hostname
	//
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
	hostnameRes, err := client.GetMetadata(context.TODO(), &imds.GetMetadataInput{
		Path: "local-hostname",
	})
	if err != nil {
		return "", fmt.Errorf("unable to retrieve the hostname from the EC2 instance: %w", err)
	}

	defer hostnameRes.Content.Close()
	hostname, err := io.ReadAll(hostnameRes.Content)
	if err != nil {
		return "", fmt.Errorf("cannot read hostname from the EC2 instance: %w", err)
	}

	return string(hostname), nil
}
