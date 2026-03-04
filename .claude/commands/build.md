---
description: Build the Windows Machine Config operator and daemon binaries
allowed-tools: Bash
argument-hint: [operator|daemon|all]
---

Build the Windows Machine Config Operator project.

Current status: !`git status --short`

## Build Targets

Build target: ${ARGUMENTS:-operator}

## Execution

1. Validate the build target is one of: operator, daemon, all
2. Check prerequisites:
    - Current status is clean (no uncommitted changes)
    - Go toolchain is available
    - Required dependencies are present
    - Remove any previous or existing binaries to avoid conflicts
3. Execute the appropriate make target:
    - `operator`: make build
    - `daemon`: env GOOS=windows GOARCH=amd64 go build -o "build/_output"/bin/windows-instance-config-daemon.exe ./cmd/daemon
    - `all`: build both operator and daemon
4. Report build status:
    - Show any compilation errors or warnings
    - Confirm successful binary creation and location
    - Display build time and binary size

If build fails, analyze the error and suggest fixes.