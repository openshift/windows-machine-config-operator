package windows

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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
	// kubeProxyPath is the location of the kube-proxy exe
	kubeProxyPath = k8sDir + "kube-proxy.exe"
	// HybridOverlayProcess is the process name of the hybrid-overlay-node.exe in the Windows VM
	HybridOverlayProcess = "hybrid-overlay-node"
	// hybridOverlayConfigurationTime is the approximate time taken for the hybrid-overlay to complete reconfiguring
	// the Windows VM's network
	hybridOverlayConfigurationTime = 2 * time.Minute
	// BaseOVNKubeOverlayNetwork is the name of base OVN HNS Overlay network
	BaseOVNKubeOverlayNetwork = "BaseOVNKubernetesHybridOverlayNetwork"
	// OVNKubeOverlayNetwork is the name of the OVN HNS Overlay network
	OVNKubeOverlayNetwork = "OVNKubernetesHybridOverlayNetwork"
	// kubeProxyServiceName is the name of the kube-proxy Windows service
	kubeProxyServiceName = "kube-proxy"
	// remotePowerShellCmdPrefix holds the PowerShell prefix that needs to be prefixed  for every remote PowerShell
	// command executed on the remote Windows VM
	remotePowerShellCmdPrefix = "powershell.exe -NonInteractive -ExecutionPolicy Bypass "
)

var log = logf.Log.WithName("windows")

// Windows contains all the  methods needed to configure a Windows VM to become a worker node
type Windows interface {
	// ID returns the cloud provider ID of the VM
	ID() string
	// CopyFile copies the given file to the remote directory in the Windows VM. The remote directory is created if it
	// does not exist
	CopyFile(string, string) error
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
func New(ipAddress, instanceID, workerIgnitionEndpoint, vxlanPort string, signer ssh.Signer) (Windows, error) {
	if workerIgnitionEndpoint == "" {
		return nil, errors.New("cannot use empty ignition endpoint")
	}

	// Update the logger name with the VM's cloud ID
	log = logf.Log.WithName(fmt.Sprintf("VM %s", instanceID))
	// For now, let's use the `Administrator` user for every node
	conn, err := newSshConnectivity("Administrator", ipAddress, signer)
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

func (vm *windows) CopyFile(filePath, remoteDir string) error {
	if err := vm.interact.transfer(filePath, remoteDir); err != nil {
		return errors.Wrapf(err, "unable to transfer %s to remote dir %s", filePath, remoteDir)
	}
	return nil
}

func (vm *windows) Run(cmd string, psCmd bool) (string, error) {
	if psCmd {
		cmd = remotePowerShellCmdPrefix + cmd
	}

	out, err := vm.interact.run(cmd)
	if err != nil {
		return "", errors.Wrapf(err, "error running %s", cmd)
	}
	return out, nil
}

func (vm *windows) Reinitialize() error {
	if err := vm.interact.init(); err != nil {
		return fmt.Errorf("failed to reinitialize ssh client: %v", err)
	}
	return nil
}

func (vm *windows) Configure() error {
	if err := vm.createDirectories(); err != nil {
		return errors.Wrap(err, "error creating directories on Windows VM")
	}
	if err := vm.transferFiles(); err != nil {
		return errors.Wrap(err, "error transferring files to Windows VM")
	}
	return vm.runBootstrapper()
}

func (vm *windows) ConfigureHybridOverlay(nodeName string) error {
	// Check if the hybrid-overlay is running
	_, err := vm.Run("Get-Process -Name \""+HybridOverlayProcess+"\"", true)

	// err being nil implies that hybrid-overlay is running.
	if err == nil {
		// Stop the hybrid-overlay
		stopCmd := "Stop-Process -Name \"" + HybridOverlayProcess + "\""
		out, err := vm.Run(stopCmd, true)
		if err != nil {
			log.Info("unable to stop hybrid-overlay", "stop command", stopCmd, "output", out)
			return errors.Wrap(err, "unable to stop hybrid-overlay")
		}
	}
	var customVxlanPortArg = ""
	if len(vm.vxlanPort) > 0 {
		customVxlanPortArg = " --hybrid-overlay-vxlan-port=" + vm.vxlanPort
	}
	// Start the hybrid-overlay in the background over ssh.
	go vm.Run(k8sDir+wkl.HybridOverlayName+" --node "+nodeName+customVxlanPortArg+
		" --k8s-kubeconfig c:\\k\\kubeconfig --logfile="+hybridOverlayLogDir+"hybrid-overlay.log", false)

	if err = vm.waitForHybridOverlayToRun(); err != nil {
		return errors.Wrapf(err, "error running %s", wkl.HybridOverlayName)
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

	return nil
}

func (vm *windows) ConfigureCNI(configFile string) error {
	// copy the CNI config file to the Windows VM
	if err := vm.CopyFile(configFile, cniConfDir); err != nil {
		return errors.Errorf("unable to copy CNI file %s to %s", configFile, cniConfDir)
	}

	cniConfigDest := cniConfDir + filepath.Base(configFile)
	// run the configure-cni command on the Windows VM
	configureCNICmd := k8sDir + "wmcb.exe configure-cni --cni-dir=\"" +
		cniDir + " --cni-config=\"" + cniConfigDest

	out, err := vm.Run(configureCNICmd, true)
	if err != nil {
		log.Info("CNI configuration failed", "command", configureCNICmd, "output", out, "error", err)
		return errors.Wrap(err, "CNI configuration failed")
	}

	return nil
}

func (vm *windows) ConfigureKubeProxy(nodeName, hostSubnet string) error {
	sVIP, err := vm.getSourceVIP()
	if err != nil {
		return errors.Wrap(err, "error getting source VIP")
	}
	kubeProxyService, err := newKubeProxyService(nodeName, hostSubnet, sVIP)
	if err != nil {
		return errors.Wrap(err, "error creating service object")
	}
	// Check if service is already running, then stop it.
	if vm.IsRunning(kubeProxyService) {
		if err := vm.stopService(kubeProxyService); err != nil {
			return errors.Wrap(err, "error stopping kube-proxy Windows service")
		}
	} else {
		if err := vm.createService(kubeProxyService); err != nil {
			return errors.Wrap(err, "error creating kube-proxy Windows service")
		}
	}
	if err := vm.startService(kubeProxyService); err != nil {
		return errors.Wrap(err, "error starting kube-proxy Windows service")
	}
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
	srcDestPairs := map[string]string{
		wkl.IgnoreWgetPowerShellPath: remoteDir,
		wkl.WmcbPath:                 k8sDir,
		wkl.HybridOverlayPath:        k8sDir,
		wkl.HNSPSModule:              remoteDir,
		wkl.FlannelCNIPluginPath:     cniDir,
		wkl.WinBridgeCNIPlugin:       cniDir,
		wkl.HostLocalCNIPlugin:       cniDir,
		wkl.WinOverlayCNIPlugin:      cniDir,
		wkl.KubeProxyPath:            k8sDir,
		wkl.KubeletPath:              k8sDir,
	}
	for src, dest := range srcDestPairs {
		if err := vm.CopyFile(src, dest); err != nil {
			return errors.Wrapf(err, "error copying %s to %s ", src, dest)
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
	log.V(1).Info("output from wmcb", "output", out)
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
	out, err := vm.Run(ignitionFileDownloadCmd, true)
	log.V(1).Info("ignition file download", "cmd", ignitionFileDownloadCmd, "output", out)
	if err != nil {
		return errors.Wrap(err, "unable to download worker.ign")
	}
	return nil
}

// createService creates the service on the Windows VM
func (vm *windows) createService(svc service) error {
	out, err := vm.Run("sc.exe create "+svc.Name()+" binPath=\""+svc.BinaryPath()+" "+
		svc.Args()+"\" start=auto", false)
	if err != nil {
		return errors.Wrapf(err, "failed to create service with output: %s", out)
	}
	return nil
}

// stopService stops the service that was already running
func (vm *windows) stopService(svc service) error {
	out, err := vm.Run("sc.exe stop "+svc.Name(), false)
	if err != nil {
		return errors.Wrapf(err, "failed to start service with output: %s", out)
	}
	return nil
}

// IsRunning gets the status of given service
func (vm *windows) IsRunning(svc service) bool {
	out, err := vm.Run("sc.exe query "+svc.Name(), false)
	if err != nil {
		log.Error(err, "failed to start service with output: %s", out)
		return false
	}
	return strings.Contains(out, "RUNNING")
}

// startService starts a previously created Windows service
func (vm *windows) startService(svc service) error {
	out, err := vm.Run("sc.exe start "+svc.Name(), false)
	if err != nil {
		return errors.Wrapf(err, "failed to start service with output: %s", out)
	}
	log.V(1).Info("started service", "name", svc.Name(), "binary", svc.BinaryPath(), "args", svc.Args())
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

// waitForHybridOverlayToRun waits for the hybrid-overlay-node.exe to run until the timeout is reached
func (vm *windows) waitForHybridOverlayToRun() error {
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		_, err = vm.Run("Get-Process -Name \""+HybridOverlayProcess+"\"", true)
		if err == nil {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// hybrid-overlay never started running
	return fmt.Errorf("timeout waiting for hybrid-overlay: %v", err)
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
		return "", errors.Wrapf(err, "failed to get source VIP with output: %s", out)
	}

	// stdout will have trailing '\r\n', so need to trim it
	sourceVIP := strings.TrimSpace(out)
	if sourceVIP == "" {
		return "", fmt.Errorf("source VIP is empty")
	}
	return sourceVIP, nil
}

// Generic helper methods

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
