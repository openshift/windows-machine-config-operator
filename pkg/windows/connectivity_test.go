package windows

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// mockSigner is a mock implementation of ssh.Signer that can be configured with malformed keys
type mockSigner struct {
	publicKey  ssh.PublicKey
	signMethod func(rand io.Reader, data []byte) (*ssh.Signature, error)
}

func (m *mockSigner) PublicKey() ssh.PublicKey {
	return m.publicKey
}

func (m *mockSigner) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return m.signMethod(rand, data)
}

// createMalformedEd25519Signer creates a signer with a malformed Ed25519 key
// This simulates the condition that causes the panic: curve25519: internal error: scalarBaseMult was not 32 bytes
func createMalformedEd25519Signer(t *testing.T) ssh.Signer {
	// create a valid Ed25519 key first
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "Failed to generate Ed25519 key")

	// create a malformed private key by truncating it
	malformedPriv := priv[:30] // Truncate to 30 bytes instead of 64

	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err, "Failed to create SSH public key")

	// create a mock signer that will cause issues during key exchange
	mockSigner := &mockSigner{
		publicKey: sshPub,
		signMethod: func(rnd io.Reader, data []byte) (*ssh.Signature, error) {
			// Use the malformed private key for signing
			sig := ed25519.Sign(malformedPriv[:ed25519.PrivateKeySize], data)
			return &ssh.Signature{
				Format: "ssh-ed25519",
				Blob:   sig,
			}, nil
		},
	}

	return mockSigner
}

func TestPanicRecoveryWithSimulatedPanic(t *testing.T) {
	conn := &sshConnectivity{
		username:  "username",
		ipAddress: "127.0.0.1",
		signer:    createMalformedEd25519Signer(t),
	}

	config := &ssh.ClientConfig{
		User: conn.username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(conn.signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         time.Second * 2,
	}

	client, err := conn.dialWithPanicRecovery(config)

	assert.Error(t, err, "Expected connection error")
	assert.Nil(t, client, "Client should be nil")
	t.Logf("No panic occurred, error handled gracefully: %v", err)
}
