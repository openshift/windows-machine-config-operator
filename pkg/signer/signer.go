package signer

import (
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

// Create creates a signer using the private key from the privateKeyPath
func Create(privateKey []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "unable to parse private key")
	}
	return signer, nil
}
