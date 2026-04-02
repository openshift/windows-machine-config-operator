package extended

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

const (
	wmcoNamespace = "openshift-windows-machine-config-operator"
)

// CheckWmcoGolangVersion verifies that the golang version reported by the cluster
// matches the version used to build the WMCO binary (OCP-37362).
func CheckWmcoGolangVersion(_ context.Context, oc *cli.CLI) error {
	serverVersion, err := oc.AsAdmin().Output("get", "--raw", "/version")
	if err != nil {
		return fmt.Errorf("failed to get cluster version: %w", err)
	}

	var versionInfo struct {
		GoVersion string `json:"goVersion"`
	}
	if err := json.Unmarshal([]byte(serverVersion), &versionInfo); err != nil {
		return fmt.Errorf("failed to parse /version JSON: %w", err)
	}
	if versionInfo.GoVersion == "" {
		return fmt.Errorf("goVersion not found in /version response")
	}

	parts := strings.Split(versionInfo.GoVersion, ".")
	if len(parts) < 2 {
		return fmt.Errorf("unexpected golang version format: %s", versionInfo.GoVersion)
	}
	truncated := strings.Join(parts[:2], ".")

	logs, err := oc.AsAdmin().Output("logs",
		"deployment.apps/windows-machine-config-operator",
		"-n", wmcoNamespace,
	)
	if err != nil {
		return fmt.Errorf("failed to get WMCO logs: %w", err)
	}

	if !strings.Contains(logs, truncated) {
		return fmt.Errorf("WMCO logs do not contain expected golang version %s (full: %s)",
			truncated, versionInfo.GoVersion)
	}
	return nil
}
