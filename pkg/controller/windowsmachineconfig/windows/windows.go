package windows

import (
	"os"

	"github.com/openshift/windows-machine-config-bootstrapper/tools/windows-node-installer/pkg/types"
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

// mkdirCmd returns the Windows command to create a directory if it does not exists
func mkdirCmd(dirName string) string {
	return "if not exist " + dirName + " mkdir " + dirName
}
