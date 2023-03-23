package signer

import (
	"fmt"

	"golang.org/x/crypto/ssh"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/secrets"
)

// Create creates a signer using the private key data
func Create(secret kubeTypes.NamespacedName, c client.Client) (ssh.Signer, error) {
	privateKey, err := secrets.GetPrivateKey(secret, c)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse private key: %w", err)
	}
	return signer, nil
}
