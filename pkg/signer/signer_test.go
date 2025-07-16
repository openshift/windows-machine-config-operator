package signer

import (
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
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
