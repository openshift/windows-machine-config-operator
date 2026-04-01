package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CLI wraps the oc command-line tool for use in OTE Ginkgo tests.
type CLI struct {
	namespace  string
	asAdmin    bool
	kubeconfig string
}

// NewCLI creates a CLI instance with the given namespace.
func NewCLI(namespace string) *CLI {
	return &CLI{
		namespace:  namespace,
		kubeconfig: kubeconfig(),
	}
}

// NewCLIWithoutNamespace creates a CLI instance without a namespace (for cluster-scoped resources).
func NewCLIWithoutNamespace() *CLI {
	return NewCLI("")
}

// AsAdmin returns a copy of the CLI that runs as cluster-admin.
func (c *CLI) AsAdmin() *CLI {
	copy := *c
	copy.asAdmin = true
	return &copy
}

// Run executes an oc subcommand with the given arguments and returns stdout, stderr, and any error.
func (c *CLI) Run(verb string, args ...string) (string, string, error) {
	cmdArgs := []string{verb}
	if c.asAdmin {
		cmdArgs = append(cmdArgs, "--as=system:admin")
	}
	if c.namespace != "" {
		cmdArgs = append(cmdArgs, "-n", c.namespace)
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.Command("oc", cmdArgs...)
	if c.kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+c.kubeconfig)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// Output is a convenience wrapper that returns stdout or an error combining stderr.
func (c *CLI) Output(verb string, args ...string) (string, error) {
	out, errOut, err := c.Run(verb, args...)
	if err != nil {
		return "", fmt.Errorf("%w\nstderr: %s", err, errOut)
	}
	return out, nil
}

func kubeconfig() string {
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc
	}
	return filepath.Join(os.Getenv("HOME"), ".kube", "config")
}
