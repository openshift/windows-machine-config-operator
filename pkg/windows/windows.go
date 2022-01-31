package windows

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	remoteDir = "C:\\Temp\\"
	// winTemp is the default Windows temporary directory
	winTemp = "C:\\Windows\\Temp\\"
	// wgetIgnoreCertCmd is the remote location of the wget-ignore-cert.ps1 script
	wgetIgnoreCertCmd = remoteDir + "wget-ignore-cert.ps1"
	// hnsPSModule is the remote location of the hns.psm1 module
	hnsPSModule = remoteDir + "hns.psm1"
	// k8sDir is the remote kubernetes executable directory
	k8sDir = "C:\\k\\"
	// logDir is the remote kubernetes log directory
	logDir = "C:\\var\\log\\"
	// kubeProxyLogDir is the remote kube-proxy log directory
	kubeProxyLogDir = logDir + "kube-proxy\\"
	// hybridOverlayLogDir is the remote hybrid-overlay log directory
	hybridOverlayLogDir = logDir + "hybrid-overlay\\"
	// cniDir is the directory for storing CNI binaries
	cniDir = k8sDir + "cni\\"
	// cniConfDir is the directory for storing CNI configuration
	cniConfDir = cniDir + "config\\"
	// ContainerdDir is the directory for storing Containerd binary
	ContainerdDir = k8sDir + "containerd\\"
	// windowsExporterPath is the location of the windows_exporter.exe
	windowsExporterPath = k8sDir + "windows_exporter.exe"
	// azureCloudNodeManagerPath is the location of the azure-cloud-node-manager.exe
	azureCloudNodeManagerPath = k8sDir + payload.AzureCloudNodeManager
	// kubeProxyPath is the location of the kube-proxy exe
	kubeProxyPath = k8sDir + "kube-proxy.exe"
	// hybridOverlayPath is the location of the hybrid-overlay-node exe
	hybridOverlayPath = k8sDir + "hybrid-overlay-node.exe"

	// hybridOverlayServiceName is the name of the hybrid-overlay-node Windows service
	hybridOverlayServiceName = "hybrid-overlay-node"
	// hybridOverlayConfigurationTime is the approximate time taken for the hybrid-overlay to complete reconfiguring
	// the Windows VM's network
	hybridOverlayConfigurationTime = 2 * time.Minute
	// BaseOVNKubeOverlayNetwork is the name of base OVN HNS Overlay network
	BaseOVNKubeOverlayNetwork = "BaseOVNKubernetesHybridOverlayNetwork"
	// OVNKubeOverlayNetwork is the name of the OVN HNS Overlay network
	OVNKubeOverlayNetwork = "OVNKubernetesHybridOverlayNetwork"
	// kubeProxyServiceName is the name of the kube-proxy Windows service
	kubeProxyServiceName = "kube-proxy"
	// kubeletServiceName is the name of the kubelet Windows service
	kubeletServiceName = "kubelet"
	// windowsExporterServiceName is the name of the windows_exporter Windows service
	windowsExporterServiceName = "windows_exporter"
	// AzureCloudNodeManagerServiceName is the name of the azure cloud node manager service
	AzureCloudNodeManagerServiceName = "cloud-node-manager"
	// windowsExporterServiceArgs specifies metrics for the windows_exporter service to collect
	// and expose metrics at endpoint with default port :9182 and default URL path /metrics
	windowsExporterServiceArgs = "--collectors.enabled " +
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
)

var (
	// filesToTransfer is a map of what files should be copied to the Windows VM and where they should be copied to
	filesToTransfer map[*payload.FileInfo]string
	// RequiredServices is a list of Windows services installed by WMCO
	// The order of this slice matters due to service dependencies. If a service depends on another service, the
	// dependent service should be placed before the service it depends on.
	RequiredServices = []string{
		windowsExporterServiceName,
		kubeProxyServiceName,
		hybridOverlayServiceName,
		kubeletServiceName}
	// RequiredDirectories is a list of directories to be created by WMCO
	RequiredDirectories = []string{
		k8sDir,
		remoteDir,
		cniDir,
		cniConfDir,
		logDir,
		kubeProxyLogDir,
		hybridOverlayLogDir,
		ContainerdDir}
)

// getFilesToTransfer returns the properly populated filesToTransfer map
func getFilesToTransfer() (map[*payload.FileInfo]string, error) {
	if filesToTransfer != nil {
		return filesToTransfer, nil
	}
	srcDestPairs := map[string]string{
		payload.IgnoreWgetPowerShellPath:  remoteDir,
		payload.WmcbPath:                  k8sDir,
		payload.HybridOverlayPath:         k8sDir,
		payload.HNSPSModule:               remoteDir,
		payload.WindowsExporterPath:       k8sDir,
		payload.WinBridgeCNIPlugin:        cniDir,
		payload.HostLocalCNIPlugin:        cniDir,
		payload.WinOverlayCNIPlugin:       cniDir,
		payload.KubeProxyPath:             k8sDir,
		payload.KubeletPath:               k8sDir,
		payload.AzureCloudNodeManagerPath: k8sDir,
		payload.ContainerdPath:            ContainerdDir,
		payload.HcsshimPath:               ContainerdDir,
		payload.ContainerdConfPath:        ContainerdDir,
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
	return k8sDir
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
	// Configure prepares the Windows VM for the bootstrapper and then runs it
	Configure() error
	// ConfigureCNI ensures that the CNI configuration in done on the node
	ConfigureCNI(string) error
	// ConfigureHybridOverlay ensures that the hybrid overlay is running on the node
	ConfigureHybridOverlay(string) error
	// ConfigureWindowsExporter ensures that the Windows metrics exporter is running on the node
	ConfigureWindowsExporter() error
	// ConfigureKubeProxy ensures that the kube-proxy service is running
	ConfigureKubeProxy(string, string) error
	// ConfigureAzureCloudNodeManager ensures that the azure-cloud-node-manager service is running
	ConfigureAzureCloudNodeManager(string) error
	// EnsureRequiredServicesStopped ensures that all services that are needed to configure a VM are stopped
	EnsureRequiredServicesStopped() error
	// Deconfigure removes all files and services created as part of the configuration process
	Deconfigure() error
}

// windows implements the Windows interface
type windows struct {
	// workerIgnitionEndpoint is the Machine Config Server(MCS) endpoint from which we can download the
	// the OpenShift worker ignition file.
	workerIgnitionEndpoint string
	// clusterDNS is the IP address of the DNS server used for all containers
	clusterDNS string
	// signer is used for authenticating against the VM
	signer ssh.Signer
	// interact is used to connect to and interact with the VM
	interact connectivity
	// vxlanPort is the custom VXLAN port
	vxlanPort string
	// instance contains information about the Windows instance to interact with
	// A valid instance is configured with a network address that either is an IPv4 address or resolves to one.
	instance *instance.Info
	log      logr.Logger
	// defaultShellPowerShell indicates if the default SSH shell is PowerShell
	defaultShellPowerShell bool
	// platformType overrides default hostname in bootstrapper
	platformType string
}

// New returns a new Windows instance constructed from the given WindowsVM
func New(workerIgnitionEndpoint, clusterDNS, vxlanPort string, instanceInfo *instance.Info, signer ssh.Signer,
	platformType string) (Windows, error) {
	log := ctrl.Log.WithName(fmt.Sprintf("wc %s", instanceInfo.Address))
	log.V(1).Info("initializing SSH connection")
	conn, err := newSshConnectivity(instanceInfo.Username, instanceInfo.Address, signer, log)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to setup VM %s sshConnectivity", instanceInfo.Address)
	}

	return &windows{
			interact:               conn,
			workerIgnitionEndpoint: workerIgnitionEndpoint,
			clusterDNS:             clusterDNS,
			vxlanPort:              vxlanPort,
			instance:               instanceInfo,
			log:                    log,
			defaultShellPowerShell: defaultShellPowershell(conn),
			platformType:           platformType,
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

func (vm *windows) EnsureRequiredServicesStopped() error {
	for _, svcName := range append(RequiredServices, AzureCloudNodeManagerServiceName) {
		svc := &service{name: svcName}
		if err := vm.ensureServiceNotRunning(svc); err != nil {
			return errors.Wrapf(err, "could not stop service %s", svcName)
		}
	}
	return nil
}

// ensureServicesAreRemoved ensures that all services installed by WMCO are removed from the instance
func (vm *windows) ensureServicesAreRemoved() error {
	for _, svcName := range append(RequiredServices, AzureCloudNodeManagerServiceName) {
		svc := &service{name: svcName}

		// If the service is not installed, do nothing
		exists, err := vm.serviceExists(svc.name)
		if err != nil {
			return errors.Wrapf(err, "unable to check if %s service exists", svc.name)
		}
		if !exists {
			continue
		}

		// Make sure the service is stopped before we attempt to delete it
		if err := vm.ensureServiceNotRunning(svc); err != nil {
			return errors.Wrapf(err, "could not stop service %s", svc.name)
		}
		if err := vm.deleteService(svc); err != nil {
			return errors.Wrapf(err, "could not delete service %s", svcName)
		}
		vm.log.Info("deconfigured", "service", svc.name)
	}
	return nil
}

func (vm *windows) Deconfigure() error {
	vm.log.Info("deconfiguring")
	if err := vm.ensureServicesAreRemoved(); err != nil {
		return errors.Wrap(err, "unable to remove Windows services")
	}
	if err := vm.removeDirectories(); err != nil {
		return errors.Wrap(err, "unable to remove created directories")
	}
	if err := vm.ensureHNSNetworksAreRemoved(); err != nil {
		return errors.Wrap(err, "unable to ensure HNS networks are removed")
	}
	return nil
}

func (vm *windows) Configure() error {
	vm.log.Info("configuring")
	if err := vm.EnsureRequiredServicesStopped(); err != nil {
		return errors.Wrap(err, "unable to stop all services")
	}
	// Set the hostName of the Windows VM if needed
	if vm.instance.NewHostname != "" {
		if err := vm.ensureHostName(); err != nil {
			return err
		}
	}
	if err := vm.createDirectories(); err != nil {
		return errors.Wrap(err, "error creating directories on Windows VM")
	}
	if err := vm.transferFiles(); err != nil {
		return errors.Wrap(err, "error transferring files to Windows VM")
	}
	if err := vm.ConfigureWindowsExporter(); err != nil {
		return errors.Wrapf(err, "error configuring Windows exporter")
	}

	return vm.runBootstrapper()
}

// ConfigureWindowsExporter starts Windows metrics exporter service, only if the file is present on the VM
func (vm *windows) ConfigureWindowsExporter() error {
	windowsExporterService, err := newService(windowsExporterPath, windowsExporterServiceName,
		windowsExporterServiceArgs, nil)
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", windowsExporterServiceName)
	}

	if err := vm.ensureServiceIsRunning(windowsExporterService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", windowsExporterServiceName)
	}

	vm.log.Info("configured", "service", windowsExporterServiceName, "args",
		windowsExporterServiceArgs)
	return nil
}

func (vm *windows) ConfigureHybridOverlay(nodeName string) error {
	var customVxlanPortArg = ""
	if len(vm.vxlanPort) > 0 {
		customVxlanPortArg = " --hybrid-overlay-vxlan-port=" + vm.vxlanPort
	}

	hybridOverlayServiceArgs := "--node " + nodeName + customVxlanPortArg + " --k8s-kubeconfig c:\\k\\kubeconfig " +
		"--windows-service " + "--logfile " + hybridOverlayLogDir + "hybrid-overlay.log"

	vm.log.Info("configure", "service", hybridOverlayServiceName, "args", hybridOverlayServiceArgs)

	hybridOverlayService, err := newService(hybridOverlayPath, hybridOverlayServiceName, hybridOverlayServiceArgs,
		[]string{kubeletServiceName})
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", hybridOverlayServiceName)
	}

	if err := vm.ensureServiceIsRunning(hybridOverlayService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", hybridOverlayServiceName)
	}

	if err = vm.waitForServiceToRun(hybridOverlayServiceName); err != nil {
		return errors.Wrapf(err, "error running %s Windows service", hybridOverlayServiceName)
	}
	// Wait for the hybrid-overlay to complete reconfiguring the network. The only way to detect that it has completed
	// the reconfiguration is to check for the HNS networks but doing that without reinitializing the WinRM client
	// results in 5+ minutes wait times for the vm.Run() call to complete. So the only alternative is to wait before
	// proceeding.
	time.Sleep(hybridOverlayConfigurationTime)

	// Running the hybrid-overlay causes network reconfiguration in the Windows VM which results in the ssh connection
	// being closed and the client is not smart enough to reconnect. We have observed that the WinRM connection does not
	// get closed and does not need reinitialization.
	if err = vm.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing VM after running hybrid-overlay")
	}

	if err = vm.waitForHNSNetworks(); err != nil {
		return errors.Wrap(err, "error waiting for OVN HNS networks to be created")
	}

	vm.log.Info("configured", "service", hybridOverlayServiceName, "args", hybridOverlayServiceArgs)
	return nil
}

func (vm *windows) ConfigureCNI(configFile string) error {
	// copy the CNI config file to the Windows VM
	file, err := payload.NewFileInfo(configFile)
	if err != nil {
		return errors.Wrap(err, "unable to get info for the CNI config file")
	}
	if err := vm.EnsureFile(file, cniConfDir); err != nil {
		return errors.Errorf("unable to copy CNI file %s to %s", configFile, cniConfDir)
	}

	cniConfigDest := cniConfDir + filepath.Base(configFile)
	// run the configure-cni command on the Windows VM
	configureCNICmd := k8sDir + "wmcb.exe configure-cni --cni-dir=" + cniDir + " --cni-config=" + cniConfigDest

	out, err := vm.Run(configureCNICmd, true)
	if err != nil {
		return errors.Wrap(err, "CNI configuration failed")
	}

	vm.log.Info("configured kubelet for CNI", "cmd", configureCNICmd, "output", out)
	return nil
}

func (vm *windows) ConfigureAzureCloudNodeManager(nodeName string) error {
	azureCloudNodeManagerServiceArgs := "--windows-service --node-name=" + nodeName + " --wait-routes=false --kubeconfig c:\\k\\kubeconfig"

	azureCloudNodeManagerService, err := newService(
		azureCloudNodeManagerPath,
		AzureCloudNodeManagerServiceName,
		azureCloudNodeManagerServiceArgs,
		nil)
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", AzureCloudNodeManagerServiceName)
	}

	if err := vm.ensureServiceIsRunning(azureCloudNodeManagerService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", AzureCloudNodeManagerServiceName)
	}
	vm.log.Info("configured", "service", AzureCloudNodeManagerServiceName, "args", azureCloudNodeManagerServiceArgs)
	return nil
}

func (vm *windows) ConfigureKubeProxy(nodeName, hostSubnet string) error {
	endpointIP, err := vm.createHNSEndpoint()
	if err != nil {
		return errors.Wrap(err, "error creating HNS endpoint")
	}

	kubeProxyServiceArgs := "--windows-service --v=4 --proxy-mode=kernelspace --feature-gates=WinOverlay=true " +
		"--hostname-override=" + nodeName + " --kubeconfig=c:\\k\\kubeconfig " +
		"--cluster-cidr=" + hostSubnet + " --log-dir=" + kubeProxyLogDir + " --logtostderr=false " +
		"--network-name=" + OVNKubeOverlayNetwork + " --source-vip=" + endpointIP +
		" --enable-dsr=false"

	kubeProxyService, err := newService(kubeProxyPath, kubeProxyServiceName, kubeProxyServiceArgs,
		[]string{hybridOverlayServiceName})
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", kubeProxyServiceName)
	}

	if err := vm.ensureServiceIsRunning(kubeProxyService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", kubeProxyServiceName)
	}
	vm.log.Info("configured", "service", kubeProxyServiceName, "args", kubeProxyServiceArgs)
	return nil
}

// Interface helper methods

// ensureHostName ensures hostname of the Windows VM matches the expected name
func (vm *windows) ensureHostName() error {
	hostNameChangedNeeded, err := vm.isHostNameChangeNeeded()
	if err != nil {
		return err
	}
	if hostNameChangedNeeded {
		if err := vm.changeHostName(); err != nil {
			return err
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
	changeHostNameCommand := "Rename-Computer -NewName " + vm.instance.NewHostname + " -Force -Restart"
	out, err := vm.Run(changeHostNameCommand, true)
	if err != nil {
		vm.log.Info("changing host name failed", "command", changeHostNameCommand, "output", out)
		return errors.Wrap(err, "changing host name failed")
	}
	// Reinitialize the SSH connection given changing the host name requires a VM restart
	if err := vm.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing VM after changing hostname")
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
		if _, err := vm.Run(rmDirCmd(dir), false); err != nil {
			return errors.Wrapf(err, "unable to remove directory %s", dir)
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

// runBootstrapper copies the bootstrapper and runs the code on the remote Windows VM
func (vm *windows) runBootstrapper() error {
	err := vm.initializeBootstrapperFiles()
	if err != nil {
		return errors.Wrap(err, "error initializing bootstrapper files")
	}
	wmcbInitializeCmd := k8sDir + "\\wmcb.exe initialize-kubelet --ignition-file " + winTemp +
		"worker.ign --kubelet-path " + k8sDir + "kubelet.exe"
	if vm.instance.SetNodeIP {
		wmcbInitializeCmd += " --node-ip=" + vm.GetIPv4Address()
	}
	if vm.clusterDNS != "" {
		wmcbInitializeCmd += " --cluster-dns " + vm.clusterDNS
	}
	wmcbInitializeCmd += " --platform-type=" + vm.platformType

	out, err := vm.Run(wmcbInitializeCmd, true)
	vm.log.Info("configured kubelet", "cmd", wmcbInitializeCmd, "output", out)
	if err != nil {
		return errors.Wrap(err, "error running bootstrapper")
	}
	return nil
}

// initializeTestBootstrapperFiles initializes the files required for initialize-kubelet
func (vm *windows) initializeBootstrapperFiles() error {
	if vm.workerIgnitionEndpoint == "" {
		return errors.New("cannot use empty ignition endpoint")
	}
	// Ignition v2.3.0 maps to Ignition config spec v3.1.0.
	ignitionAcceptHeaderSpec := "application/vnd.coreos.ignition+json`;version=3.1.0"
	// Download the worker ignition to C:\Windows\Temp\ using the script that ignores the server cert
	ignitionFileDownloadCmd := wgetIgnoreCertCmd + " -server " + vm.workerIgnitionEndpoint + " -output " +
		winTemp + "worker.ign" + " -acceptHeader " + ignitionAcceptHeaderSpec
	_, err := vm.Run(ignitionFileDownloadCmd, true)
	if err != nil {
		return errors.Wrap(err, "unable to download worker.ign")
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

// waitForHNSNetworks waits for the OVN overlay HNS networks to be created until the timeout is reached
func (vm *windows) waitForHNSNetworks() error {
	var out string
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		out, err = vm.Run("Get-HnsNetwork", true)
		if err != nil {
			// retry
			continue
		}

		if strings.Contains(out, BaseOVNKubeOverlayNetwork) &&
			strings.Contains(out, OVNKubeOverlayNetwork) {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// OVN overlay HNS networks were not found
	vm.log.Info("Get-HnsNetwork", "output", out)
	return errors.Wrap(err, "timeout waiting for OVN overlay HNS networks")
}

// waitForServiceToRun waits for the given service to be in RUNNING state
// until the timeout is reached
func (vm *windows) waitForServiceToRun(serviceName string) error {
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		serviceRunning, err := vm.isRunning(serviceName)
		if err != nil {
			return errors.Wrapf(err, "unable to check if %s Windows service is running", serviceName)
		}
		if serviceRunning {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// service did not reach running state
	return fmt.Errorf("timeout waiting for %s service to be in running state: %v", serviceName, err)
}

// createHNSEndpoint makes a request to create a new HNS endpoint for the Hybrid Overlay network on the VM. On success,
// the IP of the endpoint will be returned.
func (vm *windows) createHNSEndpoint() (string, error) {
	cmd := "Import-Module -DisableNameChecking " + hnsPSModule + "; " +
		"$net = (Get-HnsNetwork | where { $_.Name -eq '" + OVNKubeOverlayNetwork + "' }); " +
		"$endpoint = New-HnsEndpoint -NetworkId $net.ID -Name VIPEndpoint; " +
		"Attach-HNSHostEndpoint -EndpointID $endpoint.ID -CompartmentID 1; " +
		"(Get-NetIPConfiguration -AllCompartments -All -Detailed | " +
		"where { $_.NetAdapter.LinkLayerAddress -eq $endpoint.MacAddress }).IPV4Address.IPAddress.Trim()"
	out, err := vm.Run(cmd, true)
	if err != nil {
		return "", errors.Wrap(err, "failed to get source VIP")
	}

	// stdout will have trailing '\r\n', so need to trim it
	endpointIP := strings.TrimSpace(out)
	if endpointIP == "" {
		return "", fmt.Errorf("expected HNS endpoint IP is missing")
	}
	return endpointIP, nil
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
			if err := vm.removeHNSNetwork(network); err != nil {
				return false, errors.Wrapf(err, "error removing %s HNS network", network)
			}
			if err := vm.Reinitialize(); err != nil {
				return false, errors.Wrapf(err, "error reinitializing VM after removing %s HNS network", network)
			}
			out, err := vm.Run(getHNSNetworkCmd(network), true)
			if err != nil {
				return false, errors.Wrapf(err, "error waiting for %s HNS network", network)
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
	// PowerShell returns error waiting without exit status or signal error when the OVNKubeOverlayNetwork is removed.
	if out, err := vm.Run(cmd, true); err != nil && !(networkName == OVNKubeOverlayNetwork && strings.Contains(err.Error(), cmdExitNoStatus)) {
		return errors.Wrapf(err, "failed to remove %s HNS network with output: %s", networkName, out)
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

// rmDirCmd returns the Windows command to recursively remove a directory if it exists
func rmDirCmd(dirName string) string {
	return fmt.Sprintf("if exist %s rmdir %s /s /q", dirName, dirName)
}

// getHNSNetworkCmd returns the Windows command to get HNS network by name
func getHNSNetworkCmd(networkName string) string {
	return "Get-HnsNetwork | where { $_.Name -eq '" + networkName + "'}"
}
