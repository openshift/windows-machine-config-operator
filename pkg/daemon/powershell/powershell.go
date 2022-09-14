package powershell

import (
	"os/exec"

	"github.com/pkg/errors"
)

// CommandRunner runs a given powershell command
type CommandRunner interface {
	Run(string) (string, error)
}

// commandRunner implements the CommandRunner interface
type commandRunner struct{}

// Run runs the command with the PowerShell on PATH
func (r *commandRunner) Run(cmd string) (string, error) {
	out, err := exec.Command("powershell", "/c", cmd).CombinedOutput()
	if err != nil {
		return "", errors.Wrapf(err, "error running command with output %s", string(out))
	}
	return string(out), nil
}

// NewCommandRunner returns a CommandRunner which runs commands through the PowerShell on PATH
func NewCommandRunner() *commandRunner {
	return &commandRunner{}
}
