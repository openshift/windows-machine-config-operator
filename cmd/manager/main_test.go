package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
