package secrets

import (
	"context"
	"fmt"

	oconfig "github.com/openshift/api/config/v1"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/windows-machine-config-operator/pkg/cluster"
)

const (
	// UserDataSecret is the name of the userData secret that WMCO creates
	UserDataSecret = "windows-user-data"
	// PrivateKeySecret is the name of the private key secret provided by the user
	PrivateKeySecret = "cloud-private-key"
	// PrivateKeySecretKey is the key within the private key secret which holds the private key
	PrivateKeySecretKey = "private-key.pem"
)

// GetPrivateKey fetches the specified secret and extracts the private key data
func GetPrivateKey(secret kubeTypes.NamespacedName, c client.Client) ([]byte, error) {
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
func GenerateUserData(platformType oconfig.PlatformType, publicKey ssh.PublicKey) (*core.Secret, error) {
	pubKeyBytes := ssh.MarshalAuthorizedKey(publicKey)
	if pubKeyBytes == nil {
		return nil, errors.Errorf("failed to retrieve public key using signer")
	}
	userData := processTags(platformType, generateUserDataWithPubKey(string(pubKeyBytes[:])))
	// sshd service is started to create the default sshd_config file. This file is modified
	// for enabling publicKey auth and the service is restarted for the changes to take effect.
	userDataSecret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      UserDataSecret,
			Namespace: cluster.MachineAPINamespace,
		},
		Data: map[string][]byte{
			"userData": []byte(userData),
		},
	}

	return userDataSecret, nil
}

// generateUserDataWithPubKey returns the Windows user data for the given pubKey
func generateUserDataWithPubKey(pubKey string) string {
	return `function Get-RandomPassword {
				Add-Type -AssemblyName 'System.Web'
				return [System.Web.Security.Membership]::GeneratePassword(16, 2)
			}

			# Check if the capi user exists, this will be the case on Azure, and will be used instead of Administrator
			if((Get-LocalUser | Where-Object {$_.Name -eq "capi"}) -eq $null) {
				# The capi user doesn't exist, ensure the Administrator account is enabled if it exists
				# If neither users exist, an error will be written to the console, but the script will still continue
				$UserAccount = Get-LocalUser -Name "Administrator"
				if( ($UserAccount -ne $null) -and (!$UserAccount.Enabled) ) {
					$password = ConvertTo-SecureString Get-RandomPassword -asplaintext -force
					$UserAccount | Set-LocalUser -Password $password
					$UserAccount | Enable-LocalUser
				}
			}

			Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0
			$firewallRuleName = "ContainerLogsPort"
			$containerLogsPort = "10250"
			New-NetFirewallRule -DisplayName $firewallRuleName -Direction Inbound -Action Allow -Protocol TCP -LocalPort $containerLogsPort -EdgeTraversalPolicy Allow
			Set-Service -Name sshd -StartupType 'Automatic'
			Start-Service sshd
			$pubKeyConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PubkeyAuthentication yes','PubkeyAuthentication yes'
			$pubKeyConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
 			$passwordConf = (Get-Content -path C:\ProgramData\ssh\sshd_config) -replace '#PasswordAuthentication yes','PasswordAuthentication yes'
			$passwordConf | Set-Content -Path C:\ProgramData\ssh\sshd_config
			$authorizedKeyFilePath = "$env:ProgramData\ssh\administrators_authorized_keys"
			New-Item -Force $authorizedKeyFilePath
			echo "` + pubKey + `"| Out-File $authorizedKeyFilePath -Encoding ascii
			$acl = Get-Acl C:\ProgramData\ssh\administrators_authorized_keys
			$acl.SetAccessRuleProtection($true, $false)
			$administratorsRule = New-Object system.security.accesscontrol.filesystemaccessrule("Administrators","FullControl","Allow")
			$systemRule = New-Object system.security.accesscontrol.filesystemaccessrule("SYSTEM","FullControl","Allow")
			$acl.SetAccessRule($administratorsRule)
			$acl.SetAccessRule($systemRule)
			$acl | Set-Acl
			Restart-Service sshd`
}

// applyTag surrounds the given data using the tag name with the following
// pattern: <tagName>data</tagName>
func applyTag(tagName, data string) string {
	return fmt.Sprintf("<%s>%s</%s>\n", tagName, data, tagName)
}

// processTags returns the platform-specific userData with the corresponding tags
func processTags(platformType oconfig.PlatformType, userData string) string {
	switch platformType {
	case oconfig.GCPPlatformType:
		// no tag required for GCP
		break
	default:
		// enclose with powershell tag
		userData = applyTag("powershell", userData)
		// append persist tag
		userData = userData + applyTag("persist", "true")
	}
	return userData
}
