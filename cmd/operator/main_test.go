package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
)

// TestCheckIfRequiredFilesExist tests if checkIfRequiredFilesExist function is throwing appropriate error when some
// of the files required by WMCO are missing
func TestCheckIfRequiredFilesExist(t *testing.T) {
	// required files of WMCO that are missing
	var missingRequiredFiles = []string{
		"/payload/file-1",
		"/payload/file-2",
	}
	err := checkIfRequiredFilesExist(missingRequiredFiles)
	require.Error(t, err, "Function checkIfRequiredFilesExist did not throw an error when it was expected to")
	assert.Contains(t, err.Error(), "could not stat /payload/file-1: stat /payload/file-1: no such file or directory",
		"Expected error message is absent")
	assert.Contains(t, err.Error(), "could not stat /payload/file-2: stat /payload/file-2: no such file or directory",
		"Expected error message is absent")

}

// TestNewCacheOptions tests that only CertificateSigningRequests get a shortened resync period, so that pending CSRs
// are not stuck until the operator restarts, while the rest of the watched objects keep the default behaviour.
func TestNewCacheOptions(t *testing.T) {
	opts := newCacheOptions()
	require.Len(t, opts.ByObject, 1, "only CertificateSigningRequests should have object specific cache options")

	csrConfigured := false
	for obj, byObject := range opts.ByObject {
		_, isCSR := obj.(*certificatesv1.CertificateSigningRequest)
		if !isCSR {
			continue
		}
		csrConfigured = true
		require.NotNil(t, byObject.SyncPeriod, "CertificateSigningRequest resync period must be set")
		assert.Equal(t, csrResyncPeriod, *byObject.SyncPeriod,
			"CertificateSigningRequest resync period is not the expected value")
	}
	assert.True(t, csrConfigured, "CertificateSigningRequests are missing object specific cache options")
}
