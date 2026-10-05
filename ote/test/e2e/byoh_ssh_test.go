package winc

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/client-go/kubernetes/fake"
)

func TestManagedResetContractScripts(t *testing.T) {
	for _, required := range []string{`C:\Temp`, `C:\k\etc\kubernetes\manifests`, `C:\k`} {
		if !slices.Contains(byohManagedDirectories, required) {
			t.Fatalf("required managed directory %q is missing", required)
		}
	}
	if slices.Contains(byohManagedDirectories, `C:\k\pod-manifests`) {
		t.Fatal("obsolete pod-manifests path remains in reset contract")
	}
	serviceScript := managedServicesStoppedScript()
	if !strings.Contains(serviceScript, "ServiceControllerStatus]::Stopped") ||
		!strings.Contains(serviceScript, "NoServiceFoundForGivenName") ||
		!strings.Contains(serviceScript, "-ErrorAction Stop") {
		t.Fatal("service predicate does not reject non-Stopped transitional states")
	}
	if !strings.Contains(managedDirectoriesRemovedScript(), "Test-Path -LiteralPath") ||
		!strings.Contains(managedDirectoriesRemovedScript(), "-ErrorAction Stop") {
		t.Fatal("directory predicate does not check every literal managed path")
	}
}

type staticCommandSession struct {
	output []byte
	err    error
}

func (s *staticCommandSession) CombinedOutput(string) ([]byte, error) { return s.output, s.err }
func (s *staticCommandSession) Close() error                          { return nil }

func resetProtocolClient(output string, err error) *windowsSSHClient {
	return &windowsSSHClient{newSession: func() (sshCommandSession, error) {
		return &staticCommandSession{output: []byte(output), err: err}, nil
	}}
}

func TestManagedResetProofProtocolFailsClosed(t *testing.T) {
	serviceCases := []struct {
		name, output string
		err          error
		wantOK       bool
	}{
		{name: "missing service", output: managedServicesStoppedProof, wantOK: true},
		{name: "stopped service", output: managedServicesStoppedProof, wantOK: true},
		{name: "running service", err: errors.New("exit status 1")},
		{name: "transitional service", err: errors.New("exit status 1")},
		{name: "command exception", err: errors.New("exit status 2")},
		{name: "successful command without proof", output: "unexpected"},
	}
	for _, test := range serviceCases {
		t.Run(test.name, func(t *testing.T) {
			err := assertManagedServicesStopped(context.Background(), resetProtocolClient(test.output, test.err))
			if (err == nil) != test.wantOK {
				t.Fatalf("service reset protocol result mismatch: wantOK=%t err=%v", test.wantOK, err)
			}
		})
	}
	pathCases := []struct {
		name, output string
		err          error
		wantOK       bool
	}{
		{name: "path missing", output: managedDirectoriesAbsentProof, wantOK: true},
		{name: "path present", err: errors.New("exit status 1")},
		{name: "provider error", err: errors.New("exit status 2")},
		{name: "nonzero command", err: errors.New("exit status 9")},
		{name: "successful command without proof", output: "unexpected"},
	}
	for _, test := range pathCases {
		t.Run(test.name, func(t *testing.T) {
			err := assertManagedDirectoriesRemoved(context.Background(), resetProtocolClient(test.output, test.err))
			if (err == nil) != test.wantOK {
				t.Fatalf("directory reset protocol result mismatch: wantOK=%t err=%v", test.wantOK, err)
			}
		})
	}
}

func TestCanceledContextsAndSanitizedSSHClassification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lease := &byohHostLease{Address: "192.0.2.10", AddressType: byohAddressIP}
	lookup := func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}
	if err := waitForLeaseNodeWithOptions(ctx, fake.NewSimpleClientset().CoreV1().Nodes(), lease,
		lookup, "", time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("canceled discovery context was ignored")
	}
	classified := sshErrorClass(ctx, context.Canceled)
	if classified != "timeout" || strings.Contains(classified, lease.Address) {
		t.Fatalf("SSH cancellation classification was not sanitized: %q", classified)
	}
}

type closeTrackingConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

type deadlineTrackingConn struct {
	net.Conn
	mu      sync.Mutex
	calls   int
	failOn  int
	failure error
	closed  bool
}

func (c *deadlineTrackingConn) SetDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == c.failOn {
		return c.failure
	}
	return nil
}

func (c *deadlineTrackingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *deadlineTrackingConn) wasClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type blockingCommandSession struct {
	transportClosed chan struct{}
	workerExited    chan struct{}
	once            sync.Once
}

func (s *blockingCommandSession) CombinedOutput(string) ([]byte, error) {
	<-s.transportClosed
	s.once.Do(func() { close(s.workerExited) })
	return nil, errors.New("transport closed")
}

func (s *blockingCommandSession) Close() error {
	select {
	case <-s.workerExited:
		return nil
	case <-time.After(200 * time.Millisecond):
		return errors.New("command worker was not joined before session close")
	}
}

func (c *closeTrackingConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestSSHHandshakeAndSessionCreationHonorCancellation(t *testing.T) {
	t.Run("trusted host key callback accepts only the retained key", func(t *testing.T) {
		trusted := mustParseTestHostKey(t, testHostPublicKey(t))
		other := mustParseTestHostKey(t, testHostPublicKey(t))
		callback := trustedHostKeyCallback(trusted)
		if err := callback("fixture", nil, trusted); err != nil {
			t.Fatalf("retained trusted host key was rejected: %v", err)
		}
		if err := callback("fixture", nil, other); !errors.Is(err, errBYOHHostKeyMismatch) {
			t.Fatalf("different host key was not rejected with the trust sentinel: %v", err)
		}
		if err := trustedHostKeyCallback(nil)("fixture", nil, trusted); !errors.Is(err, errBYOHHostKeyMismatch) {
			t.Fatalf("missing trusted key did not fail closed: %v", err)
		}
	})

	t.Run("setting handshake deadline failure closes raw connection", func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		deadlineErr := errors.New("set deadline fixture failure")
		tracked := &deadlineTrackingConn{Conn: clientSide, failOn: 1, failure: deadlineErr}
		_, err := newWindowsSSHClientFromConnection(context.Background(), tracked, "fixture:22", &ssh.ClientConfig{
			User: "fixture", HostKeyCallback: trustedHostKeyCallback(mustParseTestHostKey(t, testHostPublicKey(t))),
			Timeout: time.Second})
		if !errors.Is(err, deadlineErr) || !tracked.wasClosed() {
			t.Fatalf("SetDeadline failure was not wrapped and closed: closed=%t err=%v", tracked.wasClosed(), err)
		}
	})

	t.Run("clearing handshake deadline failure closes established connection", func(t *testing.T) {
		privateKey, err := generateTestPrivateKey()
		if err != nil {
			t.Fatal(err)
		}
		hostSigner, err := ssh.ParsePrivateKey(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		serverConfig := &ssh.ServerConfig{NoClientAuth: true}
		serverConfig.AddHostKey(hostSigner)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			serverSide, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			defer serverSide.Close()
			connection, channels, requests, serverErr := ssh.NewServerConn(serverSide, serverConfig)
			if serverErr != nil {
				return
			}
			defer connection.Close()
			go ssh.DiscardRequests(requests)
			for channel := range channels {
				_ = channel.Reject(ssh.UnknownChannelType, "not used")
			}
		}()
		clientSide, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		deadlineErr := errors.New("clear deadline fixture failure")
		tracked := &deadlineTrackingConn{Conn: clientSide, failOn: 2, failure: deadlineErr}
		_, err = newWindowsSSHClientFromConnection(context.Background(), tracked, listener.Addr().String(), &ssh.ClientConfig{
			User: "fixture", HostKeyCallback: trustedHostKeyCallback(hostSigner.PublicKey()), Timeout: 5 * time.Second})
		if !errors.Is(err, deadlineErr) || !tracked.wasClosed() {
			t.Fatalf("deadline-clear failure was not wrapped and closed: closed=%t err=%v", tracked.wasClosed(), err)
		}
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Fatal("SSH server did not exit after deadline-clear failure closed the connection")
		}
	})

	t.Run("stalled handshake closes raw connection", func(t *testing.T) {
		clientSide, serverSide := net.Pipe()
		defer serverSide.Close()
		tracked := &closeTrackingConn{Conn: clientSide, closed: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := newWindowsSSHClientFromConnection(ctx, tracked, "fixture:22", &ssh.ClientConfig{
				User: "fixture", HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error {
					return errBYOHHostKeyMismatch
				}, Timeout: time.Second})
			result <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case err := <-result:
			if err == nil || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("stalled handshake cancellation was not classified: %v", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("stalled SSH handshake did not return promptly")
		}
		select {
		case <-tracked.closed:
		default:
			t.Fatal("stalled SSH handshake did not close the raw connection")
		}
	})

	t.Run("stalled session open closes client and joins opener", func(t *testing.T) {
		closed := make(chan struct{})
		openerExited := make(chan struct{})
		var once sync.Once
		client := &windowsSSHClient{
			newSession: func() (sshCommandSession, error) {
				<-closed
				close(openerExited)
				return nil, errors.New("client closed")
			},
			closeFn: func() error {
				once.Do(func() { close(closed) })
				return nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := runPowerShellOverSSH(ctx, client, "Write-Output 'never runs'")
		if err == nil || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("stalled session cancellation was not prompt: elapsed=%v err=%v", time.Since(started), err)
		}
		select {
		case <-openerExited:
		default:
			t.Fatal("session opener goroutine remained blocked after cancellation")
		}
	})

	t.Run("stalled command closes transport and joins worker", func(t *testing.T) {
		transportClosed := make(chan struct{})
		workerExited := make(chan struct{})
		var once sync.Once
		session := &blockingCommandSession{transportClosed: transportClosed, workerExited: workerExited}
		client := &windowsSSHClient{
			newSession: func() (sshCommandSession, error) { return session, nil },
			closeFn: func() error {
				once.Do(func() { close(transportClosed) })
				return nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := runPowerShellOverSSH(ctx, client, "Write-Output 'blocked'")
		if err == nil || time.Since(started) > 250*time.Millisecond {
			t.Fatalf("stalled command cancellation was not prompt: elapsed=%v err=%v", time.Since(started), err)
		}
		select {
		case <-transportClosed:
		default:
			t.Fatal("stalled command cancellation did not close the transport")
		}
		select {
		case <-workerExited:
		default:
			t.Fatal("command worker remained blocked after cancellation returned")
		}
	})
}
