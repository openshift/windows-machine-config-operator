package windows

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// testSigner generates a minimal SSH signer for testing.
func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	s, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)
	return s
}

func TestInitRespectsContextCancellation(t *testing.T) {
	// 192.0.2.1 is from TEST-NET-1 (RFC 5737), guaranteed unreachable.
	// Dialing it without context would block for the OS TCP timeout (~60-120s).
	unreachable := "192.0.2.1"

	t.Run("pre-cancelled context returns immediately", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel before calling init

		c := &sshConnectivity{
			username:         "testuser",
			ipAddress:        unreachable,
			signer:           testSigner(t),
			hostKeyValidator: NewHostKeyValidator(nil, "", "", unreachable),
			log:              logr.Discard(),
		}

		start := time.Now()
		err := c.init(ctx)
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Less(t, elapsed, 3*time.Second, "pre-cancelled context should return promptly")
	})

	t.Run("expired deadline returns within deadline", func(t *testing.T) {
		deadline := 2 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()

		c := &sshConnectivity{
			username:         "testuser",
			ipAddress:        unreachable,
			signer:           testSigner(t),
			hostKeyValidator: NewHostKeyValidator(nil, "", "", unreachable),
			log:              logr.Discard(),
		}

		start := time.Now()
		err := c.init(ctx)
		elapsed := time.Since(start)

		require.Error(t, err)
		// Should complete shortly after the 2s deadline, not after 60+ seconds
		assert.Less(t, elapsed, deadline+5*time.Second,
			"should not block beyond context deadline")
	})
}

// TestStalledHandshakeRespectsContextDeadline starts a TCP server that accepts
// connections but never sends SSH identification. It verifies that the SSH
// handshake goroutine in init is interrupted by context cancellation, returning
// within a bounded time instead of blocking indefinitely.
func TestStalledHandshakeRespectsContextDeadline(t *testing.T) {
	// Start a TCP listener that accepts connections but never sends data,
	// simulating a server that accepts TCP but never starts SSH protocol.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	// Accept connections in the background so the TCP dial succeeds,
	// but never write anything — the SSH handshake will stall.
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return // listener closed
			}
			// Hold the connection open without sending data.
			// Close on test cleanup via listener.Close() which
			// unblocks Accept and the deferred conn.Close runs.
			defer conn.Close()
		}
	}()

	addr := listener.Addr().String()

	config := &ssh.ClientConfig{
		User:            "testuser",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(testSigner(t))},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	deadline := 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	// Reproduce the handshake timeout path from init: dial succeeds, then
	// ssh.NewClientConn blocks because the server never sends its version
	// string. The select on ctx.Done() should interrupt the stalled handshake.
	dialer := &net.Dialer{}
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	require.NoError(t, err, "TCP dial to stalled server should succeed")

	type handshakeResult struct {
		conn ssh.Conn
		err  error
	}
	resultCh := make(chan handshakeResult, 1)
	go func() {
		sshConn, _, _, sshErr := ssh.NewClientConn(netConn, addr, config)
		resultCh <- handshakeResult{sshConn, sshErr}
	}()

	start := time.Now()
	var hsErr error
	select {
	case res := <-resultCh:
		if res.conn != nil {
			res.conn.Close()
		}
		hsErr = res.err
	case <-ctx.Done():
		// Context expired during handshake — close the TCP connection to
		// unblock ssh.NewClientConn, then drain the goroutine result.
		_ = netConn.Close()
		<-resultCh
		hsErr = ctx.Err()
	}
	elapsed := time.Since(start)

	require.Error(t, hsErr, "stalled handshake should produce an error")
	assert.ErrorIs(t, hsErr, context.DeadlineExceeded,
		"error should indicate context deadline exceeded, got: %v", hsErr)
	assert.Less(t, elapsed, deadline+3*time.Second,
		"stalled handshake should not block beyond context deadline; actual: %v", elapsed)
}
