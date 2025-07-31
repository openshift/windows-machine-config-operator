package signer

import (
	"context"
	"crypto/dsa"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"fmt"

	"golang.org/x/crypto/ssh"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
)

// minRSABitLen is the minimum RSA key size recommended for security.
const minRSABitLen = 2048

// Create creates a signer using the private key data
func Create(ctx context.Context, secret kubeTypes.NamespacedName, c client.Client) (ssh.Signer, error) {
	privateKey, err := secrets.GetPrivateKey(ctx, secret, c)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %w", err)
	}
	return signer, nil
}

// ValidatePublicKey checks if the given public key meets security standards.
// It returns an error if the key is weak.
func ValidatePublicKey(pubKey ssh.PublicKey) error {
	return validate(pubKey)
}

// validate checks the provided ssh.PublicKey for cryptographic strength and compliance with modern security standards.
// It performs the following checks:
//  1. Ensures the key implements ssh.CryptoPublicKey, which exposes the underlying crypto.PublicKey.
//  2. For RSA keys: verifies the modulus bit length is at least minRSABitLen (2048 bits), rejecting weak keys.
//  3. For DSA keys: rejects all, as DSA is deprecated and considered insecure.
//  4. For ECDSA keys: checks the curve used; specifically rejects P-224 as too weak
//  5. For Ed25519 keys: accepts as secure.
//  6. For unknown or unsupported key types: rejects with an error.
//
// Returns nil if the key is considered secure, or an error describing the weakness otherwise.
func validate(pubKey ssh.PublicKey) error {
	cryptoPubKey, ok := pubKey.(ssh.CryptoPublicKey)
	if !ok {
		// This case should ideally not be hit with standard SSH keys.
		return fmt.Errorf("invalid key type: %s", pubKey.Type())
	}

	switch key := cryptoPubKey.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < minRSABitLen {
			return fmt.Errorf("RSA key size is %d bits, which is considered weak. Use %d or greater",
				key.N.BitLen(), minRSABitLen)
		}
	case *dsa.PublicKey:
		return fmt.Errorf("DSA keys are deprecated and considered weak. Please use RSA, ECDSA, or Ed25519")
	case *ecdsa.PublicKey:
		curveName := key.Curve.Params().Name
		// P‑224 is deprecated, too small (~112‑bit) for modern standards and should be phased out by 2030
		if curveName == "P‑224" {
			return fmt.Errorf("found ECDSA key with small curve %s. Use P-256, P-384, P-521 or larger", curveName)
		}
	case ed25519.PublicKey:
		// Ed25519 is a secure algorithm
	default:
		return fmt.Errorf("unknown or unsupported public key type: %T", key)
	}

	// the key is not weak
	return nil
}
