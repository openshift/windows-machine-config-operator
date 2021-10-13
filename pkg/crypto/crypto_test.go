package crypto

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptDecrypt(t *testing.T) {
	simplePassphrase := []byte("Super secr3t!")
	rsaPassphrase, _ := generatePrivateKey()

	testCases := []struct {
		name           string
		input          string
		encryptKey     []byte
		decryptKey     []byte
		expectedOut    string
		expectedEncErr bool
		expectedDecErr bool
	}{
		// error cases
		{
			name:           "nil encryption key",
			input:          "core",
			encryptKey:     nil,
			decryptKey:     simplePassphrase,
			expectedOut:    "",
			expectedEncErr: true,
			expectedDecErr: false,
		},
		{
			name:           "nil decryption key",
			input:          "core",
			encryptKey:     simplePassphrase,
			decryptKey:     nil,
			expectedOut:    "",
			expectedEncErr: false,
			expectedDecErr: true,
		},
		{
			name:           "decrypt attempt with wrong key",
			input:          "core",
			encryptKey:     simplePassphrase,
			decryptKey:     rsaPassphrase,
			expectedOut:    "",
			expectedEncErr: false,
			expectedDecErr: true,
		},
		// happy path
		{
			name:           "simple key",
			input:          "core",
			encryptKey:     simplePassphrase,
			decryptKey:     simplePassphrase,
			expectedOut:    "core",
			expectedEncErr: false,
			expectedDecErr: false,
		},
		{
			name:           "complex key",
			input:          "Administrator_01",
			encryptKey:     rsaPassphrase,
			decryptKey:     rsaPassphrase,
			expectedOut:    "Administrator_01",
			expectedEncErr: false,
			expectedDecErr: false,
		},
		{
			name:           "empty input data",
			input:          "",
			encryptKey:     rsaPassphrase,
			decryptKey:     rsaPassphrase,
			expectedOut:    "",
			expectedEncErr: false,
			expectedDecErr: false,
		},
		{
			name:           "empty key data",
			input:          "Administrator_01",
			encryptKey:     []byte{},
			decryptKey:     []byte{},
			expectedOut:    "Administrator_01",
			expectedEncErr: false,
			expectedDecErr: false,
		},
	}

	// Ensure symmetric key cryptography functions as expected
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			cipherText, err := encrypt(test.input, test.encryptKey)
			if test.expectedEncErr {
				assert.Error(t, err)
				return
			}

			out, err := decrypt(cipherText, test.decryptKey)
			if test.expectedDecErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedOut, out)
		})
	}

	// Ensure decrypting non-encrypted data fails
	t.Run("decrypt non-encrypted data", func(t *testing.T) {
		// Not a PGP armored block of data
		badCipherBlock := "HELLO"
		// Data inside block is not a base64 encoded string
		badCipherData := "-----BEGIN ENCRYPTED DATA-----\n\nbadData\n-----END ENCRYPTED DATA------"

		for _, cipher := range []string{badCipherBlock, badCipherData} {
			_, err := decrypt(cipher, rsaPassphrase)
			assert.Error(t, err)
		}
	})
}

func TestUpgradePathDecrypt(t *testing.T) {
	simplePassphrase := []byte("secret key")
	rsaPassphrase, _ := generatePrivateKey()

	testCases := []struct {
		name           string
		input          string
		encryptKey     []byte
		decryptKey     []byte
		expectedOut    string
		expectedEncErr bool
		expectedDecErr bool
	}{
		// happy path
		{
			name:           "decrypt previous version encrypted key",
			input:          "Administrator",
			encryptKey:     simplePassphrase,
			decryptKey:     simplePassphrase,
			expectedOut:    "Administrator",
			expectedEncErr: false,
			expectedDecErr: false,
		},
		// error case
		{
			name:           "decrypt attempt with wrong key",
			input:          "Administrator",
			encryptKey:     simplePassphrase,
			decryptKey:     rsaPassphrase,
			expectedOut:    "",
			expectedEncErr: false,
			expectedDecErr: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			cipherText, err := generateBuggyEncryptedString(test.input, test.encryptKey)
			if test.expectedEncErr {
				assert.Error(t, err)
				return
			}

			out, err := DecryptFromJSONString(cipherText, test.decryptKey)
			if test.expectedDecErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.expectedOut, out)
		})
	}
}

// generatePrivateKey generates a random RSA private key
func generatePrivateKey() ([]byte, error) {
	var keyData []byte
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, errors.Wrap(err, "error generating key")
	}
	var privateKey = &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	buf := bytes.NewBuffer(keyData)
	err = pem.Encode(buf, privateKey)
	if err != nil {
		return nil, errors.Wrap(err, "error encoding generated private key")
	}
	return buf.Bytes(), nil
}

// generateBuggyEncryptedString encrypt given string and returns it without modifying it
// In older WMCO version encrypted string has extra tags. When we try to decrypt older
// version( WMCO 3.1.0 and below) encrypted string in newer version we need to make sure we are
// handling it properly in our code. This function generates encrypted string with tags and
// helps to validate this case.
func generateBuggyEncryptedString(plaintext string, key []byte) (string, error) {
	if key == nil {
		return "", errors.New("encryption passphrase cannot be nil")
	}

	// Prepare PGP block with a wrapper tag around the encrypted data
	tag := "ENCRYPTED DATA"
	msgBuffer := bytes.NewBuffer(nil)
	encoder, err := armor.Encode(msgBuffer, tag, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create armor block with tag %s", tag)
	}

	writer, err := openpgp.SymmetricallyEncrypt(encoder, key, nil, nil)
	if err != nil {
		return "", errors.Wrap(err, "failure during encryption")
	}
	_, err = writer.Write([]byte(plaintext))
	if err != nil {
		// Prevent leak in case of stream failure
		encoder.Close()
		return "", err
	}
	// Both writers must be closed before reading the bytes written to the buffer
	writer.Close()
	encoder.Close()

	return string(msgBuffer.Bytes()), nil
}
