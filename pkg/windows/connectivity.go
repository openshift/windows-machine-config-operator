package windows

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

// sshPort is the default SSH port
const sshPort = "22"

// AuthErr occurs when our authentication into the VM is rejected
type AuthErr struct {
	err string
}

func (e *AuthErr) Error() string {
	return fmt.Sprintf("SSH authentication failed: %s", e.err)
}

// newAuthErr returns a new AuthErr
func newAuthErr(err error) *AuthErr {
	return &AuthErr{err: err.Error()}
}

type connectivity interface {
	// run executes the given command on the remote system
	run(cmd string) (string, error)
	// transfer copies the file from the local disk to the remote VM directory, creating the remote directory if needed
	transfer(filePath, remoteDir string) error
	// init initialises the connectivity medium
	init() error
}

// sshConnectivity encapsulates the information needed to connect to the Windows VM over ssh
type sshConnectivity struct {
	// username is the user to connect to the VM
	username string
	// ipAddress is the VM's IP address
	ipAddress string
	// signer is used for authenticating against the VM
	signer ssh.Signer
	// sshClient is the client used to access the Windows VM via ssh
	sshClient *ssh.Client
	log       logr.Logger
}

// newSshConnectivity returns an instance of sshConnectivity
func newSshConnectivity(username, ipAddress string, signer ssh.Signer, logger logr.Logger) (connectivity, error) {
	c := &sshConnectivity{
		username:  username,
		ipAddress: ipAddress,
		signer:    signer,
		log:       logger,
	}
	if err := c.init(); err != nil {
		return nil, errors.Wrap(err, "error instantiating SSH client")
	}
	return c, nil
}

// init initialises the key based SSH client
func (c *sshConnectivity) init() error {
	if c.username == "" || c.ipAddress == "" || c.signer == nil {
		return fmt.Errorf("incomplete sshConnectivity information: %v", c)
	}

	config := &ssh.ClientConfig{
		User: c.username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(c.signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	var err error
	var sshClient *ssh.Client
	// Retry if we are unable to create a client as the VM could still be executing the steps in its user data
	err = wait.PollImmediate(time.Minute, retry.Timeout, func() (bool, error) {
		sshClient, err = ssh.Dial("tcp", c.ipAddress+":"+sshPort, config)
		if err == nil {
			return true, nil
		}
		c.log.V(1).Info("SSH dial", "IP Address", c.ipAddress, "error", err)
		if strings.Contains(err.Error(), "unable to authenticate") {
			// Authentication failure is a special case that must be handled differently
			return false, newAuthErr(err)
		}
		return false, nil
	})
	if err != nil {
		return errors.Wrapf(err, "unable to connect to Windows VM %s", c.ipAddress)
	}
	c.sshClient = sshClient
	return nil
}

// run instantiates a new SSH session and runs the command on the VM and returns the combined stdout and stderr output
func (c *sshConnectivity) run(cmd string) (string, error) {
	if c.sshClient == nil {
		return "", errors.New("run cannot be called with nil SSH client")
	}

	session, err := c.sshClient.NewSession()
	if err != nil {
		return "", err
	}
	defer func() {
		// io.EOF is returned if you attempt to close a session that is already closed which typically happens given
		// that Run(), which is called by CombinedOutput(), internally closes the session.
		if err := session.Close(); err != nil && !errors.Is(err, io.EOF) {
			c.log.Error(err, "error closing SSH session")
		}
	}()

	out, err := session.CombinedOutput(cmd)
	return string(out), err
}

// transfer uses FTP to copy the file from the local disk to the remote VM directory, creating the directory if needed
func (c *sshConnectivity) transfer(filePath, remoteDir string) error {
	if c.sshClient == nil {
		return errors.New("transfer cannot be called with nil SSH client")
	}

	ftp, err := sftp.NewClient(c.sshClient)
	if err != nil {
		return err
	}
	defer func() {
		if err := ftp.Close(); err != nil {
			c.log.Error(err, "error closing FTP connection")
		}
	}()

	f, err := os.Open(filePath)
	if err != nil {
		return errors.Wrapf(err, "error opening %s file to be transferred", filePath)
	}
	defer func() {
		if err := f.Close(); err != nil {
			c.log.Error(err, "error closing local file", "file", filePath)
		}
	}()

	if err := ftp.MkdirAll(remoteDir); err != nil {
		return errors.Wrapf(err, "error creating remote directory %s", remoteDir)
	}

	remoteFile := remoteDir + "\\" + filepath.Base(filePath)
	dstFile, err := ftp.Create(remoteFile)
	if err != nil {
		return errors.Wrapf(err, "error initializing %s file on Windows VM", remoteFile)
	}

	_, err = io.Copy(dstFile, f)
	if err != nil {
		return errors.Wrapf(err, "error copying %s to the Windows VM", filePath)
	}

	// Forcefully close the file so that we can execute it later in the case of binaries
	if err := dstFile.Close(); err != nil {
		c.log.Error(err, "error closing remote file", "file", remoteFile)
	}
	return nil
}
