package windows

import (
	"fmt"
	"os"
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
	// wgetIgnoreCertCmd is the remote location of the wget-ignore-cert.ps1 script
	wgetIgnoreCertCmd = remoteDir + "wget-ignore-cert.ps1"
	// logDir is the remote kubernetes log directory
	logDir = "C:\\k\\log\\"
	// HybridOverlayProcess is the process name of the hybrid-overlay-node.exe in the Windows VM
	HybridOverlayProcess = "hybrid-overlay-node"
	// BaseOVNKubeOverlayNetwork is the name of base OVN HNS Overlay network
	BaseOVNKubeOverlayNetwork = "BaseOVNKubernetesHybridOverlayNetwork"
	// OVNKubeOverlayNetwork is the name of the OVN HNS Overlay network
	OVNKubeOverlayNetwork = "OVNKubernetesHybridOverlayNetwork"
)

var log = logf.Log.WithName("windows")

// Windows is a wrapper for the WindowsVM interface.
type Windows struct {
	types.WindowsVM
}

// New returns a new instance of windows struct
func New(vm types.WindowsVM) *Windows {
	return &Windows{vm}
}

// Configure prepares the Windows VM for the bootstrapper and then runs it
func (vm *Windows) Configure() error {
	// Create the temp directory
	_, _, err := vm.Run(mkdirCmd(remoteDir), false)
	if err != nil {
		return errors.Wrapf(err, "unable to create remote directory %v", remoteDir)
	}
	if err := vm.CopyFile(wkl.IgnoreWgetPowerShellPath, remoteDir); err != nil {
		return errors.Wrapf(err, "error while copying powershell script")
	}
	return vm.runBootstrapper()
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
	stdout, stderr, err := vm.Run(wmcbInitializeCmd, true)
	if err != nil {
		return errors.Wrap(err, "error running bootstrapper")
	}
	if len(stderr) > 0 {
		log.Info("bootstrapper initialization failed", "stderr", stderr)
	}
	log.V(5).Info("stdout from wmcb", "stdout", stdout)
	return nil
}

// initializeTestBootstrapperFiles initializes the files required for initialize-kubelet
func (vm *Windows) initializeBootstrapperFiles() error {
	err := vm.CopyFile(wkl.KubeletPath, winTemp)
	if err != nil {
		return errors.Wrapf(err, "unable to copy kubelet.exe to %s", winTemp)
	}
	// TODO: As of now, getting cluster address from the environment var, remove it.
	// 		Download the worker ignition to C:\Windows\Temp\ using the script that ignores the server cert
	// 		Jira story: https://issues.redhat.com/browse/WINC-274
	ClusterAddress := os.Getenv("CLUSTER_ADDR")
	ignitionFileDownloadCmd := wgetIgnoreCertCmd + " -server https://api-int." +
		ClusterAddress + ":22623/config/worker" + " -output " + winTemp + "worker.ign"
	stdout, stderr, err := vm.Run(ignitionFileDownloadCmd, true)
	if err != nil {
		return errors.Wrap(err, "unable to download worker.ign")
	}

	log.V(5).Info("stderr associated with the ignition file download", "stderr", stderr)
	if len(stderr) > 0 {
		log.Info("error while downloading the ignition file from cluster", "stderr", stderr)
	}
	log.V(5).Info("stdout associated with the ignition file download", "stdout", stdout)
	return nil
}

// ConfigureHybridOverlay ensures that the hybrid overlay is running on the node
func (vm *Windows) ConfigureHybridOverlay(nodeName string) error {
	// Check if the hybrid-overlay is running
	_, stderr, err := vm.Run("Get-Process -Name \""+HybridOverlayProcess+"\"", true)

	// stderr being empty implies that hybrid-overlay was running.
	if err == nil || stderr == "" {
		// Stop the hybrid-overlay
		stopCmd := "Stop-Process -Name \"" + HybridOverlayProcess + "\""
		_, stderr, err := vm.Run(stopCmd, true)
		if err != nil || stderr != "" {
			log.Info("unable to stop hybrid-overlay", "stop command", stopCmd, "stderr", stderr)
			return errors.Wrap(err, "unable to stop hybrid-overlay")
		}
	}

	_, stderr, err = vm.Run(mkdirCmd(logDir), false)
	if err != nil {
		return errors.Wrap(err, fmt.Sprintf("unable to create %s directory:\n%s", logDir, stderr))
	}

	if err := vm.CopyFile(wkl.HybridOverlayPath, remoteDir); err != nil {
		return errors.Wrap(err, fmt.Sprintf("error copying %s-->%s", wkl.HybridOverlayPath,
			remoteDir+wkl.HybridOverlayName))
	}

	// Start the hybrid-overlay in the background over ssh. We cannot use vm.Run() and by extension WinRM.Run() here as
	// we observed WinRM.Run() returning before the commands completes execution. The reason for that is unclear and
	// requires further investigation.
	// TODO: This will be removed in https://issues.redhat.com/browse/WINC-353
	go vm.RunOverSSH(remoteDir+wkl.HybridOverlayName+" --node "+nodeName+
		" --k8s-kubeconfig c:\\k\\kubeconfig > "+logDir+"hybrid-overlay.log 2>&1", false)

	if err = vm.waitForHybridOverlayToRun(); err != nil {
		return errors.Wrap(err, fmt.Sprintf("error running %s", wkl.HybridOverlayName))
	}

	if err = vm.waitForHNSNetworks(); err != nil {
		return errors.Wrap(err, "error waiting for OVN HNS networks to be created")
	}

	// Running the hybrid-overlay causes network reconfiguration in the Windows VM which results in the ssh connection
	// being closed and the client is not smart enough to reconnect. We have observed that the WinRM connection does not
	// get closed and does not need reinitialization.
	if err = vm.Reinitialize(); err != nil {
		return errors.Wrap(err, "error reinitializing VM after running hybrid-overlay")
	}

	return nil
}

// waitForHNSNetworks waits for the OVN overlay HNS networks to be created until the timeout is reached
func (vm *Windows) waitForHNSNetworks() error {
	var stdout string
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		stdout, _, err = vm.Run("Get-HnsNetwork", true)
		if err != nil {
			// retry
			continue
		}

		if strings.Contains(stdout, BaseOVNKubeOverlayNetwork) &&
			strings.Contains(stdout, OVNKubeOverlayNetwork) {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// OVN overlay HNS networks were not found
	log.Info("Get-HnsNetwork", "stdout", stdout)
	return errors.Wrap(err, "timeout waiting for OVN overlay HNS networks")
}

// waitForHybridOverlayToRun waits for the hybrid-overlay-node.exe to run until the timeout is reached
func (vm *Windows) waitForHybridOverlayToRun() error {
	var err error
	for retries := 0; retries < retry.Count; retries++ {
		_, _, err = vm.Run("Get-Process -Name \""+HybridOverlayProcess+"\"", true)
		if err == nil {
			return nil
		}
		time.Sleep(retry.Interval)
	}

	// hybrid-overlay never started running
	return fmt.Errorf("timeout waiting for hybrid-overlay: %v", err)
}

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
