package windows

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
	"github.com/openshift/windows-machine-config-operator/pkg/controller/retry"
	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/pkg/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// remoteDir is the remote temporary directory created on the Windows VM
	remoteDir = "C:\\Temp\\"
	// winTemp is the default Windows temporary directory
	winTemp = "C:\\Windows\\Temp\\"
	// cniDir is the directory for storing CNI files
	cniDir = "C:\\Temp\\cni\\"
	// wgetIgnoreCertCmd is the remote location of the wget-ignore-cert.ps1 script
	wgetIgnoreCertCmd = remoteDir + "wget-ignore-cert.ps1"
	// logDir is the remote kubernetes log directory
	logDir = "C:\\k\\log\\"
	// HybridOverlayProcess is the process name of the hybrid-overlay-node.exe in the Windows VM
	HybridOverlayProcess = "hybrid-overlay-node"
	// hybridOverlayConfigurationTime is the approximate time taken for the hybrid-overlay to complete reconfiguring
	// the Windows VM's network
	hybridOverlayConfigurationTime = 2 * time.Minute
	// BaseOVNKubeOverlayNetwork is the name of base OVN HNS Overlay network
	BaseOVNKubeOverlayNetwork = "BaseOVNKubernetesHybridOverlayNetwork"
	// OVNKubeOverlayNetwork is the name of the OVN HNS Overlay network
	OVNKubeOverlayNetwork = "OVNKubernetesHybridOverlayNetwork"
)

var log = logf.Log.WithName("windows")

// Windows is a wrapper for the WindowsVM interface.
type Windows struct {
	types.WindowsVM
	// workerIgnitionEndpoint is the Machine Config Server(MCS) endpoint from which we can download the
	// the OpenShift worker ignition file.
	workerIgnitionEndpoint string
}

// New returns a new instance of windows struct
func New(vm types.WindowsVM, workerIgnitionEndpoint string) *Windows {
	return &Windows{WindowsVM: vm, workerIgnitionEndpoint: workerIgnitionEndpoint}
}

// Configure prepares the Windows VM for the bootstrapper and then runs it
func (vm *Windows) Configure() error {
	// Create the temp directory
	_, err := vm.RunOverSSH(mkdirCmd(remoteDir), false)
	if err != nil {
		return errors.Wrapf(err, "unable to create remote directory %v", remoteDir)
	}
	if err := vm.CopyFile(wkl.IgnoreWgetPowerShellPath, remoteDir); err != nil {
		return errors.Wrapf(err, "error while copying powershell script")
	}
	return vm.runBootstrapper()
}

// validate the WindowsVM node object
func (vm *Windows) Validate() error {
	if vm.GetCredentials() == nil {
		return fmt.Errorf("nil credentials for VM")
	}
	if vm.GetCredentials().GetIPAddress() == "" {
		return fmt.Errorf("empty IP for VM: %v", vm.GetCredentials())
	}
	if vm.GetCredentials().GetInstanceId() == "" {
		return fmt.Errorf("empty instance id for VM: %v", vm.GetCredentials())
	}
	if vm.workerIgnitionEndpoint == "" {
		return errors.New("cannot use empty cluster address")
	}
	return nil
}

// runBootstrapper copies the bootstrapper and runs the code on the remote Windows VM
func (vm *Windows) runBootstrapper() error {
	if err := vm.CopyFile(wkl.WmcbPath, remoteDir); err != nil {
		return errors.Wrap(err, "error while copying wmcb binary")
	}
	err := vm.initializeBootstrapperFiles()
	if err != nil {
		return errors.Wrap(err, "error initializing bootstrapper files")
	}
	wmcbInitializeCmd := remoteDir + "\\wmcb.exe initialize-kubelet --ignition-file " + winTemp +
		"worker.ign --kubelet-path " + winTemp + "kubelet.exe"
	out, err := vm.RunOverSSH(wmcbInitializeCmd, true)
	log.V(1).Info("output from wmcb", "output", out)
	if err != nil {
		return errors.Wrap(err, "error running bootstrapper")
	}
	return nil
}

// initializeTestBootstrapperFiles initializes the files required for initialize-kubelet
func (vm *Windows) initializeBootstrapperFiles() error {
	err := vm.CopyFile(wkl.KubeletPath, winTemp)
	if err != nil {
		return errors.Wrapf(err, "unable to copy kubelet.exe to %s", winTemp)
	}

	// Download the worker ignition to C:\Windows\Temp\ using the script that ignores the server cert
	ignitionFileDownloadCmd := wgetIgnoreCertCmd + " -server " + vm.workerIgnitionEndpoint + " -output " +
		winTemp + "worker.ign"
	out, err := vm.RunOverSSH(ignitionFileDownloadCmd, true)
	log.V(1).Info("ignition file download", "cmd", ignitionFileDownloadCmd, "output", out)
	if err != nil {
		return errors.Wrap(err, "unable to download worker.ign")
	}
	return nil
}

// ConfigureHybridOverlay ensures that the hybrid overlay is running on the node
func (vm *Windows) ConfigureHybridOverlay(nodeName string) error {
	// Check if the hybrid-overlay is running
	out, err := vm.RunOverSSH("Get-Process -Name \""+HybridOverlayProcess+"\"", true)

	// err being nil implies that hybrid-overlay was running.
	if err == nil {
		// Stop the hybrid-overlay
		stopCmd := "Stop-Process -Name \"" + HybridOverlayProcess + "\""
		out, err := vm.RunOverSSH(stopCmd, true)
		if err != nil {
			log.Info("unable to stop hybrid-overlay", "stop command", stopCmd, "output", out)
			return errors.Wrap(err, "unable to stop hybrid-overlay")
		}
	}

	out, err = vm.RunOverSSH(mkdirCmd(logDir), false)
	if err != nil {
		return errors.Wrapf(err, "unable to create %s directory:\n%s", logDir, out)
	}

	if err := vm.CopyFile(wkl.HybridOverlayPath, remoteDir); err != nil {
		return errors.Wrapf(err, "error copying %s-->%s", wkl.HybridOverlayPath,
			remoteDir+wkl.HybridOverlayName)
	}

	// Start the hybrid-overlay in the background over ssh. We cannot use vm.Run() and by extension WinRM.Run() here as
	// we observed WinRM.Run() returning before the commands completes execution. The reason for that is unclear and
	// requires further investigation.
	// TODO: This will be removed in https://issues.redhat.com/browse/WINC-353
	go vm.RunOverSSH(remoteDir+wkl.HybridOverlayName+" --node "+nodeName+
		" --k8s-kubeconfig c:\\k\\kubeconfig > "+logDir+"hybrid-overlay.log 2>&1", false)

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

// waitForHNSNetworks waits for the OVN overlay HNS networks to be created until the timeout is reached
func (vm *Windows) waitForHNSNetworks() error {
	var out string
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		out, err = vm.RunOverSSH("Get-HnsNetwork", true)
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
func (vm *Windows) waitForHybridOverlayToRun() error {
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		_, err = vm.RunOverSSH("Get-Process -Name \""+HybridOverlayProcess+"\"", true)
		if err == nil {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// hybrid-overlay never started running
	return fmt.Errorf("timeout waiting for hybrid-overlay: %v", err)
}

// ConfigureCNI ensures that the CNI configuration in done on the node
func (vm *Windows) ConfigureCNI(configFile string) error {
	// create cni directory
	_, err := vm.RunOverSSH(mkdirCmd(cniDir), false)
	if err != nil {
		return errors.Wrapf(err, "unable to create CNI directory %v", cniDir)
	}

	// copy the CNI plugins and CNI config file to the windows VM
	var cniFiles = []string{
		wkl.FlannelCNIPluginPath,
		wkl.WinBridgeCNIPlugin,
		wkl.HostLocalCNIPlugin,
		wkl.WinOverlayCNIPlugin,
		configFile,
	}
	for _, file := range cniFiles {
		if err := vm.CopyFile(file, cniDir); err != nil {
			return errors.Errorf("unable to copy CNI file %s to %s", file, cniDir)
		}
	}

	cniConfigDest := cniDir + filepath.Base(configFile)
	// run the configure-cni command on windows VM
	configureCNICmd := remoteDir + "wmcb.exe configure-cni --cni-dir=\"" +
		cniDir + " --cni-config=\"" + cniConfigDest

	out, err := vm.RunOverSSH(configureCNICmd, true)
	if err != nil {
		log.Info("CNI configuration failed", "command", configureCNICmd, "output", out, "error", err)
		return errors.Wrap(err, "CNI configuration failed")
	}

	return nil
}

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
