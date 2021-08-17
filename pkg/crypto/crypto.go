package crypto

import (
	"bytes"
	"io"
	"io/ioutil"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
)

const (
	wmcoMarker = "<wmcoMarker>"
)

// EncryptToJSONString returns the encrypted JSON text value of the given string
func EncryptToJSONString(plaintext string, key []byte) (string, error) {
	encryptedData, err := encrypt(plaintext, key)
	if err != nil {
		return "", err
	}
	// Make encrypted string compatible as JSON Patch request body. Encryption introduces line breaks to the data,
	// so the newline characters are marked with text placeholder in order to be easily reversed during decryption
	return strings.Replace(encryptedData, "\n", wmcoMarker, -1), nil
}

// DecryptFromJSONString returns the plaintext string value of encrypted JSON text
func DecryptFromJSONString(encryptedData string, key []byte) (string, error) {
	// Convert data from JSON compatible representation to string value
	encryptedData = strings.Replace(encryptedData, wmcoMarker, "\n", -1)
	return decrypt(encryptedData, key)
}

// encrypt creates an encrypted block of text from a plaintext message using the given key
func encrypt(plaintext string, key []byte) (string, error) {
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

// decrypt converts encrypted contents to plaintext using the given key as a passphrase
func decrypt(encrypted string, key []byte) (string, error) {
	if key == nil {
		return "", errors.New("decryption passphrase cannot be empty")
	}

	// Unwrap encoded block holding the message content
	msgBuffer := bytes.NewBuffer([]byte(encrypted))
	armorBlock, err := armor.Decode(msgBuffer)
	if err != nil {
		return "", errors.Wrapf(err, "unable to process given data %s", encrypted)
	}

	msgBody, err := readMessage(armorBlock.Body, key)
	if err != nil {
		return "", err
	}
	plainTextBytes, err := ioutil.ReadAll(msgBody)
	if err != nil {
		return "", errors.Wrap(err, "unable to parse decrypted data into a readable value")
	}
	return string(plainTextBytes), nil
}

// readMessage attempts to read symmetrically encrypted data from the given reader
func readMessage(reader io.Reader, key []byte) (io.Reader, error) {
	// Flag needed to signal if the key has already been used and failed, else "function will be called again, forever"
	// documentation: https://godoc.org/golang.org/x/crypto/openpgp#PromptFunction
	attempted := false
	promptFunction := func(keys []openpgp.Key, symmetric bool) ([]byte, error) {
		if attempted {
			return nil, errors.New("invalid passphrase supplied")
		}
		attempted = true
		return key, nil
	}
	message, err := openpgp.ReadMessage(reader, nil, promptFunction, nil)
	if err != nil {
		return nil, errors.Wrap(err, "unable to decrypt message using given key")
	}
	return message.UnverifiedBody, nil
}
