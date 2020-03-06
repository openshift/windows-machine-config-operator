package tracker

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialsEncode(t *testing.T) {
	username := "test"
	password := "secret"
	creds := newCredentials(username, password)

	encodedCreds, err := creds.encode()
	require.NoError(t, err, "error encoding credentials")

	assert.NotContains(t, encodedCreds.data, username, "encoded credentials contains username in plain text")
	assert.NotContains(t, encodedCreds.data, password, "encoded credentials contains password in plain text")
}

func TestCredentialsDecode(t *testing.T) {
	username := "test"
	password := "secret"
	creds := newCredentials(username, password)

	encodedCreds, err := creds.encode()
	require.NoError(t, err, "error encoding credentials")

	decodedCreds, err := encodedCreds.decode()
	require.NoError(t, err, "error decoding credentials")

	assert.Equal(t, creds, decodedCreds, "decoded credentials did not match")
}

func TestCredentialsDecodeErrors(t *testing.T) {
	randomData := []byte("RandomData")
	randomEncodedData := make([]byte, base64.StdEncoding.EncodedLen(len(randomData)))
	base64.StdEncoding.Encode(randomEncodedData, randomData)

	tests := []struct {
		name               string
		inputData          []byte
		expectedErrMessage string
	}{
		{
			name:               "non base64 data returns error on decoding",
			inputData:          randomData,
			expectedErrMessage: "error decoding encoded credentials",
		},
		{
			name:               "random base64 data returns error on decoding",
			inputData:          randomEncodedData,
			expectedErrMessage: "error unmarshaling encoded credentials",
		},
		{
			name:               "nil data returns error on decoding",
			inputData:          nil,
			expectedErrMessage: "encoded data was nil",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encodedCreds := newEncodedCredentials(test.inputData)
			_, err := encodedCreds.decode()
			assert.Error(t, err)
			assert.Contains(t, err.Error(), test.expectedErrMessage)
		})
	}
}
