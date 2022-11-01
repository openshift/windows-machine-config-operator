package windows

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitPath(t *testing.T) {
	testCases := []struct {
		name                string
		inputWindowsPath    string
		expectedOutDir      string
		expectedOutFileName string
	}{
		{
			name:                "empty input",
			inputWindowsPath:    "",
			expectedOutDir:      "",
			expectedOutFileName: "",
		},
		{
			name:                "filename only",
			inputWindowsPath:    "test.yml",
			expectedOutDir:      "",
			expectedOutFileName: "test.yml",
		},
		{
			name:                "directory only",
			inputWindowsPath:    "C:\\var\\log\\",
			expectedOutDir:      "C:\\var\\log\\",
			expectedOutFileName: "",
		},
		{
			name:                "full Windows path",
			inputWindowsPath:    "C:\\var\\log\\service.txt",
			expectedOutDir:      "C:\\var\\log\\",
			expectedOutFileName: "service.txt",
		},
		{
			name:                "full linux path",
			inputWindowsPath:    "/home/user/README.md",
			expectedOutDir:      "",
			expectedOutFileName: "/home/user/README.md",
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			dir, fileName := SplitPath(test.inputWindowsPath)
			assert.Equal(t, test.expectedOutDir, dir)
			assert.Equal(t, test.expectedOutFileName, fileName)
		})
	}
}
