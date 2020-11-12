package windows

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/payload"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
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
	// windowsExporterPath is the location of the windows_exporter.exe
	windowsExporterPath = k8sDir + "windows_exporter.exe"
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
	// windowsExporterServiceArgs specifies metrics for the windows_exporter service to collect
	// and expose metrics at endpoint with default port :9182 and default URL path /metrics
	windowsExporterServiceArgs = "--collectors.enabled " +
		"cpu,cs,logical_disk,net,os,service,system,textfile,container,memory\""
	// remotePowerShellCmdPrefix holds the PowerShell prefix that needs to be prefixed  for every remote PowerShell
	// command executed on the remote Windows VM
	remotePowerShellCmdPrefix = "powershell.exe -NonInteractive -ExecutionPolicy Bypass "
)

var log = logf.Log.WithName("windows")

// filesToTransfer is a map of what files should be copied to the Windows VM and where they should be copied to
var filesToTransfer map[*payload.FileInfo]string

// getFilesToTransfer returns the properly populated filesToTransfer map
func getFilesToTransfer() (map[*payload.FileInfo]string, error) {
	if filesToTransfer != nil {
		return filesToTransfer, nil
	}
	srcDestPairs := map[string]string{
		payload.IgnoreWgetPowerShellPath: remoteDir,
		payload.WmcbPath:                 k8sDir,
		payload.HybridOverlayPath:        k8sDir,
		payload.HNSPSModule:              remoteDir,
		payload.WindowsExporterPath:      k8sDir,
		payload.FlannelCNIPluginPath:     cniDir,
		payload.WinBridgeCNIPlugin:       cniDir,
		payload.HostLocalCNIPlugin:       cniDir,
		payload.WinOverlayCNIPlugin:      cniDir,
		payload.KubeProxyPath:            k8sDir,
		payload.KubeletPath:              k8sDir,
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

// Windows contains all the  methods needed to configure a Windows VM to become a worker node
type Windows interface {
	// ID returns the cloud provider ID of the VM
	ID() string
	// EnsureFile ensures the given file exists within the specified directory on the Windows VM. The file will be copied
	// to the Windows VM if it is not present or if it has the incorrect contents. The remote directory is created if it
	// does not exist.
	EnsureFile(*payload.FileInfo, string) error
	// FileExists returns true if a specific file exists at the given path on the Windows VM
	FileExists(string) (bool, error)
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
}

// windows implements the Windows interface
type windows struct {
	// ipAddress is the IP address associated with the Windows VM created
	ipAddress string
	// id is the VM's cloud provider ID
	id string
	// workerIgnitionEndpoint is the Machine Config Server(MCS) endpoint from which we can download the
	// the OpenShift worker ignition file.
	workerIgnitionEndpoint string
	// signer is used for authenticating against the VM
	signer ssh.Signer
	// interact is used to connect to and interact with the VM
	interact connectivity
	// vxlanPort is the custom VXLAN port
	vxlanPort string
}

// New returns a new Windows instance constructed from the given WindowsVM
func New(ipAddress, providerName, instanceID, workerIgnitionEndpoint, vxlanPort string, signer ssh.Signer) (Windows, error) {
	if workerIgnitionEndpoint == "" {
		return nil, errors.New("cannot use empty ignition endpoint")
	}

	// TODO: This should be changed so that the "core" user is used on all platforms for SSH connections.
	// https://issues.redhat.com/browse/WINC-430
	var adminUser string
	if providerName == "azure" {
		adminUser = "capi"
	} else {
		adminUser = "Administrator"
	}

	// Update the logger name with the VM's cloud ID
	log = logf.Log.WithName(fmt.Sprintf("VM %s", instanceID))

	log.V(1).Info("initializing SSH connection", "user", adminUser)
	conn, err := newSshConnectivity(adminUser, ipAddress, signer)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to setup VM %s sshConnectivity", instanceID)
	}

	return &windows{
			id:                     instanceID,
			interact:               conn,
			workerIgnitionEndpoint: workerIgnitionEndpoint,
			vxlanPort:              vxlanPort,
		},
		nil
}

// Interface methods

func (vm *windows) ID() string {
	return vm.id
}

func (vm *windows) EnsureFile(file *payload.FileInfo, remoteDir string) error {
	// Only copy the file to the Windows VM if it does not already exist wth the desired content
	remotePath := remoteDir + "\\" + filepath.Base(file.Path)
	fileExists, err := vm.FileExists(remotePath)
	if err != nil {
		return errors.Wrapf(err, "error checking if file '%s' exists on the Windows VM", remotePath)
	}
	if fileExists {
		remoteFile, err := vm.newFileInfo(remotePath)
		if err != nil {
			return errors.Wrapf(err, "error getting info on file '%s' on the Windows VM", remotePath)
		}
		if file.SHA256 == remoteFile.SHA256 {
			// The file already exists with the expected content, do nothing
			log.V(1).Info("file already exists on VM with expected content", "file", file.Path)
			return nil
		}
	}

	log.V(1).Info("copy", "local file", file.Path, "remote dir", remoteDir)
	if err := vm.interact.transfer(file.Path, remoteDir); err != nil {
		return errors.Wrapf(err, "unable to transfer %s to remote dir %s", file.Path, remoteDir)
	}
	return nil
}

func (vm *windows) FileExists(path string) (bool, error) {
	out, err := vm.Run("Test-Path "+path, true)
	if err != nil {
		return false, errors.Wrapf(err, "error checking if file %s exists on Windows VM %s", path, vm.ID())
	}
	return strings.TrimSpace(out) == "True", nil
}

func (vm *windows) Run(cmd string, psCmd bool) (string, error) {
	if psCmd {
		cmd = remotePowerShellCmdPrefix + cmd
	}

	out, err := vm.interact.run(cmd)
	if err != nil {
		log.Error(err, "error running", "cmd", cmd, "out", out)
		return out, errors.Wrapf(err, "error running %s", cmd)
	}
	log.V(1).Info("run", "cmd", cmd, "out", out)
	return out, nil
}

func (vm *windows) Reinitialize() error {
	if err := vm.interact.init(); err != nil {
		return fmt.Errorf("failed to reinitialize ssh client: %v", err)
	}
	return nil
}

// ensureRequiredServicesStopped ensures that all services that are needed to configure a VM are stopped
func (vm *windows) ensureRequiredServicesStopped() error {
	// This slice order matters due to service dependencies
	requiredSVCs := []string{windowsExporterServiceName, kubeProxyServiceName, hybridOverlayServiceName,
		kubeletServiceName}
	for _, svcName := range requiredSVCs {
		svc := &service{name: svcName}
		if err := vm.ensureServiceNotRunning(svc); err != nil {
			return errors.Wrap(err, "could not stop service %d")
		}
	}
	return nil
}

func (vm *windows) Configure() error {
	log.Info("configuring")
	if err := vm.ensureRequiredServicesStopped(); err != nil {
		return errors.Wrap(err, "unable to stop required services")
	}
	if err := vm.createDirectories(); err != nil {
		return errors.Wrap(err, "error creating directories on Windows VM")
	}
	if err := vm.transferFiles(); err != nil {
		return errors.Wrap(err, "error transferring files to Windows VM")
	}
	if err := vm.ConfigureWindowsExporter(); err != nil {
		return errors.Wrapf(err, "error configuring Windows exporter on the Windows VM %s", vm.ID())
	}

	return vm.runBootstrapper()
}

// Start Windows metrics exporter service, only if the file is present on the VM
func (vm *windows) ConfigureWindowsExporter() error {
	windowsExporterService, err := newService(windowsExporterPath, windowsExporterServiceName, windowsExporterServiceArgs)
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", windowsExporterServiceName)
	}

	if err := vm.ensureServiceIsRunning(windowsExporterService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", windowsExporterServiceName)
	}

	return nil
}

func (vm *windows) ConfigureHybridOverlay(nodeName string) error {
	var customVxlanPortArg = ""
	if len(vm.vxlanPort) > 0 {
		customVxlanPortArg = " --hybrid-overlay-vxlan-port=" + vm.vxlanPort
	}

	hybridOverlayServiceArgs := "--node " + nodeName + customVxlanPortArg + " --k8s-kubeconfig c:\\k\\kubeconfig " +
		"--windows-service " + "--logfile " + hybridOverlayLogDir + "hybrid-overlay.log\" depend= " + kubeletServiceName

	log.Info("configure", "service", hybridOverlayServiceName, "args", hybridOverlayServiceArgs)

	hybridOverlayService, err := newService(hybridOverlayPath, hybridOverlayServiceName, hybridOverlayServiceArgs)
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

	log.Info("configured", "service", hybridOverlayServiceName, "args", hybridOverlayServiceArgs)
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
	configureCNICmd := k8sDir + "wmcb.exe configure-cni --cni-dir=\"" +
		cniDir + " --cni-config=\"" + cniConfigDest

	out, err := vm.Run(configureCNICmd, true)
	if err != nil {
		return errors.Wrap(err, "CNI configuration failed")
	}

	log.Info("configured kubelet for CNI", "cmd", configureCNICmd, "output", out)
	return nil
}

func (vm *windows) ConfigureKubeProxy(nodeName, hostSubnet string) error {
	sVIP, err := vm.getSourceVIP()
	if err != nil {
		return errors.Wrap(err, "error getting source VIP")
	}

	kubeProxyServiceArgs := "--windows-service --v=4 --proxy-mode=kernelspace --feature-gates=WinOverlay=true " +
		"--hostname-override=" + nodeName + " --kubeconfig=c:\\k\\kubeconfig " +
		"--cluster-cidr=" + hostSubnet + " --log-dir=" + kubeProxyLogDir + " --logtostderr=false " +
		"--network-name=OVNKubernetesHybridOverlayNetwork --source-vip=" + sVIP +
		" --enable-dsr=false\" depend= " + hybridOverlayServiceName

	kubeProxyService, err := newService(kubeProxyPath, kubeProxyServiceName, kubeProxyServiceArgs)
	if err != nil {
		return errors.Wrapf(err, "error creating %s service object", kubeProxyServiceName)
	}

	if err := vm.ensureServiceIsRunning(kubeProxyService); err != nil {
		return errors.Wrapf(err, "error ensuring %s Windows service has started running", kubeProxyServiceName)
	}
	log.Info("configured", "service", kubeProxyServiceName, "args", kubeProxyServiceArgs)
	return nil
}

// Interface helper methods

// createDirectories creates directories required for configuring the Windows node on the VM
func (vm *windows) createDirectories() error {
	directoriesToCreate := []string{
		k8sDir,
		remoteDir,
		cniDir,
		cniConfDir,
		logDir,
		kubeProxyLogDir,
		hybridOverlayLogDir,
	}
	for _, dir := range directoriesToCreate {
		if _, err := vm.Run(mkdirCmd(dir), false); err != nil {
			return errors.Wrapf(err, "unable to create remote directory %s", dir)
		}
	}
	return nil
}

// transferFiles copies various files required for configuring the Windows node, to the VM.
func (vm *windows) transferFiles() error {
	log.Info("transferring files")
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

	out, err := vm.Run(wmcbInitializeCmd, true)
	log.Info("configured kubelet", "cmd", wmcbInitializeCmd, "output", out)
	if err != nil {
		return errors.Wrap(err, "error running bootstrapper")
	}
	return nil
}

// initializeTestBootstrapperFiles initializes the files required for initialize-kubelet
func (vm *windows) initializeBootstrapperFiles() error {
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

// ensureServiceIsRunning ensures a Windows service is running on the VM, creating and starting it if not already so
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
	svcCreateCmd := "sc.exe create " + svc.name + " binPath=\"" + svc.binaryPath + " " + svc.args + " start=auto"
	_, err := vm.Run(svcCreateCmd, false)
	if err != nil {
		return errors.Wrapf(err, "failed to create service %s", svc.name)
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
			log.V(1).Error(err, "unable to check if Windows service is running", "service", svc.name)
			return false, nil
		}
		return !serviceRunning, nil
	})
	if err != nil {
		return errors.Wrapf(err, "error waiting for the %s service to stop", svc.name)
	}

	return nil
}

// serviceExists checks if the given service exists on Windows VM
func (vm *windows) serviceExists(serviceName string) (bool, error) {
	_, err := vm.Run("sc.exe qc "+serviceName, false)
	if err != nil {
		// 1060 is an error code representing ERROR_SERVICE_DOES_NOT_EXIST
		// referenced: https://docs.microsoft.com/en-us/windows/win32/debug/system-error-codes--1000-1299-
		if strings.Contains(err.Error(), "status 1060") {
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
	log.Info("Get-HnsNetwork", "output", out)
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

// getSourceVIP returns the source VIP of the VM
func (vm *windows) getSourceVIP() (string, error) {
	cmd := "\"Import-Module -DisableNameChecking " + hnsPSModule + "; " +
		"$net = (Get-HnsNetwork | where { $_.Name -eq 'OVNKubernetesHybridOverlayNetwork' }); " +
		"$endpoint = New-HnsEndpoint -NetworkId $net.ID -Name VIPEndpoint; " +
		"Attach-HNSHostEndpoint -EndpointID $endpoint.ID -CompartmentID 1; " +
		"(Get-NetIPConfiguration -AllCompartments -All -Detailed | " +
		"where { $_.NetAdapter.LinkLayerAddress -eq $endpoint.MacAddress }).IPV4Address.IPAddress.Trim()\""
	out, err := vm.Run(cmd, true)
	if err != nil {
		return "", errors.Wrap(err, "failed to get source VIP")
	}

	// stdout will have trailing '\r\n', so need to trim it
	sourceVIP := strings.TrimSpace(out)
	if sourceVIP == "" {
		return "", fmt.Errorf("source VIP is empty")
	}
	return sourceVIP, nil
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

// Generic helper methods

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
