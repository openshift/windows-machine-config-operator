package powershell

import (
	"fmt"
	"os/exec"
)

// CommandRunner runs a given powershell command
type CommandRunner interface {
	Run(string) (string, error)
}

// commandRunner implements the CommandRunner interface
type commandRunner struct{}

// Run runs the command with the PowerShell on PATH
func (r *commandRunner) Run(cmd string) (string, error) {
	out, err := exec.Command("powershell", "-ExecutionPolicy", "Bypass", "/c", cmd).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("error running command with output %s: %w", string(out), err)
	}
	return string(out), nil
}

// NewCommandRunner returns a CommandRunner which runs commands through the PowerShell on PATH
func NewCommandRunner() *commandRunner {
	return &commandRunner{}
}
