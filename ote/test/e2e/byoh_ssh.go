package winc

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type sshCommandSession interface {
	CombinedOutput(string) ([]byte, error)
	Close() error
}

// windowsSSHClient is the narrow out-of-band transport used only for direct key authentication and
// post-unregistration reset proof.
type windowsSSHClient struct {
	client     *ssh.Client
	newSession func() (sshCommandSession, error)
	closeFn    func() error
}

var errBYOHHostKeyMismatch = errors.New("presented SSH host key does not match trusted inventory")

func trustedHostKeyCallback(expected ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		if expected == nil || presented == nil || !bytes.Equal(presented.Marshal(), expected.Marshal()) {
			return errBYOHHostKeyMismatch
		}
		return nil
	}
}

func sshErrorClass(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "authenticate") {
		return "authentication"
	}
	if strings.Contains(lower, "host key") {
		return "host-key"
	}
	return "transport"
}

func newWindowsSSHClient(ctx context.Context, lease *byohHostLease, privateKey []byte) (*windowsSSHClient, error) {
	if lease == nil || lease.SSHAddress == "" || lease.Username == "" || lease.TrustedHostKey == nil {
		return nil, fmt.Errorf("incomplete BYOH SSH fixture")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse SSH private key: %w", err)
	}
	config := &ssh.ClientConfig{
		User: lease.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: trustedHostKeyCallback(lease.TrustedHostKey),
		Timeout:         30 * time.Second,
	}
	connection, err := (&net.Dialer{Timeout: config.Timeout}).DialContext(ctx, "tcp",
		net.JoinHostPort(lease.SSHAddress, "22"))
	if err != nil {
		return nil, fmt.Errorf("connect to selected BYOH host over SSH (%s)", sshErrorClass(ctx, err))
	}
	return newWindowsSSHClientFromConnection(ctx, connection, net.JoinHostPort(lease.SSHAddress, "22"), config)
}

func newWindowsSSHClientFromConnection(ctx context.Context, connection net.Conn, address string,
	config *ssh.ClientConfig) (*windowsSSHClient, error) {
	closeOnCancel := context.AfterFunc(ctx, func() { _ = connection.Close() })
	var handshakeDeadline time.Time
	if config != nil && config.Timeout > 0 {
		handshakeDeadline = time.Now().Add(config.Timeout)
	}
	if deadline, ok := ctx.Deadline(); ok && (handshakeDeadline.IsZero() || deadline.Before(handshakeDeadline)) {
		handshakeDeadline = deadline
	}
	if !handshakeDeadline.IsZero() {
		if err := connection.SetDeadline(handshakeDeadline); err != nil {
			closeErr := connection.Close()
			return nil, errors.Join(fmt.Errorf("set selected BYOH SSH handshake deadline: %w", err),
				wrapOptionalError("close SSH connection after deadline failure", closeErr))
		}
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection,
		address, config)
	closeOnCancel()
	if err != nil {
		_ = connection.Close()
		class := sshErrorClass(ctx, err)
		if class == "host-key" {
			return nil, fmt.Errorf("establish selected BYOH SSH session (%s): %w", class, errBYOHHostKeyMismatch)
		}
		return nil, fmt.Errorf("establish selected BYOH SSH session (%s)", class)
	}
	if ctx.Err() != nil {
		_ = clientConnection.Close()
		return nil, fmt.Errorf("establish selected BYOH SSH session (%s)", sshErrorClass(ctx, ctx.Err()))
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		closeErr := clientConnection.Close()
		return nil, errors.Join(fmt.Errorf("clear selected BYOH SSH handshake deadline: %w", err),
			wrapOptionalError("close SSH connection after clearing deadline", closeErr))
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	return &windowsSSHClient{client: client,
		newSession: func() (sshCommandSession, error) { return client.NewSession() }, closeFn: client.Close}, nil
}

func (c *windowsSSHClient) close() error {
	if c == nil {
		return nil
	}
	if c.closeFn != nil {
		return c.closeFn()
	}
	if c.client != nil {
		return c.client.Close()
	}
	return nil
}

func encodePowerShell(script string) string {
	encoded := utf16.Encode([]rune(script))
	bytes := make([]byte, len(encoded)*2)
	for i, value := range encoded {
		bytes[i*2], bytes[i*2+1] = byte(value), byte(value>>8)
	}
	return base64.StdEncoding.EncodeToString(bytes)
}

func runPowerShellOverSSH(ctx context.Context, client *windowsSSHClient, script string) (output string, retErr error) {
	if client == nil || client.newSession == nil {
		return "", fmt.Errorf("SSH client is not initialized")
	}
	type sessionResult struct {
		session sshCommandSession
		err     error
	}
	sessionCh := make(chan sessionResult, 1)
	go func() {
		session, err := client.newSession()
		sessionCh <- sessionResult{session: session, err: err}
	}()
	var session sshCommandSession
	select {
	case <-ctx.Done():
		_ = client.close()
		result := <-sessionCh
		if result.session != nil {
			_ = result.session.Close()
		}
		return "", fmt.Errorf("create selected BYOH SSH session (%s): %w", sshErrorClass(ctx, ctx.Err()), ctx.Err())
	case result := <-sessionCh:
		if result.err != nil {
			return "", fmt.Errorf("create selected BYOH SSH session (%s)", sshErrorClass(ctx, result.err))
		}
		session = result.session
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("close selected BYOH SSH session: %w", closeErr)
		}
	}()
	type result struct {
		output []byte
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		output, err := session.CombinedOutput("powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand " +
			encodePowerShell(script))
		resultCh <- result{output: output, err: err}
	}()
	select {
	case <-ctx.Done():
		// Closing the transport first guarantees CombinedOutput is released. Join the worker before
		// returning so cancellation cannot leak a command goroutine. Close errors are secondary to
		// the context cancellation and are therefore best effort on this branch.
		_ = client.close()
		<-resultCh
		return "", fmt.Errorf("PowerShell command canceled (%s): %w", sshErrorClass(ctx, ctx.Err()), ctx.Err())
	case result := <-resultCh:
		if result.err != nil {
			return "", fmt.Errorf("PowerShell command failed (%s)", sshErrorClass(ctx, result.err))
		}
		return strings.TrimSpace(string(result.output)), nil
	}
}

// verifySSHAuthentication proves direct client authentication while verifying the server against
// the producer-supplied OpenSSH host public key retained in the physical-host lease.
func verifySSHAuthentication(ctx context.Context, lease *byohHostLease, privateKey []byte) error {
	return wait.PollUntilContextTimeout(ctx, 15*time.Second, 10*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			client, err := newWindowsSSHClient(ctx, lease, privateKey)
			if err != nil {
				if errors.Is(err, errBYOHHostKeyMismatch) {
					return false, err
				}
				return false, nil
			}
			_, commandErr := runPowerShellOverSSH(ctx, client, "Write-Output 'ready'")
			closeErr := client.close()
			if commandErr != nil {
				return false, nil
			}
			if closeErr != nil {
				return false, fmt.Errorf("close authenticated BYOH SSH client: %w", closeErr)
			}
			return true, nil
		})
}

var byohManagedServices = []string{
	"windows_exporter", "kube-proxy", "hybrid-overlay-node", "kubelet",
	"windows-instance-config-daemon", "containerd",
}

var byohManagedDirectories = []string{
	`C:\Temp`, `C:\k\cni`, `C:\k\cni\config`, `C:\var\log`, `C:\var\log\kubelet`,
	`C:\var\log\csi-proxy`, `C:\var\log\kube-proxy`, `C:\var\log\wicd`,
	`C:\var\log\hybrid-overlay`, `C:\k\containerd`, `C:\var\log\containerd`,
	`C:\var\log\windows-exporter`, `C:\k\containerd\registries`, `C:\k\etc\kubernetes\manifests`,
	`C:\k`, `C:\k\tls`, `C:\k\wicd-certs`,
}

const (
	managedServicesStoppedProof   = "WMCO_BYOH_SERVICES_STOPPED"
	managedDirectoriesAbsentProof = "WMCO_BYOH_DIRECTORIES_ABSENT"
	managedPathStateProof         = "WMCO_BYOH_PATH_STATE_VERIFIED"
)

func managedServicesStoppedScript() string {
	return "$ErrorActionPreference='Stop'; $names=@('" + strings.Join(byohManagedServices, "','") +
		"'); foreach($name in $names){try{$svc=Get-Service -Name $name -ErrorAction Stop; " +
		"if($svc.Status -ne [System.ServiceProcess.ServiceControllerStatus]::Stopped){exit 1}} " +
		"catch [Microsoft.PowerShell.Commands.ServiceCommandException]{" +
		"if($_.FullyQualifiedErrorId -notlike 'NoServiceFoundForGivenName,*'){exit 2}} catch{exit 2}}; " +
		"Write-Output '" + managedServicesStoppedProof + "'"
}

func managedDirectoriesRemovedScript() string {
	return "$ErrorActionPreference='Stop'; $paths=@('" + strings.Join(byohManagedDirectories, "','") +
		"'); try{foreach($path in $paths){if(Test-Path -LiteralPath $path -ErrorAction Stop){exit 1}}} " +
		"catch{exit 2}; Write-Output '" + managedDirectoriesAbsentProof + "'"
}

func assertManagedServicesStopped(ctx context.Context, client *windowsSSHClient) error {
	output, err := runPowerShellOverSSH(ctx, client, managedServicesStoppedScript())
	if err != nil {
		return err
	}
	if output != managedServicesStoppedProof {
		return fmt.Errorf("managed service reset probe returned no exact success proof")
	}
	return nil
}

func assertManagedDirectoriesRemoved(ctx context.Context, client *windowsSSHClient) error {
	output, err := runPowerShellOverSSH(ctx, client, managedDirectoriesRemovedScript())
	if err != nil {
		return err
	}
	if output != managedDirectoriesAbsentProof {
		return fmt.Errorf("managed directory reset probe returned no exact success proof")
	}
	return nil
}

func assertPathState(ctx context.Context, client *windowsSSHClient, path string, present bool) error {
	expected := "$false"
	if present {
		expected = "$true"
	}
	output, err := runPowerShellOverSSH(ctx, client, fmt.Sprintf(
		"$ErrorActionPreference='Stop'; try{if((Test-Path -LiteralPath '%s' -ErrorAction Stop) -ne %s){exit 1}} "+
			"catch{exit 2}; Write-Output '%s'", strings.ReplaceAll(path, "'", "''"), expected,
		managedPathStateProof))
	if err != nil {
		return err
	}
	if output != managedPathStateProof {
		return fmt.Errorf("managed path reset probe returned no exact success proof")
	}
	return nil
}

func (f *byohTestFixture) cloudPrivateKey(ctx context.Context) ([]byte, error) {
	secret, err := f.coreClient.Secrets(wmcoNamespace).Get(ctx, byohPrivateKeySecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("required cloud private key Secret is unavailable: %w", err)
	}
	key := secret.Data[byohPrivateKeyDataKey]
	if len(key) == 0 {
		return nil, fmt.Errorf("required cloud private key Secret has no private key")
	}
	return append([]byte(nil), key...), nil
}

func (f *byohTestFixture) sshClient(ctx context.Context, lease *byohHostLease) (*windowsSSHClient, error) {
	if f.sshClientForTest != nil {
		return f.sshClientForTest(ctx, lease)
	}
	key, err := f.cloudPrivateKey(ctx)
	if err != nil {
		return nil, err
	}
	return newWindowsSSHClient(ctx, lease, key)
}

func (f *byohTestFixture) assertPostDeconfigurationPathAbsent(ctx context.Context, lease *byohHostLease,
	path string) error {
	client, err := f.sshClient(ctx, lease)
	if err != nil {
		return err
	}
	assertErr := assertPathState(ctx, client, path, false)
	return errors.Join(assertErr, wrapOptionalError("close post-deconfiguration SSH client", client.close()))
}
