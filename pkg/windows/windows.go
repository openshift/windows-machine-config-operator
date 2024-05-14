package windows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	config "github.com/openshift/api/config/v1"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
	"github.com/openshift/windows-machine-config-operator/pkg/instance"
	"github.com/openshift/windows-machine-config-operator/pkg/nodeconfig/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

const (
	// remoteDir is the remote temporary directory created on the Windows VM
	remoteDir = "C:\\Temp"
	// GcpGetHostnameScriptRemotePath is the remote location of the PowerShell script that resolves the hostname
	// for GCP instances
	GcpGetHostnameScriptRemotePath = remoteDir + "\\" + payload.GcpGetHostnameScriptName
	// WinDefenderExclusionScriptRemotePath is the remote location of the PowerShell script that creates an exclusion
	// for containerd if the Windows Defender Antivirus is active
	WinDefenderExclusionScriptRemotePath = remoteDir + "\\" + payload.WinDefenderExclusionScriptName
	// HNSPSModule is the remote location of the hns.psm1 module
	HNSPSModule = remoteDir + "\\hns.psm1"
	// K8sDir is the remote kubernetes executable directory
	K8sDir = "C:\\k"
	// CredentialProviderConfig is the config file for the credential provider
	CredentialProviderConfig = K8sDir + "\\credential-provider-config.yaml"
	// KubeconfigPath is the remote location of the kubelet's kubeconfig
	KubeconfigPath = K8sDir + "\\kubeconfig"
	// logDir is the remote kubernetes log directory
	logDir = "C:\\var\\log"
	// KubeletLogDir is the remote kubelet log directory
	KubeletLogDir = logDir + "\\kubelet"
	// KubeProxyLogDir is the remote kube-proxy log directory
	KubeProxyLogDir = logDir + "\\kube-proxy"
	// HybridOverlayLogDir is the remote hybrid-overlay log directory
	HybridOverlayLogDir = logDir + "\\hybrid-overlay"
	// wicdLogDir is the remote wicd log directory
	wicdLogDir = logDir + "\\wicd"
	// cniDir is the directory for storing CNI binaries
	cniDir = K8sDir + "\\cni"
	// CniConfDir is the directory for storing CNI configuration
	CniConfDir = cniDir + "\\config"
	// ContainerdDir is the directory for storing Containerd binary
	ContainerdDir = K8sDir + "\\containerd"
	// ContainerdPath is the location of the containerd exe
	ContainerdPath = ContainerdDir + "\\containerd.exe"
	// ContainerdConfPath is the location of containerd config file
	ContainerdConfPath = ContainerdDir + "\\containerd_conf.toml"
	// ContainerdConfigDir is the remote directory for containerd registry config
	ContainerdConfigDir = ContainerdDir + "\\registries"
	// containerdLogDir is the remote containerd log directory
	containerdLogDir = logDir + "\\containerd"
	// ContainerdLogPath is the location of the containerd log file
	ContainerdLogPath = containerdLogDir + "\\containerd.log"
	// ContainerdServiceName is containerd Windows service name
	ContainerdServiceName = "containerd"
	// WicdServiceName is the Windows service name for WICD
	WicdServiceName = "windows-instance-config-daemon"
	// wicdPath is the path to the WICD executable
	wicdPath = K8sDir + "\\windows-instance-config-daemon.exe"
	// windowsExporterPath is the location of the windows_exporter.exe
	windowsExporterPath = K8sDir + "\\windows_exporter.exe"
	// NetworkConfScriptPath is the location of the network configuration script
	NetworkConfScriptPath = remoteDir + "\\network-conf.ps1"
	// AzureCloudNodeManagerPath is the location of the azure-cloud-node-manager.exe
	AzureCloudNodeManagerPath = K8sDir + "\\" + payload.AzureCloudNodeManager
	// ECRCredentialProviderPath is the location of ecr credential provider exe
	ECRCredentialProviderPath = K8sDir + "\\ecr-credential-provider.exe"
	// podManifestDirectory is the directory needed by kubelet for the static pods
	// We shouldn't override if the pod manifest directory already exists
	podManifestDirectory = K8sDir + "\\etc\\kubernetes\\manifests"
	// BootstrapKubeconfigPath is the location of the bootstrap kubeconfig
	BootstrapKubeconfigPath = K8sDir + "\\bootstrap-kubeconfig"
	// KubeletPath is the location of the kubelet exe
	KubeletPath = K8sDir + "\\kubelet.exe"
	// KubeLogRunnerPath is the location of the kube-log-runner exe
	KubeLogRunnerPath = K8sDir + "\\kube-log-runner.exe"
	// KubeletConfigPath is the location of the kubelet configuration file
	KubeletConfigPath = K8sDir + "\\kubelet.conf"
	// KubeletLog is the location of the kubelet log file
	KubeletLog = KubeletLogDir + "\\kubelet.log"
	// KubeProxyLog is the location of the kube-proxy log file
	KubeProxyLog = KubeProxyLogDir + "\\kube-proxy.log"
	// KubeProxyPath is the location of the kube-proxy exe
	KubeProxyPath = K8sDir + "\\kube-proxy.exe"
	// CSIProxyPath is the location of the csi-proxy exe
	CSIProxyPath = K8sDir + "\\csi-proxy.exe"
	// csiProxyLogDir is the location of the csi-proxy log file
	csiProxyLogDir = logDir + "\\csi-proxy"
	// CSIProxyLog is the location of the csi-proxy log file
	CSIProxyLog = csiProxyLogDir + "\\csi-proxy.log"
	// HybridOverlayPath is the location of the hybrid-overlay-node exe
	HybridOverlayPath = K8sDir + "\\hybrid-overlay-node.exe"
	// HybridOverlayServiceName is the name of the hybrid-overlay-node Windows service
	HybridOverlayServiceName = "hybrid-overlay-node"
	// BaseOVNKubeOverlayNetwork is the name of base OVN HNS Overlay network
	BaseOVNKubeOverlayNetwork = "BaseOVNKubernetesHybridOverlayNetwork"
	// OVNKubeOverlayNetwork is the name of the OVN HNS Overlay network
	OVNKubeOverlayNetwork = "OVNKubernetesHybridOverlayNetwork"
	// KubeProxyServiceName is the name of the kube-proxy Windows service
	KubeProxyServiceName = "kube-proxy"
	// KubeletServiceName is the name of the kubelet Windows service
	KubeletServiceName = "kubelet"
	// WindowsExporterServiceName is the name of the windows_exporter Windows service
	WindowsExporterServiceName = "windows_exporter"
	// AzureCloudNodeManagerServiceName is the name of the azure cloud node manager service
	AzureCloudNodeManagerServiceName = "cloud-node-manager"
	// WindowsExporterServiceCommand specifies metrics for the windows_exporter service to collect
	// and expose metrics at endpoint with default port :9182 and default URL path /metrics
	WindowsExporterServiceCommand = windowsExporterPath + " --collectors.enabled " +
		"cpu,cs,logical_disk,net,os,service,system,textfile,container,memory,cpu_info"
	// serviceQueryCmd is the Windows command used to query a service
	serviceQueryCmd = "sc.exe qc "
	// serviceNotFound is part of the error output returned when a service does not exist. 1060 is an error code
	// representing ERROR_SERVICE_DOES_NOT_EXIST
	// referenced: https://docs.microsoft.com/en-us/windows/win32/debug/system-error-codes--1000-1299-
	serviceNotFound = "FAILED 1060"
	// cmdExitNoStatus is part of the error message returned when a command takes too long to report status back to
	// PowerShell.
	cmdExitNoStatus = "command exited without exit status or exit signal"
	// ManagedTag indicates that the service being described is managed by OpenShift. This ensures that all services
	// created as part of Node configuration can be searched for by checking their description for this string
	ManagedTag = "OpenShift managed"
	// containersFeatureName is the name of the Windows feature that is required to be enabled on the Windows instance.
	containersFeatureName = "Containers"
	// wicdKubeconfigPath is the path of the kubeconfig used by WICD
	wicdKubeconfigPath = K8sDir + "\\wicd-kubeconfig"
	// TrustedCABundlePath is the location of the trusted CA bundle file
	TrustedCABundlePath = remoteDir + "\\ca-bundle.crt"
)

var (
	// RequiredServices is a list of Windows services installed by WMCO. WICD owns all services aside from itself.
	// The order of this slice matters due to service dependencies. If a service depends on another service, the
	// dependent service should be placed before the service it depends on.
	RequiredServices = []string{
		WindowsExporterServiceName,
		KubeProxyServiceName,
		HybridOverlayServiceName,
		KubeletServiceName,
		WicdServiceName,
		ContainerdServiceName}
	// RequiredDirectories is a list of directories to be created by WMCO
	RequiredDirectories = []string{
		remoteDir,
		cniDir,
		CniConfDir,
		logDir,
		KubeletLogDir,
		csiProxyLogDir,
		KubeProxyLogDir,
		wicdLogDir,
		HybridOverlayLogDir,
		ContainerdDir,
		containerdLogDir,
		ContainerdConfigDir,
		podManifestDirectory,
		K8sDir,
	}
)

// createPayload returns the map of files to transfer with generated file info
func createPayload(platform *config.PlatformType) (map[*payload.FileInfo]string, error) {
	srcDestPairs := getFilesToTransfer(platform)
	files := make(map[*payload.FileInfo]string)
	for src, dest := range srcDestPairs {
		f, err := payload.NewFileInfo(src)
		if err != nil {
			return nil, fmt.Errorf("could not create FileInfo object for file %s: %w", src, err)
		}
		files[f] = dest
	}
	return files, nil
}

// getFilesToTransfer returns the properly populated filesToTransfer map. Note this does not include the WICD binary.
func getFilesToTransfer(platform *config.PlatformType) map[string]string {
	srcDestPairs := map[string]string{
		payload.GcpGetValidHostnameScriptPath:  remoteDir,
		payload.WinDefenderExclusionScriptPath: remoteDir,
		payload.HybridOverlayPath:              K8sDir,
		payload.HNSPSModule:                    remoteDir,
		payload.WindowsExporterPath:            K8sDir,
		payload.WinBridgeCNIPlugin:             cniDir,
		payload.HostLocalCNIPlugin:             cniDir,
		payload.WinOverlayCNIPlugin:            cniDir,
		payload.KubeProxyPath:                  K8sDir,
		payload.KubeletPath:                    K8sDir,
		payload.KubeLogRunnerPath:              K8sDir,
		payload.CSIProxyPath:                   K8sDir,
		payload.ContainerdPath:                 ContainerdDir,
		payload.HcsshimPath:                    ContainerdDir,
		payload.ContainerdConfPath:             ContainerdDir,
		payload.NetworkConfigurationScript:     remoteDir,
	}

	if platform == nil {
		return srcDestPairs
	}
	switch *platform {
	case config.AWSPlatformType:
		srcDestPairs[payload.ECRCredentialProviderPath] = K8sDir
	case config.AzurePlatformType:
		srcDestPairs[payload.AzureCloudNodeManagerPath] = K8sDir
	}
	return srcDestPairs
}

// GetK8sDir returns the location of the kubernetes executable directory
func GetK8sDir() string {
	return K8sDir
}

// Windows contains all the methods needed to configure a Windows VM to become a worker node
type Windows interface {
	// GetIPv4Address returns the IPv4 address of the associated instance.
	GetIPv4Address() string
	// EnsureFile ensures the given file exists within the specified directory on the Windows VM. The file will be copied
	// to the Windows VM if it is not present or if it has the incorrect contents. The remote directory is created if it
	// does not exist.
	EnsureFile(*payload.FileInfo, string) error
	// EnsureFileContent ensures the given filename and content exists within the specified directory on the Windows VM.
	// The content will be copied to the Windows VM if the file is not present or has incorrect contents. The remote
	// directory is created if it does not exist.
	EnsureFileContent([]byte, string, string) error
	// FileExists returns true if a specific file exists at the given path and checksum on the Windows VM. Set an
	// empty checksum (checksum == "") to disable checksum check.
	FileExists(string, string) (bool, error)
	// ReplaceDir transfers the given files to their given paths within the remote directory the Windows instance.
	// The destination dir will only contain the given files after this function is called, clearing existing content.
	ReplaceDir(map[string][]byte, string) error
	// Run executes the given command remotely on the Windows VM over a ssh connection and returns the combined output
	// of stdout and stderr. If the bool is set, it implies that the cmd is to be execute in PowerShell. This function
	// should be used in scenarios where you want to execute a command that runs in the background. In these cases we
	// have observed that Run() returns before the command completes and as a result killing the process.
	Run(string, bool) (string, error)
	// RebootAndReinitialize reboots the instance and re-initializes the Windows SSH client
	RebootAndReinitialize() error
	// Bootstrap prepares the Windows instance and runs the WICD bootstrap command
	Bootstrap(string, string, string) error
	// ConfigureWICD ensures that the Windows Instance Config Daemon is running on the node
	ConfigureWICD(string, string) error
	// RemoveFilesAndNetworks removes all files and networks created by WMCO
	RemoveFilesAndNetworks() error
	// RunWICDCleanup ensures the WICD service is stopped and runs the cleanup command that ensures all WICD-managed
	// services are also stopped
	RunWICDCleanup(string, string) error
	// RestoreAWSRoutes restores the default routes on AWS VMs. This function should not be called on non-AWS VMs
	RestoreAWSRoutes() error
}

// windows implements the Windows interface
type windows struct {
	// clusterDNS is the IP address of the DNS server used for all containers
	clusterDNS string
	// signer is used for authenticating against the VM
	signer ssh.Signer
	// interact is used to connect to and interact with the VM
	interact connectivity
	// instance contains information about the Windows instance to interact with
	// A valid instance is configured with a network address that either is an IPv4 address or resolves to one.
	instance *instance.Info
	log      logr.Logger
	// defaultShellPowerShell indicates if the default SSH shell is PowerShell
	defaultShellPowerShell bool
	// filesToTransfer is the map of files needed for the windows VM
	filesToTransfer map[*payload.FileInfo]string
}

// New returns a new Windows instance constructed from the given WindowsVM
func New(clusterDNS string, instanceInfo *instance.Info, signer ssh.Signer, platform *config.PlatformType) (Windows, error) {
	log := ctrl.Log.WithName(fmt.Sprintf("wc %s", instanceInfo.Address))
	log.V(1).Info("initializing SSH connection")
	conn, err := newSshConnectivity(instanceInfo.Username, instanceInfo.Address, signer, log)
	if err != nil {
		return nil, fmt.Errorf("unable to setup VM %s sshConnectivity: %w", instanceInfo.Address, err)
	}

	files, err := createPayload(platform)
	if err != nil {
		return nil, fmt.Errorf("unable to create payload: %w", err)
	}

	return &windows{
			interact:               conn,
			clusterDNS:             clusterDNS,
			instance:               instanceInfo,
			log:                    log,
			defaultShellPowerShell: defaultShellPowershell(conn),
			filesToTransfer:        files,
		},
		nil
}

// defaultShellPowershell returns true if the default SSH shell of the connected VM is PowerShell.
func defaultShellPowershell(conn connectivity) bool {
	// Get-Help is a basic command that is expected to work on PowerShell and not through cmd.exe.
	// If this command succeed, it is safe to assume that the shell is PowerShell.
	// If it fails there is a chance of having a false-negative, but since we already have a connection to the VM
	// it is much more likely that the shell is not PowerShell.
	_, err := conn.run("Get-Help")
	if err != nil {
		return false
	}
	return true
}

// Interface methods

func (vm *windows) GetIPv4Address() string {
	return vm.instance.IPv4Address
}

func (vm *windows) EnsureFileContent(contents []byte, filename string, remoteDir string) error {
	// build remote path
	remotePath := remoteDir + "\\" + filepath.Base(filename)
	// calc checksum
	checksum := fmt.Sprintf("%x", sha256.Sum256(contents))
	// check if the file exist with the expected content
	fileExists, err := vm.FileExists(remotePath, checksum)
	if err != nil {
		return fmt.Errorf("error checking if file '%s' exists on the Windows VM: %w", remotePath, err)
	}
	if fileExists {
		// The file already exists with the expected content, do nothing
		return nil
	}
	vm.log.V(1).Info("copy", "file content", filename, "remote dir", remoteDir)

	c, err := vm.interact.createSFTPClient()
	if err != nil {
		return fmt.Errorf("Failed to create SFTP client: %w", err)
	}
	if err := vm.interact.transfer(c, bytes.NewReader(contents), filename, remoteDir); err != nil {
		return fmt.Errorf("unable to copy %s content to remote dir %s: %w", filename, remoteDir, err)
	}
	return c.Close()
}

func (vm *windows) EnsureFile(file *payload.FileInfo, remoteDir string) error {
	// Only copy the file to the Windows VM if it does not already exist wth the desired content
	remotePath := remoteDir + "\\" + filepath.Base(file.Path)
	fileExists, err := vm.FileExists(remotePath, file.SHA256)
	if err != nil {
		return fmt.Errorf("error checking if file '%s' exists on the Windows VM: %w", remotePath, err)
	}
	if fileExists {
		// The file already exists with the expected content, do nothing
		return nil
	}
	f, err := os.Open(file.Path)
	if err != nil {
		return fmt.Errorf("error opening %s file to be transferred: %w", file.Path, err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			vm.log.Error(err, "error closing local file", "file", file.Path)
		}
	}()
	vm.log.V(1).Info("copy", "local file", file.Path, "remote dir", remoteDir)

	c, err := vm.interact.createSFTPClient()
	if err != nil {
		return fmt.Errorf("Failed to create SFTP client: %w", err)
	}
	if err := vm.interact.transfer(c, f, filepath.Base(file.Path), remoteDir); err != nil {
		return fmt.Errorf("unable to transfer %s to remote dir %s: %w", file.Path, remoteDir, err)
	}
	return c.Close()
}

func (vm *windows) FileExists(path, checksum string) (bool, error) {
	out, err := vm.Run("Test-Path "+path, true)
	if err != nil {
		return false, fmt.Errorf("error checking if file %s exists: %w", path, err)
	}
	found := strings.TrimSpace(out) == "True"
	// avoid checksum validation if not found or in lack of reference checksum
	if !found || checksum == "" {
		return found, nil
	}
	// file exist, compare checksum
	remoteFile, err := vm.newFileInfo(path)
	if err != nil {
		return false, fmt.Errorf("error getting info on file '%s' on the Windows VM: %w", path, err)
	}
	if remoteFile.SHA256 == checksum {
		vm.log.V(1).Info("file already exists on VM with expected content", "file", path)
		return true, nil
	}
	// file exist with diff content
	return false, nil
}

func (vm *windows) ReplaceDir(files map[string][]byte, remoteDir string) error {
	vm.log.V(1).Info("publishing", "remote destination dir", remoteDir, "number of files", len(files))

	if out, err := vm.Run(rmDirCmd(remoteDir), true); err != nil {
		return fmt.Errorf("unable to remove directory %s, out: %s, err: %s", remoteDir, out, err)
	}
	if out, err := vm.Run(mkdirCmd(remoteDir), false); err != nil {
		return fmt.Errorf("unable to create remote directory %s, out: %s: %w", remoteDir, out, err)
	}

	sftpClient, err := vm.interact.createSFTPClient()
	if err != nil {
		return fmt.Errorf("Failed to create SFTP client: %w", err)
	}
	if err = vm.interact.transferFiles(sftpClient, files, remoteDir); err != nil {
		return err
	}
	return sftpClient.Close()
}

func (vm *windows) Run(cmd string, psCmd bool) (string, error) {
	if psCmd && !vm.defaultShellPowerShell {
		cmd = formatRemotePowerShellCommand(cmd)
	} else if !psCmd && vm.defaultShellPowerShell {
		// When running cmd through powershell, double quotes can cause parsing issues, so replace with single quotes
		// CMD doesn't treat ' as quotes when processing commands, so the quotes must be changed on a case by case basis
		cmd = strings.ReplaceAll(cmd, "\"", "'")
		cmd = "cmd /c " + cmd
	}

	out, err := vm.interact.run(cmd)
	if err != nil {
		return out, fmt.Errorf("error running %s: %w", cmd, err)
	}
	vm.log.V(1).Info("run", "cmd", cmd, "out", out)
	return out, nil
}

// RebootAndReinitialize restarts the Windows instance and re-initializes the SSH connection for further configuration
func (vm *windows) RebootAndReinitialize() error {
	vm.log.Info("rebooting instance")
	if _, err := vm.Run("Restart-Computer -Force", true); err != nil {
		return fmt.Errorf("error rebooting the Windows VM: %w", err)
	}
	// Wait for instance to be unreachable via SSH, implies reboot is underway
	if err := vm.waitUntilUnreachable(); err != nil {
		return fmt.Errorf("instance reboot failed to start: %w", err)
	}
	// Wait for instance to come back online and reinitialize the SSH connection after the reboot
	if err := vm.reinitialize(); err != nil {
		return fmt.Errorf("error reinitializing SSH connection after VM reboot: %w", err)
	}
	vm.log.V(1).Info("successful reboot")
	return nil
}

func (vm *windows) RunWICDCleanup(watchNamespace, wicdKubeconfig string) error {
	// Make sure WICD service is not running before calling node cleanup and/or bootstrap
	if err := vm.deconfigureWICD(); err != nil {
		return err
	}
	// We have to ensure the WICD files exist separately before the rest of the files because we must ensure services
	// are stopped and their files closed before modifying them.
	if err := vm.ensureWICDFilesExist(wicdKubeconfig); err != nil {
		return err
	}
	wicdCleanupCmd := fmt.Sprintf("%s cleanup --kubeconfig %s --namespace %s", wicdPath, wicdKubeconfigPath,
		watchNamespace)
	if out, err := vm.Run(wicdCleanupCmd, true); err != nil {
		vm.log.Info("failed to cleanup node", "command", wicdCleanupCmd, "output", out)
		return err
	}
	return nil
}

func (vm *windows) RemoveFilesAndNetworks() error {
	if err := vm.ensureHNSNetworksAreRemoved(); err != nil {
		return fmt.Errorf("unable to ensure HNS networks are removed: %w", err)
	}
	if err := vm.removeDirectories(); err != nil {
		return fmt.Errorf("unable to remove created directories: %w", err)
	}
	return nil
}

func (vm *windows) Bootstrap(desiredVer, watchNamespace, wicdKubeconfigContents string) error {
	vm.log.Info("configuring")

	// Stop any services that may be running. This prevents the node being shown as Ready after a failed configuration.
	if err := vm.RunWICDCleanup(watchNamespace, wicdKubeconfigContents); err != nil {
		return fmt.Errorf("unable to cleanup the Windows instance: %w", err)
	}

	if err := vm.ensureHostNameAndContainersFeature(); err != nil {
		return err
	}
	if err := vm.createDirectories(); err != nil {
		return fmt.Errorf("error creating directories on Windows VM: %w", err)
	}
	if err := vm.transferFiles(); err != nil {
		return fmt.Errorf("error transferring files to Windows VM: %w", err)
	}

	wicdBootstrapCmd := fmt.Sprintf("%s bootstrap --desired-version %s --kubeconfig %s --namespace %s",
		wicdPath, desiredVer, wicdKubeconfigPath, watchNamespace)
	if out, err := vm.Run(wicdBootstrapCmd, true); err != nil {
		vm.log.Info("failed to bootstrap node", "command", wicdBootstrapCmd, "output", out)
		return err
	}
	return nil
}

// ConfigureWICD starts the Windows Instance Config Daemon service
func (vm *windows) ConfigureWICD(watchNamespace, wicdKubeconfigContents string) error {
	if err := vm.ensureWICDFilesExist(wicdKubeconfigContents); err != nil {
		return err
	}
	wicdServiceArgs := fmt.Sprintf("controller --windows-service --log-dir %s --kubeconfig %s --namespace %s",
		wicdLogDir, wicdKubeconfigPath, watchNamespace)
	if cluster.IsProxyEnabled() {
		wicdServiceArgs = fmt.Sprintf("%s --ca-bundle %s", wicdServiceArgs, TrustedCABundlePath)
	}
	// if WICD crashes, attempt to restart WICD after 10, 30, and 60 seconds, and then every 2 minutes after that.
	// reset this counter 5 min after a period with no crashes
	recoveryActions := []recoveryAction{
		{
			actionType: serviceRestart,
			delay:      10,
		},
		{
			actionType: serviceRestart,
			delay:      30,
		},
		{
			actionType: serviceRestart,
			delay:      60,
		},
		{
			actionType: serviceRestart,
			delay:      120,
		},
	}
	// if WICD has not crashed in the past 5 minutes, reset the crash counter
	recoveryPeriod := 300
	wicdService, err := newService(wicdPath, WicdServiceName, wicdServiceArgs, nil, recoveryActions, recoveryPeriod)
	if err != nil {
		return fmt.Errorf("error creating %s service object: %w", WicdServiceName, err)
	}
	if err := vm.ensureServiceIsRunning(wicdService); err != nil {
		return fmt.Errorf("error ensuring %s Windows service has started running: %w", WicdServiceName, err)
	}
	vm.log.Info("configured", "service", WicdServiceName, "args", wicdServiceArgs)
	return nil
}

func (vm *windows) RestoreAWSRoutes() error {
	ec2LaunchV2ServiceName := "\"Amazon EC2Launch\""
	serviceExists, err := vm.serviceExists(ec2LaunchV2ServiceName)
	if err != nil {
		return fmt.Errorf("error checking if %s service exists: %w", ec2LaunchV2ServiceName, err)
	}
	// We don't want ensureServiceIsRunning to create the service if it does not exist, so we return without an error.
	// Returning an error does not make sense as we could have an AMI configured without the EC2Launch service present
	// and the route created using some other customer specific method which is unknown to us.
	if !serviceExists {
		vm.log.Info("missing", "service", ec2LaunchV2ServiceName)
		return nil
	}
	ec2Launch, err := newService("C:\\Program Files\\Amazon\\EC2Launch\\service\\EC2LaunchService.exe",
		ec2LaunchV2ServiceName, "", nil, nil, 0)
	if err != nil {
		return err
	}
	if err = vm.ensureServiceIsRunning(ec2Launch); err != nil {
		return err
	}
	// The EC2Launch service stops running once it has completed its tasks like creating routes. So we wait until it
	// has stopped running before proceeding.
	if err = vm.waitStopped(ec2LaunchV2ServiceName); err != nil {
		return err
	}
	return nil
}

// Interface helper methods

// ensureWICDFilesExist ensures all files required for WICD to run exist. If needed, creates the destination directory,
// WICD binary, and kubeconfig.
func (vm *windows) ensureWICDFilesExist(wicdKubeconfig string) error {
	if _, err := vm.Run(mkdirCmd(K8sDir), false); err != nil {
		return fmt.Errorf("unable to create remote directory %s: %w", K8sDir, err)
	}
	wicdFileInfo, err := payload.NewFileInfo(payload.WICDPath)
	if err != nil {
		return fmt.Errorf("could not create FileInfo object for file %s: %w", payload.WICDPath, err)
	}
	if err := vm.EnsureFile(wicdFileInfo, K8sDir); err != nil {
		return fmt.Errorf("error copying %s to %s: %w", wicdFileInfo.Path, K8sDir, err)
	}
	return vm.ensureWICDKubeconfig(wicdKubeconfig)
}

// ensureHostNameAndContainersFeature ensures hostname of the Windows VM matches the expected name
// and the required Windows feature is enabled.
func (vm *windows) ensureHostNameAndContainersFeature() error {
	rebootNeeded := false
	// Set the hostName of the Windows VM if needed
	if vm.instance.NewHostname != "" {
		hostNameChangedNeeded, err := vm.isHostNameChangeNeeded()
		if err != nil {
			return err
		}
		if hostNameChangedNeeded {
			if err := vm.changeHostName(); err != nil {
				return err
			}
			rebootNeeded = true
		}
	}
	isContainersFeatureEnabled, err := vm.isContainersFeatureEnabled()
	if err != nil {
		return err
	}
	if !isContainersFeatureEnabled {
		if err := vm.enableContainersWindowsFeature(); err != nil {
			return fmt.Errorf("error enabling Windows Containers feature: %w", err)
		}
		rebootNeeded = true
	}
	// Changing the host name or enabling the Containers feature requires a VM restart for
	// the change to take effect.
	if rebootNeeded {
		if err := vm.RebootAndReinitialize(); err != nil {
			return fmt.Errorf("error restarting the Windows instance and reinitializing SSH connection: %w", err)
		}
	}

	return nil
}

// isHostNameChangeNeeded tells if we need to update the host name of the Windows VM
func (vm *windows) isHostNameChangeNeeded() (bool, error) {
	out, err := vm.Run("hostname", true)
	if err != nil {
		return false, fmt.Errorf("error getting the host name, with stdout %s: %w", out, err)
	}
	return !strings.Contains(out, vm.instance.NewHostname), nil
}

// changeHostName changes the hostName of the Windows VM to match the expected value
func (vm *windows) changeHostName() error {
	changeHostNameCommand := "Rename-Computer -NewName " + vm.instance.NewHostname + " -Force"
	out, err := vm.Run(changeHostNameCommand, true)
	if err != nil {
		vm.log.Info("changing host name failed", "command", changeHostNameCommand, "output", out)
		return fmt.Errorf("changing host name failed: %w", err)
	}
	return nil
}

// createDirectories creates directories required for configuring the Windows node on the VM
func (vm *windows) createDirectories() error {
	for _, dir := range RequiredDirectories {
		if _, err := vm.Run(mkdirCmd(dir), false); err != nil {
			return fmt.Errorf("unable to create remote directory %s: %w", dir, err)
		}
	}
	return nil
}

// removeDirectories removes all directories created as part of the configuration process
func (vm *windows) removeDirectories() error {
	vm.log.Info("removing directories")
	for _, dir := range RequiredDirectories {
		if dir == K8sDir {
			// Exclude WICD binary and credential files, used in the WICD cleanup cmd
			// This allows us to retry reconciliation again in case of a failure here
			// which will result in the WICD cleanup cmd being called.
			if out, err := vm.Run(rmK8sFilesCmd(), true); err != nil {
				return fmt.Errorf("unable to remove directory %s, out: %s, err: %s", dir, out, err)
			}
			continue
		}
		if out, err := vm.Run(rmDirCmd(dir), true); err != nil {
			return fmt.Errorf("unable to remove directory %s, out: %s, err: %s", dir, out, err)
		}
	}
	// Do a best effort deletion of k8sDir which now includes only the WICD related files
	// At this point, we don't want to retry reconciliation again as the WICD files could have been deleted.
	if out, err := vm.Run(rmDirCmd(K8sDir), true); err != nil {
		vm.log.V(1).Error(err, "unable to remove directory", "dir", K8sDir, "out", out)
	}
	return nil
}

// transferFiles copies various files required for configuring the Windows node, to the VM.
func (vm *windows) transferFiles() error {
	vm.log.Info("transferring files")
	for src, dest := range vm.filesToTransfer {
		if err := vm.EnsureFile(src, dest); err != nil {
			return fmt.Errorf("error copying %s to %s: %w", src.Path, dest, err)
		}
	}
	return nil
}

// ensureServiceIsRunning ensures a Windows service is running on the VM, creating and starting it if not already so
func (vm *windows) ensureServiceIsRunning(svc *service) error {
	serviceExists, err := vm.serviceExists(svc.name)
	if err != nil {
		return fmt.Errorf("error checking if %s Windows service exists: %w", svc.name, err)
	}
	// create service if it does not exist
	if !serviceExists {
		if err := vm.createService(svc); err != nil {
			return fmt.Errorf("error creating %s Windows service: %w", svc.name, err)
		}
	}
	if err := vm.startService(svc); err != nil {
		return fmt.Errorf("error starting %s Windows service: %w", svc.name, err)
	}
	return nil
}

// createService creates the service on the Windows VM. The service's description and recovery action will be set after
// the service is created.
func (vm *windows) createService(svc *service) error {
	if svc == nil {
		return fmt.Errorf("service object should not be nil")
	}
	svcCreateCmd := fmt.Sprintf("sc.exe create %s binPath=\"%s %s\" start=auto", svc.name, svc.binaryPath,
		svc.args)
	if len(svc.dependencies) > 0 {
		dependencyList := strings.Join(svc.dependencies, "/")
		svcCreateCmd += " depend=" + dependencyList
	}

	_, err := vm.Run(svcCreateCmd, false)
	if err != nil {
		return fmt.Errorf("failed to create service %s: %w", svc.name, err)
	}

	if err := vm.setServiceDescription(svc.name); err != nil {
		return fmt.Errorf("error setting description of the %s Windows service: %w", svc.name, err)
	}
	if err := vm.setRecoveryActions(svc); err != nil {
		return fmt.Errorf("error setting recovery actions for the %s Windows service: %w", svc.name, err)
	}

	return nil
}

// setServiceDescription sets the given service's description to the expected value. This can only be done after service
// creation.
func (vm *windows) setServiceDescription(svcName string) error {
	cmd := fmt.Sprintf("sc.exe description %s \"%s %s\"", svcName, ManagedTag, svcName)
	out, err := vm.Run(cmd, false)
	if err != nil {
		return fmt.Errorf("failed to set service description with stdout %s: %w", out, err)
	}
	return nil
}

func (vm *windows) setRecoveryActions(svc *service) error {
	if len(svc.recoveryActions) == 0 {
		return nil
	}
	actions := fmt.Sprintf("%s/%d", svc.recoveryActions[0].actionType, svc.recoveryActions[0].delay)
	for _, action := range svc.recoveryActions[1:] {
		actions = fmt.Sprintf("%s/%s/%d", actions, action.actionType, action.delay)
	}
	cmd := fmt.Sprintf("sc.exe failure %s reset= %d actions= %s", svc.name, svc.recoveryPeriod, actions)
	out, err := vm.Run(cmd, false)
	if err != nil {
		return fmt.Errorf("failed to set recovery actions with stdout: %s: %w", out, err)
	}
	return nil
}

// ensureServiceNotRunning stops a service if it exists and is running
func (vm *windows) ensureServiceNotRunning(svc *service) error {
	if svc == nil {
		return fmt.Errorf("service object should not be nil")
	}

	exists, err := vm.serviceExists(svc.name)
	if err != nil {
		return fmt.Errorf("error checking if service exists: %w", err)
	}
	if !exists {
		// service does not exist, therefore it is not running
		return nil
	}

	running, err := vm.isRunning(svc.name)
	if err != nil {
		return fmt.Errorf("unable to check if service is running: %w", err)
	}
	if !running {
		return nil
	}
	if err := vm.stopService(svc); err != nil {
		return fmt.Errorf("unable to stop service: %w", err)
	}
	return nil

}

// ensureServiceIsRemoved ensures that the given service installed by WMCO is removed from the instance
func (vm *windows) ensureServiceIsRemoved(svcName string) error {
	svc := &service{name: svcName}
	// If the service is not installed, do nothing
	exists, err := vm.serviceExists(svcName)
	if err != nil {
		return fmt.Errorf("error checking if %s Windows service exists: %w", svc.name, err)
	}
	if !exists {
		return nil
	}
	// Make sure the service is stopped before we attempt to delete it
	if err := vm.ensureServiceNotRunning(svc); err != nil {
		return fmt.Errorf("error stopping %s Windows service: %w", svc.name, err)
	}
	if err := vm.deleteService(svc); err != nil {
		return fmt.Errorf("error deleting %s Windows service: %w", svc.name, err)
	}
	vm.log.Info("deconfigured", "service", svc.name)
	return nil
}

// stopService stops the service that was already running
func (vm *windows) stopService(svc *service) error {
	if svc == nil {
		return fmt.Errorf("service object should not be nil")
	}
	// Success here means that the stop has initiated, not necessarily completed
	out, err := vm.Run("sc.exe stop "+svc.name, false)
	if err != nil {
		return fmt.Errorf("failed to stop %s service with output: %s: %w", svc.name, out, err)
	}

	// Wait until the service has stopped
	if err = vm.waitStopped(svc.name); err != nil {
		return fmt.Errorf("error waiting for the %s service to stop: %w", svc.name, err)
	}

	return nil
}

// waitStopped returns once the service has stopped within the retry.Timeout interval otherwise returns an error
func (vm *windows) waitStopped(serviceName string) error {
	return wait.PollImmediate(retry.Interval, retry.Timeout, func() (bool, error) {
		serviceRunning, err := vm.isRunning(serviceName)
		if err != nil {
			vm.log.V(1).Error(err, "unable to check if Windows service is running", "service", serviceName)
			return false, nil
		}
		return !serviceRunning, nil
	})
}

// deleteService deletes the specified Windows service
func (vm *windows) deleteService(svc *service) error {
	if svc == nil {
		return fmt.Errorf("service object cannot be nil")
	}

	// Success here means that the stop has initiated, not necessarily completed
	out, err := vm.Run("sc.exe delete "+svc.name, false)
	if err != nil {
		return fmt.Errorf("failed to delete %s service with output: %s: %w", svc.name, out, err)
	}

	// Wait until the service is fully deleted
	err = wait.PollImmediate(retry.Interval, retry.Timeout, func() (bool, error) {
		exists, err := vm.serviceExists(svc.name)
		if err != nil {
			vm.log.V(1).Error(err, "unable to check if Windows service exists", "service", svc.name)
			return false, nil
		}
		return !exists, nil
	})
	if err != nil {
		return fmt.Errorf("error waiting for the %s service to be deleted: %w", svc.name, err)
	}

	return nil
}

// serviceExists checks if the given service exists on Windows VM
func (vm *windows) serviceExists(serviceName string) (bool, error) {
	out, err := vm.Run(serviceQueryCmd+serviceName, false)
	if err != nil {
		if strings.Contains(out, serviceNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// isRunning checks the status of given service
func (vm *windows) isRunning(serviceName string) (bool, error) {
	out, err := vm.Run("sc.exe query "+serviceName, false)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "RUNNING"), nil
}

// startService starts a previously created Windows service
func (vm *windows) startService(svc *service) error {
	if svc == nil {
		return fmt.Errorf("service object should not be nil")
	}
	serviceRunning, err := vm.isRunning(svc.name)
	if err != nil {
		return fmt.Errorf("unable to check if %s Windows service is running: %w", svc.name, err)
	}
	if serviceRunning {
		return nil
	}
	out, err := vm.Run("sc.exe start "+svc.name, false)
	if err != nil {
		return fmt.Errorf("failed to start %s service with output: %s: %w", svc.name, out, err)
	}
	return nil
}

// newFileInfo returns a pointer to a FileInfo object created from the specified file on the Windows VM
func (vm *windows) newFileInfo(path string) (*payload.FileInfo, error) {
	// Get-FileHash returns an object with multiple properties, we are interested in the `Hash` property
	command := "$out = Get-FileHash " + path + " -Algorithm SHA256; $out.Hash"
	out, err := vm.Run(command, true)
	if err != nil {
		return nil, fmt.Errorf("error getting file hash: %w", err)
	}
	// The returned hash will be in all caps with newline characters, doing ToLower() to
	// make the output normalized with the go sha256 library
	sha := strings.ToLower(strings.TrimSpace(out))
	return &payload.FileInfo{Path: path, SHA256: sha}, nil
}

// ensureHNSNetworksAreRemoved ensures the HNS networks created by the hybrid-overlay configuration process are removed
// by repeatedly checking and retrying the removal of each network.
func (vm *windows) ensureHNSNetworksAreRemoved() error {
	vm.log.Info("removing HNS networks")
	var err error
	// VIP HNS endpoint created by the operator is also deleted when the HNS networks are deleted.
	for _, network := range []string{BaseOVNKubeOverlayNetwork, OVNKubeOverlayNetwork} {
		err = wait.PollImmediate(retry.Interval, retry.Timeout, func() (bool, error) {
			// reinitialize and retry on failure to avoid connection reset SSH errors
			if err := vm.removeHNSNetwork(network); err != nil {
				vm.log.V(1).Error(err, "error removing %s HNS network", "network", network)
				if err := vm.reinitialize(); err != nil {
					return false, fmt.Errorf("error reinitializing VM after removing %s HNS network: %w", network, err)
				}
				return false, nil
			}
			if err := vm.reinitialize(); err != nil {
				return false, fmt.Errorf("error reinitializing VM after removing %s HNS network: %w", network, err)
			}
			out, err := vm.Run(getHNSNetworkCmd(network), true)
			if err != nil {
				vm.log.V(1).Error(err, "error waiting for HNS network", "network", network)
				return false, nil
			}
			return !strings.Contains(out, network), nil
		})
		if err != nil {
			return fmt.Errorf("failed ensuring %s HNS network is removed: %w", network, err)
		}
	}
	return nil
}

// removeHNSNetwork removes the given HNS network.
func (vm *windows) removeHNSNetwork(networkName string) error {
	cmd := getHNSNetworkCmd(networkName) + " | Remove-HnsNetwork;"
	// PowerShell returns error waiting without exit status or signal error when the networks are removed.
	if out, err := vm.Run(cmd, true); err != nil && !strings.Contains(err.Error(), cmdExitNoStatus) {
		return fmt.Errorf("failed to remove %s HNS network with output: %s: %w", networkName, out, err)
	}
	return nil
}

// enableContainersWindowsFeature enables the required Windows Containers feature on the Windows instance.
func (vm *windows) enableContainersWindowsFeature() error {
	command := "$ProgressPreference='SilentlyContinue'; Install-WindowsFeature -Name " + containersFeatureName
	out, err := vm.Run(command, true)
	if err != nil {
		return fmt.Errorf("failed to enable required Windows feature: %s with output: %s: %w",
			containersFeatureName, out, err)
	}
	return nil
}

// isContainersFeatureEnabled returns true if the required Containers Windows feature is enabled on the Windows instance
func (vm *windows) isContainersFeatureEnabled() (bool, error) {
	command := "Get-WindowsOptionalFeature -FeatureName " + containersFeatureName + " -Online"
	out, err := vm.Run(command, true)
	if err != nil {
		return false, fmt.Errorf("failed to get Windows feature: %s: %w", containersFeatureName, err)
	}
	return strings.Contains(out, "Enabled"), nil
}

// waitUntilUnreachable tries to run a dummy command until it fails to see if the instance is reachable via SSH
func (vm *windows) waitUntilUnreachable() error {
	return wait.PollUntilContextTimeout(context.TODO(), retry.WindowsAPIInterval, retry.ResourceChangeTimeout, true,
		func(ctx context.Context) (bool, error) {
			_, err := vm.Run("Get-Help", true)
			return (err != nil), nil
		})
}

func (vm *windows) reinitialize() error {
	if err := vm.interact.init(); err != nil {
		return fmt.Errorf("failed to reinitialize ssh client: %v", err)
	}
	return nil
}

// ensureWICDSecretContent ensures the WICD kubeconfig on the instance has the expected contents
func (vm *windows) ensureWICDKubeconfig(contents string) error {
	kcDir, kc := SplitPath(wicdKubeconfigPath)
	return vm.EnsureFileContent([]byte(contents), kc, kcDir)
}

// deconfigureWICD ensures the WICD service running on the Windows instance is removed
func (vm *windows) deconfigureWICD() error {
	if err := vm.ensureServiceIsRemoved(WicdServiceName); err != nil {
		return fmt.Errorf("error ensuring %s Windows service is removed: %w", WicdServiceName, err)
	}
	return nil
}

// Generic helper methods

// formatRemotePowerShellCommand returns a formatted string, prepended with the required PowerShell prefix and
// surrounding quotes needed to execute the given command on a remote Windows VM
func formatRemotePowerShellCommand(command string) string {
	remotePowerShellCmdPrefix := "powershell.exe -NonInteractive -ExecutionPolicy Bypass"
	return fmt.Sprintf("%s \"%s\"", remotePowerShellCmdPrefix, command)
}

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	// trailing space required due to directories ending in `\` causing issues on VMs with PowerShell as the shell.
	return fmt.Sprintf("if not exist %s mkdir %s ", dirName, dirName)
}

// rmDirCmd returns the PowerShell command to recursively remove a directory if it exists
func rmDirCmd(dirName string) string {
	return fmt.Sprintf("if(Test-Path %s) {Remove-Item -Recurse -Force %s}", dirName, dirName)
}

// rmK8sFilesCmd() returns the PowerShell command to remove the k8sDir files excluding WICD files
func rmK8sFilesCmd() string {
	return fmt.Sprintf("if(Test-Path %s) {Get-ChildItem %s -Recurse -Exclude %s,%s | Remove-Item -Force -Recurse}",
		K8sDir, K8sDir, wicdPath, wicdKubeconfigPath)
}

// getHNSNetworkCmd returns the Windows command to get HNS network by name
func getHNSNetworkCmd(networkName string) string {
	return "Get-HnsNetwork | where { $_.Name -eq '" + networkName + "'}"
}

// SplitPath splits a Windows file path into the directory and base file name.
// Example: 'C:\\k\\bootstrap-kubeconfig' --> dir: 'C:\\k\\', fileName: 'bootstrap-kubeconfig'
func SplitPath(filepath string) (dir string, fileName string) {
	splitIndex := strings.LastIndexByte(filepath, '\\') + 1
	return filepath[:splitIndex], filepath[splitIndex:]
}
