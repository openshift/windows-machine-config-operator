package signer

import (
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestValidatePublicKey(t *testing.T) {
	// Strong RSA (2048-bit)
	strongRSAKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate strong RSA key: %v", err)
	}
	strongRSAPub, err := ssh.NewPublicKey(&strongRSAKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to create SSH public key from strong RSA key: %v", err)
	}

	// Weak RSA (1024-bit)
	weakRSAKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("Failed to generate weak RSA key: %v", err)
	}
	weak1024RSAPub, err := ssh.NewPublicKey(&weakRSAKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to create SSH public key from weak RSA key: %v", err)
	}

	// Strong curve ECDSA (P-256)
	strongECDSAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate strong ECDSA key: %v", err)
	}
	strongECDSAPub, err := ssh.NewPublicKey(&strongECDSAKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to create SSH public key from strong ECDSA key: %v", err)
	}

	// Strong Ed25519
	ed25519Pub, ed25519Priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate Ed25519 key: %v", err)
	}
	ed25519SSHPub, err := ssh.NewPublicKey(ed25519Pub)
	if err != nil {
		t.Fatalf("Failed to create SSH public key from Ed25519 key: %v", err)
	}
	_ = ed25519Priv // Suppress unused variable warning

	// Weak DSA
	params := new(dsa.Parameters)
	if err := dsa.GenerateParameters(params, rand.Reader, dsa.L1024N160); err != nil {
		t.Fatalf("Failed to generate DSA params: %v", err)
	}
	dsaKey := new(dsa.PrivateKey)
	dsaKey.Parameters = *params
	if err := dsa.GenerateKey(dsaKey, rand.Reader); err != nil {
		t.Fatalf("Failed to generate DSA key: %v", err)
	}
	dsaPub, err := ssh.NewPublicKey(&dsaKey.PublicKey)
	if err != nil {
		t.Fatalf("Failed to create SSH public key from DSA key: %v", err)
	}

	testCases := []struct {
		name    string
		key     ssh.PublicKey
		wantErr bool
	}{
		{"Strong RSA", strongRSAPub, false},
		{"Weak RSA", weak1024RSAPub, true},
		{"Strong ECDSA", strongECDSAPub, false},
		{"Ed25519", ed25519SSHPub, false},
		{"DSA", dsaPub, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePublicKey(tc.key)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidatePublicKey() error = %v, wantErr %v", err, tc.wantErr)
				return
			}
		})
	}
}

// TestValidateKeyOperation tests the validateKeyOperation function with various key types
func TestValidateKeyOperation(t *testing.T) {
	testCases := []struct {
		name        string
		signerFunc  func() (ssh.Signer, error)
		expectError bool
		description string
	}{
		{
			name: "Valid Ed25519 Key",
			signerFunc: func() (ssh.Signer, error) {
				_, priv, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "Ed25519 key should pass validation",
		},
		{
			name: "Valid RSA 2048 Key",
			signerFunc: func() (ssh.Signer, error) {
				priv, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "RSA 2048-bit key should pass validation",
		},
		{
			name: "Valid RSA 4096 Key",
			signerFunc: func() (ssh.Signer, error) {
				priv, err := rsa.GenerateKey(rand.Reader, 4096)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "RSA 4096-bit key should pass validation",
		},
		{
			name: "Valid ECDSA P-256 Key",
			signerFunc: func() (ssh.Signer, error) {
				priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "ECDSA P-256 key should pass validation",
		},
		{
			name: "Valid ECDSA P-384 Key",
			signerFunc: func() (ssh.Signer, error) {
				priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "ECDSA P-384 key should pass validation",
		},
		{
			name: "Valid ECDSA P-521 Key",
			signerFunc: func() (ssh.Signer, error) {
				priv, err := ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
				if err != nil {
					return nil, err
				}
				return ssh.NewSignerFromKey(priv)
			},
			expectError: false,
			description: "ECDSA P-521 key should pass validation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log(tc.description)

			signer, err := tc.signerFunc()
			if err != nil {
				t.Fatalf("Failed to create signer: %v", err)
			}

			err = validateKeyOperation(signer)
			if tc.expectError && err == nil {
				t.Errorf("validateKeyOperation() expected error but got none")
			} else if !tc.expectError && err != nil {
				t.Errorf("validateKeyOperation() unexpected error: %v", err)
			}

			if err == nil {
				t.Logf("✓ Key type %s passed operation validation", signer.PublicKey().Type())
			}
		})
	}
}

// TestValidateKeyOperationWithMockSigner tests validation behavior with a mock signer
func TestValidateKeyOperationWithMockSigner(t *testing.T) {
	// Create a valid Ed25519 key for the public key
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate Ed25519 key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("Failed to create SSH public key: %v", err)
	}

	testCases := []struct {
		name        string
		signer      ssh.Signer
		expectError bool
		description string
	}{
		{
			name: "Signer returns error on Sign",
			signer: &mockSignerForTest{
				publicKey: sshPub,
				signFunc: func() (*ssh.Signature, error) {
					return nil, fmt.Errorf("mock signing error")
				},
			},
			expectError: true,
			description: "Should fail when signer returns error",
		},
		{
			name: "Signer returns nil signature",
			signer: &mockSignerForTest{
				publicKey: sshPub,
				signFunc: func() (*ssh.Signature, error) {
					return nil, nil
				},
			},
			expectError: true,
			description: "Should fail when signer returns nil signature",
		},
		{
			name: "Signer returns signature with empty format",
			signer: &mockSignerForTest{
				publicKey: sshPub,
				signFunc: func() (*ssh.Signature, error) {
					return &ssh.Signature{
						Format: "",
						Blob:   []byte("test"),
					}, nil
				},
			},
			expectError: true,
			description: "Should fail when signature format is empty",
		},
		{
			name: "Signer returns signature with empty blob",
			signer: &mockSignerForTest{
				publicKey: sshPub,
				signFunc: func() (*ssh.Signature, error) {
					return &ssh.Signature{
						Format: "ssh-ed25519",
						Blob:   []byte{},
					}, nil
				},
			},
			expectError: true,
			description: "Should fail when signature blob is empty",
		},
		{
			name: "Ed25519 signer with wrong signature format",
			signer: &mockSignerForTest{
				publicKey: sshPub,
				signFunc: func() (*ssh.Signature, error) {
					return &ssh.Signature{
						Format: "ssh-rsa",
						Blob:   []byte("test-signature-blob"),
					}, nil
				},
			},
			expectError: true,
			description: "Should fail when Ed25519 key produces wrong signature format",
		},
		{
			name: "Public key that fails to marshal",
			signer: &mockSignerForTest{
				publicKey: &mockPublicKeyCannotMarshal{},
				signFunc: func() (*ssh.Signature, error) {
					return &ssh.Signature{
						Format: "ssh-ed25519",
						Blob:   []byte("test-signature-blob"),
					}, nil
				},
			},
			expectError: true,
			description: "Should fail when public key cannot be marshaled (returns empty slice)",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Log(tc.description)
			err := validateKeyOperation(tc.signer)
			if tc.expectError && err == nil {
				t.Error("expected error but got none")
			} else if !tc.expectError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if err != nil {
				t.Logf("success: %v", err)
			}
		})
	}
}

// mockSignerForTest is a mock implementation of ssh.Signer for testing
type mockSignerForTest struct {
	publicKey ssh.PublicKey
	signFunc  func() (*ssh.Signature, error)
}

func (m *mockSignerForTest) PublicKey() ssh.PublicKey {
	return m.publicKey
}

func (m *mockSignerForTest) Sign(rand io.Reader, data []byte) (*ssh.Signature, error) {
	return m.signFunc()
}

type mockPublicKeyCannotMarshal struct{}

func (m *mockPublicKeyCannotMarshal) Type() string {
	return "ssh-ed25519"
}

func (m *mockPublicKeyCannotMarshal) Marshal() []byte {
	// Return empty slice to simulate marshal failure
	return []byte{}
}

func (m *mockPublicKeyCannotMarshal) Verify(data []byte, sig *ssh.Signature) error {
	return nil
}
