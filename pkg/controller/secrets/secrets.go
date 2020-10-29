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
			Set-Service -Name sshd -StartupType ‘Automatic’
			Start-Service sshd
			$pubKeyConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
			$pubKeyConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
 			$passwordConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
			$passwordConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$authorizedKeyFilePath = "$env:ProgramData\ssh\administrators_authorized_keys"
			New-Item -Force $authorizedKeyFilePath
			echo "` + string(pubKeyBytes[:]) + `"| Out-File $authorizedKeyFilePath -Encoding ascii
			$acl = Get-Acl C:\ProgramData\ssh\administrators_authorized_keys
			$acl.SetAccessRuleProtection($true, $false)
			$administratorsRule = New-Object system.security.accesscontrol.filesystemaccessrule("Administrators","FullControl","Allow")
			$systemRule = New-Object system.security.accesscontrol.filesystemaccessrule("SYSTEM","FullControl","Allow")
			$acl.SetAccessRule($administratorsRule)
			$acl.SetAccessRule($systemRule)
			$acl | Set-Acl
			Restart-Service sshd
			</powershell>
			<persist>true</persist>`),
		},
	}

	return userDataSecret, nil
}
