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

// validate performs the actual validation of the public key.
func validate(pubKey ssh.PublicKey) error {
	cryptoPubKey, ok := pubKey.(ssh.CryptoPublicKey)
	if !ok {
		// This case should ideally not be hit with standard SSH keys.
		return fmt.Errorf("unsupported key type: %s", pubKey.Type())
	}

	switch key := cryptoPubKey.CryptoPublicKey().(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return fmt.Errorf("RSA key size is %d bits, which is considered weak. A minimum of 2048 bits is recommended", key.N.BitLen())
		}
	case *dsa.PublicKey:
		return fmt.Errorf("DSA keys are deprecated and considered weak. Please use RSA, ECDSA, or Ed25519")
	case *ecdsa.PublicKey:
		curveName := key.Curve.Params().Name
		switch curveName {
		case "P-256", "P-384", "P-521":
			// Secure curves, do nothing
			// See https://csrc.nist.gov/pubs/fips/186-5/final
		default:
			return fmt.Errorf("ECDSA key uses curve %s, which is not a recommended curve. Recommended curves are P-256, P-384, and P-521", curveName)
		}
	case ed25519.PublicKey:
		// Ed25519 is a secure algorithm
	default:
		return fmt.Errorf("unknown or unsupported public key type: %T", key)
	}

	// the key is considered secure
	return nil
}
