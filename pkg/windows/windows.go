package windows

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"

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
	// removeHNSCommand is the Windows command used to remove HNS network.
	removeHNSCommand = "Remove-HnsNetwork"
	// ManagedTag indicates that the service being described is managed by OpenShift. This ensures that all services
	// created as part of Node configuration can be searched for by checking their description for this string
	ManagedTag = "OpenShift managed"
	// containersFeatureName is the name of the Windows feature that is required to be enabled on the Windows instance.
	containersFeatureName = "Containers"
	// wicdCAFile is the name of the file that holds the WICD service account CA certificate
	wicdCAFile = K8sDir + "\\sa-ca.crt"
	// wicdTokenFile is the name of the file that holds the WICD service account secret token
	wicdTokenFile = K8sDir + "\\sa-token"
)

var (
	// filesToTransfer is a map of what files should be copied to the Windows VM and where they should be copied to
	filesToTransfer map[*payload.FileInfo]string
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
		KubeProxyLogDir,
		wicdLogDir,
		HybridOverlayLogDir,
		ContainerdDir,
		containerdLogDir,
		podManifestDirectory,
		K8sDir,
	}
)

// getFilesToTransfer returns the properly populated filesToTransfer map. Note this does not include the WICD binary.
func getFilesToTransfer() (map[*payload.FileInfo]string, error) {
	if filesToTransfer != nil {
		return filesToTransfer, nil
	}
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
		payload.AzureCloudNodeManagerPath:      K8sDir,
		payload.KubeLogRunnerPath:              K8sDir,
		payload.ContainerdPath:                 ContainerdDir,
		payload.HcsshimPath:                    ContainerdDir,
		payload.ContainerdConfPath:             ContainerdDir,
		payload.NetworkConfigurationScript:     remoteDir,
	}
	files := make(map[*payload.FileInfo]string)
	for src, dest := range srcDestPairs {
		f, err := payload.NewFileInfo(src)
		if err != nil {
			return nil, errors.Wrapf(err, "could not create FileInfo object for file %s", src)
		}
		files[f] = dest
	}
	filesToTransfer = files
	return filesToTransfer, nil
}

// GetK8sDir returns the location of the kubernetes executable directory
func GetK8sDir() string {
	return K8sDir
}

// Authentication holds credential information used to communicate with a kubernetes cluster
type Authentication struct {
	// CaCert is the PEM-encoded certificate authority certificate
	CaCert []byte
	// Token is the bearer Token used to grant access to the cluster API server
	Token []byte
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
	// Run executes the given command remotely on the Windows VM over a ssh connection and returns the combined output
	// of stdout and stderr. If the bool is set, it implies that the cmd is to be execute in PowerShell. This function
	// should be used in scenarios where you want to execute a command that runs in the background. In these cases we
	// have observed that Run() returns before the command completes and as a result killing the process.
	Run(string, bool) (string, error)
	// Reinitialize re-initializes the Windows VM's SSH client
	Reinitialize() error
	// Bootstrap prepares the Windows instance and runs the WICD bootstrap command
	Bootstrap(string, string, string, *Authentication) error
	// ConfigureWICD ensures that the Windows Instance Config Daemon is running on the node
	ConfigureWICD(string, string, *Authentication) error
	// Deconfigure removes all files and networks created by WMCO and runs the WICD cleanup command.
	Deconfigure(string, string) error
	// RunWICDCleanup runs the WICD cleanup command that ensures all services configured by WICD are stopped.
	RunWICDCleanup(string, string) error
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
}

// New returns a new Windows instance constructed from the given WindowsVM
func New(clusterDNS, vxlanPort string, instanceInfo *instance.Info, signer ssh.Signer) (Windows, error) {
	log := ctrl.Log.WithName(fmt.Sprintf("wc %s", instanceInfo.Address))
	log.V(1).Info("initializing SSH connection")
	conn, err := newSshConnectivity(instanceInfo.Username, instanceInfo.Address, signer, log)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to setup VM %s sshConnectivity", instanceInfo.Address)
	}

	return &windows{
			interact:               conn,
			clusterDNS:             clusterDNS,
			instance:               instanceInfo,
			log:                    log,
			defaultShellPowerShell: defaultShellPowershell(conn),
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
		return errors.Wrapf(err, "error checking if file '%s' exists on the Windows VM", remotePath)
	}
	if fileExists {
		// The file already exists with the expected content, do nothing
		return nil
	}
	vm.log.V(1).Info("copy", "file content", filename, "remote dir", remoteDir)
	if err := vm.interact.transfer(bytes.NewReader(contents), filename, remoteDir); err != nil {
		return errors.Wrapf(err, "unable to copy %s content to remote dir %s", filename, remoteDir)
	}
	return nil
}

func (vm *windows) EnsureFile(file *payload.FileInfo, remoteDir string) error {
	// Only copy the file to the Windows VM if it does not already exist wth the desired content
	remotePath := remoteDir + "\\" + filepath.Base(file.Path)
	fileExists, err := vm.FileExists(remotePath, file.SHA256)
	if err != nil {
		return errors.Wrapf(err, "error checking if file '%s' exists on the Windows VM", remotePath)
	}
	if fileExists {
		// The file already exists with the expected content, do nothing
		return nil
	}
	f, err := os.Open(file.Path)
	if err != nil {
		return errors.Wrapf(err, "error opening %s file to be transferred", file.Path)
	}
	defer func() {
		if err := f.Close(); err != nil {
			vm.log.Error(err, "error closing local file", "file", file.Path)
		}
	}()
	vm.log.V(1).Info("copy", "local file", file.Path, "remote dir", remoteDir)
	if err := vm.interact.transfer(f, filepath.Base(file.Path), remoteDir); err != nil {
		return errors.Wrapf(err, "unable to transfer %s to remote dir %s", file.Path, remoteDir)
	}
	return nil
}

func (vm *windows) FileExists(path, checksum string) (bool, error) {
	out, err := vm.Run("Test-Path "+path, true)
	if err != nil {
		return false, errors.Wrapf(err, "error checking if file %s exists", path)
	}
	found := strings.TrimSpace(out) == "True"
	// avoid checksum validation if not found or in lack of reference checksum
	if !found || checksum == "" {
		return found, nil
	}
	// file exist, compare checksum
	remoteFile, err := vm.newFileInfo(path)
	if err != nil {
		return false, errors.Wrapf(err, "error getting info on file '%s' on the Windows VM", path)
	}
	if remoteFile.SHA256 == checksum {
		vm.log.V(1).Info("file already exists on VM with expected content", "file", path)
		return true, nil
	}
	// file exist with diff content
	return false, nil
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
		// Hack to not print the error log for "sc.exe qc" returning 1060 for non existent services
		// and not print error when the command takes too long to return after removing HNS networks.
		if !(strings.Contains(cmd, serviceQueryCmd) && strings.Contains(out, serviceNotFound)) &&
			!(strings.Contains(err.Error(), cmdExitNoStatus) && strings.HasSuffix(cmd, removeHNSCommand+";\"")) {
			vm.log.Error(err, "error running", "cmd", cmd, "out", out)
		}
		return out, errors.Wrapf(err, "error running %s", cmd)
	}
	vm.log.V(1).Info("run", "cmd", cmd, "out", out)
	return out, nil
}

func (vm *windows) Reinitialize() error {
	if err := vm.interact.init(); err != nil {
		return fmt.Errorf("failed to reinitialize ssh client: %v", err)
	}
	return nil
}

func (vm *windows) RunWICDCleanup(apiServerURL string, watchNamespace string) error {
	wicdCleanupCmd := fmt.Sprintf("%s cleanup --api-server %s --sa-ca %s --sa-token %s --namespace %s",
		wicdPath, apiServerURL, wicdCAFile, wicdTokenFile, watchNamespace)
	if out, err := vm.Run(wicdCleanupCmd, true); err != nil {
		vm.log.Info("failed to cleanup node", "command", wicdCleanupCmd, "output", out)
		return err
	}
	return nil
}

func (vm *windows) Deconfigure(watchNamespace string, apiServerURL string) error {
	vm.log.Info("deconfiguring")

	if err := vm.deconfigureWICD(); err != nil {
		return err
	}
	if err := vm.RunWICDCleanup(apiServerURL, watchNamespace); err != nil {
		return errors.Wrap(err, "Unable to cleanup the Windows instance")
	}
	if err := vm.removeDirectories(); err != nil {
		return errors.Wrap(err, "unable to remove created directories")
	}
	if err := vm.ensureHNSNetworksAreRemoved(); err != nil {
		return errors.Wrap(err, "unable to ensure HNS networks are removed")
	}
	return nil
}

func (vm *windows) Bootstrap(desiredVer, apiServerURL, watchNamespace string, credentials *Authentication) error {
	vm.log.Info("configuring")

	// Make sure WICD service is not running before calling node cleanup and/or bootstrap
	if err := vm.deconfigureWICD(); err != nil {
		return err
	}
	// We have to ensure the WICD files exist separately before the rest of the files because we must ensure services
	// are stopped and their files closed before modifying them.
	if err := vm.ensureWICDExists(credentials); err != nil {
		return err
	}
	if err := vm.ensureWICDSecretContent(credentials); err != nil {
		return err
	}
	// Stop any services that may be running. This prevents the node being shown as Ready after a failed configuration.
	if err := vm.RunWICDCleanup(apiServerURL, watchNamespace); err != nil {
		return errors.Wrap(err, "Unable to cleanup the Windows instance")
	}

	if err := vm.ensureHostNameAndContainersFeature(); err != nil {
		return err
	}
	if err := vm.createDirectories(); err != nil {
		return errors.Wrap(err, "error creating directories on Windows VM")
	}
	if err := vm.transferFiles(); err != nil {
		return errors.Wrap(err, "error transferring files to Windows VM")
	}

	wicdBootstrapCmd := fmt.Sprintf("%s bootstrap --desired-version %s --api-server %s --sa-ca %s --sa-token %s --namespace %s",
		wicdPath, desiredVer, apiServerURL, wicdCAFile, wicdTokenFile, watchNamespace)
	if out, err := vm.Run(wicdBootstrapCmd, true); err != nil {
		vm.log.Info("failed to bootstrap node", "command", wicdBootstrapCmd, "output", out)
		return err
	}
	return nil
}

// ConfigureWICD starts the Windows Instance Config Daemon service
func (vm *windows) ConfigureWICD(apiServerURL, watchNamespace string, credentials *Authentication) error {
	if err := vm.ensureWICDSecretContent(credentials); err != nil {
		return err
	}
	wicdServiceArgs := fmt.Sprintf("controller --windows-service --log-dir %s --api-server %s --sa-ca %s --sa-token %s --namespace %s",
		wicdLogDir, apiServerURL, wicdCAFile, wicdTokenFile, watchNamespace)

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
		return errors.Wrapf(err, "error creating %s service object", WicdServiceName)
	}
	if err := vm.ensureServiceIsRunning(wicdService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", WicdServiceName)
	}
	vm.log.Info("configured", "service", WicdServiceName, "args", wicdServiceArgs)
	return nil
}

// Interface helper methods

// ensureWICDExists ensures the WICD executable exists. Creates the destination directory and binary file, if needed.
func (vm *windows) ensureWICDExists(credentials *Authentication) error {
	if _, err := vm.Run(mkdirCmd(K8sDir), false); err != nil {
		return errors.Wrapf(err, "unable to create remote directory %s", K8sDir)
	}
	wicdFileInfo, err := payload.NewFileInfo(payload.WICDPath)
	if err != nil {
		return errors.Wrapf(err, "could not create FileInfo object for file %s", payload.WICDPath)
	}
	if err := vm.EnsureFile(wicdFileInfo, K8sDir); err != nil {
		return errors.Wrapf(err, "error copying %s to %s ", wicdFileInfo.Path, K8sDir)
	}
	return nil
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
			return errors.Wrapf(err, "error enabling Windows Containers feature")
		}
		rebootNeeded = true
	}
	// Changing the host name or enabling the Containers feature requires a VM restart for
	// the change to take effect.
	if rebootNeeded {
		if err := vm.rebootAndReinitialize(); err != nil {
			return errors.Wrapf(err, "error restarting the Windows instance and reinitializing SSH connection")
		}
	}

	return nil
}

// isHostNameChangeNeeded tells if we need to update the host name of the Windows VM
func (vm *windows) isHostNameChangeNeeded() (bool, error) {
	out, err := vm.Run("hostname", true)
	if err != nil {
		return false, errors.Wrapf(err, "error getting the host name, with stdout %s", out)
	}
	return !strings.Contains(out, vm.instance.NewHostname), nil
}

// changeHostName changes the hostName of the Windows VM to match the expected value
func (vm *windows) changeHostName() error {
	changeHostNameCommand := "Rename-Computer -NewName " + vm.instance.NewHostname + " -Force"
	out, err := vm.Run(changeHostNameCommand, true)
	if err != nil {
		vm.log.Info("changing host name failed", "command", changeHostNameCommand, "output", out)
		return errors.Wrap(err, "changing host name failed")
	}
	return nil
}

// createDirectories creates directories required for configuring the Windows node on the VM
func (vm *windows) createDirectories() error {
	for _, dir := range RequiredDirectories {
		if _, err := vm.Run(mkdirCmd(dir), false); err != nil {
			return errors.Wrapf(err, "unable to create remote directory %s", dir)
		}
	}
	return nil
}

// removeDirectories removes all directories created as part of the configuration process
func (vm *windows) removeDirectories() error {
	vm.log.Info("removing directories")
	for _, dir := range RequiredDirectories {
		if out, err := vm.Run(rmDirCmd(dir), true); err != nil {
			return fmt.Errorf("unable to remove directory %s, out: %s, err: %s", dir, out, err)
		}
	}
	return nil
}

// transferFiles copies various files required for configuring the Windows node, to the VM.
func (vm *windows) transferFiles() error {
	vm.log.Info("transferring files")
	filesToTransfer, err := getFilesToTransfer()
	if err != nil {
		return errors.Wrapf(err, "error getting list of files to transfer")
	}
	for src, dest := range filesToTransfer {
		if err := vm.EnsureFile(src, dest); err != nil {
			return errors.Wrapf(err, "error copying %s to %s ", src.Path, dest)
		}
	}
	return nil
}

// ensureServiceIsRunning ensures a Windows service is running on the VM, creating and starting it if not already so.
// The service's description will be set as part of this.
func (vm *windows) ensureServiceIsRunning(svc *service) error {
	serviceExists, err := vm.serviceExists(svc.name)
	if err != nil {
		return errors.Wrapf(err, "error checking if %s Windows service exists", svc.name)
	}
	// create service if it does not exist
	if !serviceExists {
		if err := vm.createService(svc); err != nil {
			return errors.Wrapf(err, "error creating %s Windows service", svc.name)
		}
	}
	if err := vm.setServiceDescription(svc.name); err != nil {
		return errors.Wrapf(err, "error setting description of the %s Windows service", svc.name)
	}
	if err := vm.setRecoveryActions(svc); err != nil {
		return errors.Wrapf(err, "error setting recovery actions for the %s Windows service", svc.name)
	}
	if err := vm.startService(svc); err != nil {
		return errors.Wrapf(err, "error starting %s Windows service", svc.name)
	}
	return nil
}

// createService creates the service on the Windows VM
func (vm *windows) createService(svc *service) error {
	if svc == nil {
		return errors.New("service object should not be nil")
	}
	svcCreateCmd := fmt.Sprintf("sc.exe create %s binPath=\"%s %s\" start=auto", svc.name, svc.binaryPath,
		svc.args)
	if len(svc.dependencies) > 0 {
		dependencyList := strings.Join(svc.dependencies, "/")
		svcCreateCmd += " depend=" + dependencyList
	}

	_, err := vm.Run(svcCreateCmd, false)
	if err != nil {
		return errors.Wrapf(err, "failed to create service %s", svc.name)
	}
	return nil
}

// setServiceDescription sets the given service's description to the expected value. This can only be done after service
// creation.
func (vm *windows) setServiceDescription(svcName string) error {
	cmd := fmt.Sprintf("sc.exe description %s \"%s %s\"", svcName, ManagedTag, svcName)
	out, err := vm.Run(cmd, false)
	if err != nil {
		return errors.Wrapf(err, "failed to set service description with stdout %s", out)
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
		return errors.Wrapf(err, "failed to set recovery actions with stdout: %s", out)
	}
	return nil
}

// ensureServiceNotRunning stops a service if it exists and is running
func (vm *windows) ensureServiceNotRunning(svc *service) error {
	if svc == nil {
		return errors.New("service object should not be nil")
	}

	exists, err := vm.serviceExists(svc.name)
	if err != nil {
		return errors.Wrap(err, "error checking if service exists")
	}
	if !exists {
		// service does not exist, therefore it is not running
		return nil
	}

	running, err := vm.isRunning(svc.name)
	if err != nil {
		return errors.Wrap(err, "unable to check if service is running")
	}
	if !running {
		return nil
	}
	if err := vm.stopService(svc); err != nil {
		return errors.Wrap(err, "unable to stop service")
	}
	return nil

}

// ensureServiceIsRemoved ensures that the given service installed by WMCO is removed from the instance
func (vm *windows) ensureServiceIsRemoved(svcName string) error {
	svc := &service{name: svcName}
	// If the service is not installed, do nothing
	exists, err := vm.serviceExists(svcName)
	if err != nil {
		return errors.Wrapf(err, "error checking if %s Windows service exists", svc.name)
	}
	if !exists {
		return nil
	}
	// Make sure the service is stopped before we attempt to delete it
	if err := vm.ensureServiceNotRunning(svc); err != nil {
		return errors.Wrapf(err, "error stopping %s Windows service", svc.name)
	}
	if err := vm.deleteService(svc); err != nil {
		return errors.Wrapf(err, "error deleting %s Windows service", svc.name)
	}
	vm.log.Info("deconfigured", "service", svc.name)
	return nil
}

// stopService stops the service that was already running
func (vm *windows) stopService(svc *service) error {
	if svc == nil {
		return errors.New("service object should not be nil")
	}
	// Success here means that the stop has initiated, not necessarily completed
	out, err := vm.Run("sc.exe stop "+svc.name, false)
	if err != nil {
		return errors.Wrapf(err, "failed to stop %s service with output: %s", svc.name, out)
	}

	// Wait until the service has stopped
	err = wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		serviceRunning, err := vm.isRunning(svc.name)
		if err != nil {
			vm.log.V(1).Error(err, "unable to check if Windows service is running", "service", svc.name)
			return false, nil
		}
		return !serviceRunning, nil
	})
	if err != nil {
		return errors.Wrapf(err, "error waiting for the %s service to stop", svc.name)
	}

	return nil
}

// deleteService deletes the specified Windows service
func (vm *windows) deleteService(svc *service) error {
	if svc == nil {
		return errors.New("service object cannot be nil")
	}

	// Success here means that the stop has initiated, not necessarily completed
	out, err := vm.Run("sc.exe delete "+svc.name, false)
	if err != nil {
		return errors.Wrapf(err, "failed to delete %s service with output: %s", svc.name, out)
	}

	// Wait until the service is fully deleted
	err = wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
		exists, err := vm.serviceExists(svc.name)
		if err != nil {
			vm.log.V(1).Error(err, "unable to check if Windows service exists", "service", svc.name)
			return false, nil
		}
		return !exists, nil
	})
	if err != nil {
		return errors.Wrapf(err, "error waiting for the %s service to be deleted", svc.name)
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
		return errors.New("service object should not be nil")
	}
	serviceRunning, err := vm.isRunning(svc.name)
	if err != nil {
		return errors.Wrapf(err, "unable to check if %s Windows service is running", svc.name)
	}
	if serviceRunning {
		return nil
	}
	out, err := vm.Run("sc.exe start "+svc.name, false)
	if err != nil {
		return errors.Wrapf(err, "failed to start %s service with output: %s", svc.name, out)
	}
	return nil
}

// newFileInfo returns a pointer to a FileInfo object created from the specified file on the Windows VM
func (vm *windows) newFileInfo(path string) (*payload.FileInfo, error) {
	// Get-FileHash returns an object with multiple properties, we are interested in the `Hash` property
	command := "$out = Get-FileHash " + path + " -Algorithm SHA256; $out.Hash"
	out, err := vm.Run(command, true)
	if err != nil {
		return nil, errors.Wrap(err, "error getting file hash")
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
		err = wait.Poll(retry.Interval, retry.Timeout, func() (bool, error) {
			// reinitialize and retry on failure to avoid connection reset SSH errors
			if err := vm.removeHNSNetwork(network); err != nil {
				vm.log.V(1).Error(err, "error removing %s HNS network", "network", network)
				if err := vm.Reinitialize(); err != nil {
					return false, errors.Wrapf(err, "error reinitializing VM after removing %s HNS network", network)
				}
				return false, nil
			}
			if err := vm.Reinitialize(); err != nil {
				return false, errors.Wrapf(err, "error reinitializing VM after removing %s HNS network", network)
			}
			out, err := vm.Run(getHNSNetworkCmd(network), true)
			if err != nil {
				vm.log.V(1).Error(err, "error waiting for HNS network", "network", network)
				return false, nil
			}
			return !strings.Contains(out, network), nil
		})
		if err != nil {
			return errors.Wrapf(err, "failed ensuring %s HNS network is removed", network)
		}
	}
	return nil
}

// removeHNSNetwork removes the given HNS network.
func (vm *windows) removeHNSNetwork(networkName string) error {
	cmd := getHNSNetworkCmd(networkName) + " | Remove-HnsNetwork;"
	// PowerShell returns error waiting without exit status or signal error when the networks are removed.
	if out, err := vm.Run(cmd, true); err != nil && !strings.Contains(err.Error(), cmdExitNoStatus) {
		return errors.Wrapf(err, "failed to remove %s HNS network with output: %s", networkName, out)
	}
	return nil
}

// enableContainersWindowsFeature enables the required Windows Containers feature on the Windows instance.
func (vm *windows) enableContainersWindowsFeature() error {
	command := "Install-WindowsFeature -Name " + containersFeatureName
	out, err := vm.Run(command, true)
	if err != nil {
		return errors.Wrapf(err, "failed to enable required Windows feature: %s with output: %s",
			containersFeatureName, out)
	}
	return nil
}

// isContainersFeatureEnabled returns true if the required Containers Windows feature is enabled on the Windows instance
func (vm *windows) isContainersFeatureEnabled() (bool, error) {
	command := "Get-WindowsOptionalFeature -FeatureName " + containersFeatureName + " -Online"
	out, err := vm.Run(command, true)
	if err != nil {
		return false, errors.Wrapf(err, "failed to get Windows feature: %s", containersFeatureName)
	}
	return strings.Contains(out, "Enabled"), nil
}

// rebootAndReinitialize restarts the Windows instance and re-initializes the SSH connection for further configuration
func (vm *windows) rebootAndReinitialize() error {
	if _, err := vm.Run("Restart-Computer -Force", true); err != nil {
		return errors.Wrapf(err, "error rebooting the Windows VM")
	}
	// Reinitialize the SSH connection after the VM reboot
	if err := vm.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing SSH connection after VM reboot")
	}
	return nil
}

// ensureWICDSecretContent checks if the WICD CA cert and token files on the instance hold the expected values
func (vm *windows) ensureWICDSecretContent(credentials *Authentication) error {
	caDir, caFileName := SplitPath(wicdCAFile)
	err := vm.EnsureFileContent(credentials.CaCert, caFileName, caDir)
	if err != nil {
		return err
	}
	tokenDir, tokenFileName := SplitPath(wicdTokenFile)
	return vm.EnsureFileContent(credentials.Token, tokenFileName, tokenDir)
}

// deconfigureWICD ensures the WICD service running on the Windows instance is removed
func (vm *windows) deconfigureWICD() error {
	if err := vm.ensureServiceIsRemoved(WicdServiceName); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service is removed", WicdServiceName)
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
