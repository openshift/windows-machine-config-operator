package secrets

import (
	"context"
	"fmt"

	oconfig "github.com/openshift/api/config/v1"
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
	// TLSSecret is the name of the TLS secret that servica-ca-operator creates
	TLSSecret = "windows-machine-config-operator-tls"
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
		return []byte{}, fmt.Errorf("cloud-private-key missing 'private-key.pem' secret")
	}
	return privateKey, nil
}

// GenerateUserData generates the desired value of userdata secret.
func GenerateUserData(platformType oconfig.PlatformType, publicKey ssh.PublicKey) (*core.Secret, error) {
	pubKeyBytes := ssh.MarshalAuthorizedKey(publicKey)
	if pubKeyBytes == nil {
		return nil, fmt.Errorf("failed to retrieve public key using signer")
	}
	userData := processTags(platformType, generateUserDataWithPubKey(platformType, string(pubKeyBytes[:])))
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
func generateUserDataWithPubKey(platformType oconfig.PlatformType, pubKey string) string {
	windowsExporterPort := "9182"
	userData := `function Get-RandomPassword {
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
			New-NetFirewallRule -DisplayName "WindowsExporter" -Direction Inbound -Action Allow -Protocol TCP -LocalPort "` + windowsExporterPort + `" -EdgeTraversalPolicy Allow
			Set-Service -Name sshd -StartupType 'Automatic'
			Start-Service sshd
			(Get-Content -path C:\ProgramData\ssh\sshd_config) | ForEach-Object {
    			$_.replace('#PubkeyAuthentication yes','PubkeyAuthentication yes').replace('#PasswordAuthentication yes','PasswordAuthentication no')
 			} | Set-Content -path C:\ProgramData\ssh\sshd_config
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
			Restart-Service sshd
			`

	if platformType == oconfig.AWSPlatformType {
		userData = appendAwsUserDataConfig(userData)
	}
	return userData
}

// appendAwsUserDataConfig appends the AWS EC2Launch installation script to
// the given userData to persist the instance metadata routes
func appendAwsUserDataConfig(userData string) string {
	// EC2Launch v2 minimum version where the task to persist routes was introduced
	// see https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2launchv2-versions.html#ec2launchv2-version-history
	EC2LaunchMinimumVersion := "2.0.1643"

	// S3 URL for the latest EC2Launch version
	// see https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2launch-v2-install.html#lv2-download-s3
	EC2LaunchS3LatestURL := "https://s3.amazonaws.com/amazon-ec2launch-v2/windows/amd64/latest/AmazonEC2Launch.msi"

	// append the EC2Launch installation script
	userData += `function Install-LatestEC2LaunchV2 {
				# set download dir
				$DownloadDir = "$env:USERPROFILE\Desktop\EC2Launchv2"
				New-Item -Path "$DownloadDir" -ItemType Directory -Force
				# URL for your download location
				$Url = "` + EC2LaunchS3LatestURL + `"
				# set download location
				$DownloadFile = "$DownloadDir" + "\" + $(Split-Path -Path $Url -Leaf)
				# download the agent
				Invoke-WebRequest -Uri $Url -OutFile $DownloadFile
				# run the install
				msiexec /i "$DownloadFile"
			}
			$EC2LaunchMinimumVersion = "` + EC2LaunchMinimumVersion + `"
			$EC2LaunchExeLocation = "$env:ProgramFiles\Amazon\EC2Launch\EC2Launch.exe"
			
			if ( -not (Test-Path -Path $EC2LaunchExeLocation -PathType Leaf) ) {
				Write-Output "EC2Launch binary not found in $EC2LaunchExeLocation, installing..."
			
				Install-LatestEC2LaunchV2
			}
			
			$currentVersion = $(& $EC2LaunchExeLocation version)
			
			# check supported minimum version
			if ($currentVersion -lt $EC2LaunchMinimumVersion) {   
				Write-Output "EC2Launch upgrading from version $currentVersion"
			
				Install-LatestEC2LaunchV2
			
				$currentVersion = $(& $EC2LaunchExeLocation version)
			}
			
			Write-Output "EC2Launch version $currentVersion"
			
			& $EC2LaunchExeLocation run-task add-routes --persistent
			`
	return userData
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

// GenerateServiceAccountTokenSecret returns a pointer to a secret with the given name and namespace with
// ServiceAccountToken type and the given serviceAccountName as the annotation
func GenerateServiceAccountTokenSecret(namespace, serviceAccountName string) *core.Secret {
	return &core.Secret{
		TypeMeta: meta.TypeMeta{},
		ObjectMeta: meta.ObjectMeta{
			Name:        serviceAccountName,
			Namespace:   namespace,
			Annotations: map[string]string{core.ServiceAccountNameKey: serviceAccountName},
		},
		Type: core.SecretTypeServiceAccountToken,
	}
}
