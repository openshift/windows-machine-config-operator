package windows

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
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
