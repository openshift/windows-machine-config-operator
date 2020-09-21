package secrets

import (
	"context"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/controller/signer"
)

const (
	// userDataSecret is the name of the userData secret that WMCO creates
	userDataSecret = "windows-user-data"
	// userDataNamespace is the namespace of the userData secret that WMCO creates
	userDataNamespace = "openshift-machine-api"
	// PrivateKeySecret is the name of the private key secret provided by the user
	PrivateKeySecret = "cloud-private-key"
	// PrivateKeySecretKey is the key within the private key secret which holds the private key
	PrivateKeySecretKey = "private-key.pem"
)

// GetPrivateKey fetches the specified secret and extracts the private key data
func GetPrivateKey(secret kubeTypes.NamespacedName, c client.Client) ([]byte, error) {
	// Fetch the private key secret
	privateKeySecret := &core.Secret{}
	if err := c.Get(context.TODO(), secret, privateKeySecret); err != nil {
		// Error reading the object - requeue the request.
		return []byte{}, err
	}
	privateKey, ok := privateKeySecret.Data[PrivateKeySecretKey]
	if !ok {
		return []byte{}, errors.New("cloud-private-key missing 'private-key.pem' secret")
	}
	return privateKey, nil
}

// GenerateUserData generates the desired value of userdata secret.
func GenerateUserData(privateKey []byte) (*core.Secret, error) {
	keySigner, err := signer.Create(privateKey)
	if err != nil {
		return nil, err
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(keySigner.PublicKey())
	if pubKeyBytes == nil {
		return nil, errors.Errorf("failed to retrieve public key using signer")
	}

	// sshd service is started to create the default sshd_config file. This file is modified
	// for enabling publicKey auth and the service is restarted for the changes to take effect.
	userDataSecret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      userDataSecret,
			Namespace: userDataNamespace,
		},
		Data: map[string][]byte{
			"userData": []byte(`<powershell>
			Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
			$firewallRuleName = "ContainerLogsPort"
			$containerLogsPort = "10250"
			New-NetFirewallRule -DisplayName $firewallRuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $containerLogsPort -EdgeTraversalPolicy Allow
			Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force
			Install-Module -Force OpenSSHUtils
			Set-Service -Name ssh-agent -StartupType ‘Automatic’
			Set-Service -Name sshd -StartupType ‘Automatic’
			Start-Service ssh-agent
			Start-Service sshd
			$pubKeyConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
			$pubKeyConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
 			$passwordConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
			$passwordConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$authFileConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys','#AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys'
			$authFileConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$pubKeyLocationConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace 'Match Group administrators','#Match Group administrators'
			$pubKeyLocationConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			Restart-Service sshd
			New-item -Path $env:USERPROFILE -Name .ssh -ItemType Directory -force
			echo "` + string(pubKeyBytes[:]) + `"| Out-File $env:USERPROFILE\.ssh\authorized_keys -Encoding ascii
			</powershell>
			<persist>true</persist>`),
		},
	}

	return userDataSecret, nil
}
