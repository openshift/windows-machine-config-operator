package secrets

import (
	"bytes"
	"context"

	"io/ioutil"

	"github.com/pkg/errors"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
	"golang.org/x/crypto/ssh"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeTypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// UserDataSecret is the name of the userData secret that WMCO creates
	UserDataSecret = "windows-user-data"
	// UserDataNamespace is the namespace of the userData secret that WMCO creates
	UserDataNamespace = "openshift-machine-api"
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
func GenerateUserData(publicKey ssh.PublicKey) (*core.Secret, error) {
	pubKeyBytes := ssh.MarshalAuthorizedKey(publicKey)
	if pubKeyBytes == nil {
		return nil, errors.Errorf("failed to retrieve public key using signer")
	}

	// sshd service is started to create the default sshd_config file. This file is modified
	// for enabling publicKey auth and the service is restarted for the changes to take effect.
	userDataSecret := &core.Secret{
		ObjectMeta: meta.ObjectMeta{
			Name:      UserDataSecret,
			Namespace: UserDataNamespace,
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

// Encrypt attempts to encrpyt a plaintext message using the given key as an encoded passphrase
func Encrypt(plainText string, key []byte) (string, error) {
	msgBuffer := bytes.NewBuffer(nil)

	// prepares PGP block to hold encrpyted data
	encoder, _ := armor.Encode(msgBuffer, "ENCRYPTED DATA", nil)

	serializeSettings := &packet.Config{
		DefaultCipher: packet.CipherAES256,
	}

	writer, _ := openpgp.SymmetricallyEncrypt(encoder, key, nil, serializeSettings)
	_, err := writer.Write([]byte(plainText))
	if err != nil {
		return "", err
	}
	writer.Close()
	encoder.Close()

	cipherText := msgBuffer.Bytes()
	return string(cipherText), nil
}

// Decrypt attempts to convert encrypted contents to plaintext using the given key
func Decrypt(cipherText string, key []byte) (string, error) {
	msgBuffer := bytes.NewBuffer([]byte(cipherText))

	// unwraps encoded message contents
	armorBlock, err := armor.Decode(msgBuffer)
	if err != nil {
		return "", err
	}

	// flag needed to signal if the key has already been used and failed, else "function will be called again, forever"
	// documentation: https://godoc.org/golang.org/x/crypto/openpgp#PromptFunction
	attempted := false
	promptFunction := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		if attempted {
			return nil, errors.New("invalid passphrase supplied")
		}
		attempted = true
		return key, nil
	}

	parseSettings := &packet.Config{
		DefaultCipher: packet.CipherAES256,
	}

	message, err := openpgp.ReadMessage(armorBlock.Body, nil, promptFunction, parseSettings)
	if err != nil {
		return "", errors.Wrap(err, "unable to decrypt message using given key")
	}
	plainText, err := ioutil.ReadAll(message.UnverifiedBody)
	if err != nil {
		return "", errors.Wrap(err, "unable to parse decrypted data into a readable value")
	}

	return string(plainText), nil
}
