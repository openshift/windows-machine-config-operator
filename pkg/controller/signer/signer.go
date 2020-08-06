package signer

import (
	"io/ioutil"

	wkl "github.com/openshift/windows-machine-config-operator/pkg/controller/wellknownlocations"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

// Create creates a signer using the private key from the privateKeyPath
// TODO: As a part of https://issues.redhat.com/browse/WINC-316 , modify CreateSigner to take private key secret
//  and return the signer
func Create() (ssh.Signer, error) {
	privateKeyBytes, err := ioutil.ReadFile(wkl.PrivateKeyPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find private key from path: %v", wkl.PrivateKeyPath)
	}

	signer, err := ssh.ParsePrivateKey(privateKeyBytes)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to parse private key: %v", wkl.PrivateKeyPath)
	}
	return signer, nil
}
