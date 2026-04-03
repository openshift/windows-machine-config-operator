// windows-machine-config-operator-tests-ext is the OTE (OpenShift Tests Extension)
// binary for Windows Containers tests. Tests are registered as plain Go functions
// using upstream Ginkgo, without the OpenShift Ginkgo fork.
//
// References:
//   - OTE Integration Guide: https://github.com/openshift-eng/openshift-tests-extension
package main

import (
	"context"
	"os"

	otecmd "github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	"github.com/spf13/cobra"

	"github.com/openshift/windows-machine-config-operator/ote/test/extended"
	"github.com/openshift/windows-machine-config-operator/ote/test/extended/cli"
)

func main() {
	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "windows-machine-config-operator")

	// All WINC tests - used for full runs and informing jobs.
	// No qualifier needed: all tests in this binary are WINC tests.
	ext.AddSuite(e.Suite{
		Name: "windows/all",
	})

	// Parallel subset: non-Serial, non-Slow, non-Disruptive tests.
	ext.AddSuite(e.Suite{
		Name: "windows/parallel",
		Qualifiers: []string{
			`!name.contains("[Serial]") && !name.contains("[Slow]") && !name.contains("[Disruptive]")`,
		},
	})

	// Serial subset: tests that must run in isolation.
	ext.AddSuite(e.Suite{
		Name: "windows/serial",
		Qualifiers: []string{
			`name.contains("[Serial]")`,
		},
	})

	// Storage-specific tests.
	ext.AddSuite(e.Suite{
		Name: "windows/storage",
		Qualifiers: []string{
			`name.contains("storage")`,
		},
	})

	// Register test specs manually — no OpenShift Ginkgo fork required.
	specs := et.ExtensionTestSpecs{
		{
			Name: "[sig-windows] Windows_Containers Author:rrasouli-Smokerun-Medium-37362-[wmco] wmco using correct golang version [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckWmcoGolangVersion(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
	}

	// Batch 4: WICD, Workloads & Misc (no SSH)
	batch4 := et.ExtensionTestSpecs{
		{
			Name: "[sig-windows] Windows_Containers Author:jfrancoa-Smokerun-Medium-50403-[wmco] wmco creates and maintains Windows services ConfigMap [Disruptive] [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckWicdConfigMap(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
		{
			Name: "[sig-windows] Windows_Containers Author:rrasouli-Smokerun-Medium-60814-[wmco] Check containerd version is properly reported [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckContainerdVersion(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
		{
			Name: "[sig-windows] Windows_Containers Author:sgao-Smokerun-Critical-25593-[wmco] Prevent scheduling non Windows workloads on Windows nodes [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckPreventNonWindowsWorkloads(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
		{
			Name: "[sig-windows] Windows_Containers Author:rrasouli-Smokerun-Medium-42204-[wmco] Create Windows pod with a Projected Volume [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckWindowsPodProjectedVolume(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
		{
			Name: "[sig-windows] Windows_Containers Author:rrasouli-Smokerun-High-38186-[wmco] Windows LB service [Slow] [OTP]",
			Run: func(ctx context.Context) *et.ExtensionTestResult {
				if err := extended.CheckWindowsLBService(ctx, cli.NewCLIWithoutNamespace()); err != nil {
					return &et.ExtensionTestResult{Result: et.ResultFailed, Output: err.Error()}
				}
				return &et.ExtensionTestResult{Result: et.ResultPassed}
			},
		},
	}
	specs = append(specs, batch4...)

	ext.AddSpecs(specs)
	registry.Register(ext)

	root := &cobra.Command{
		Use:   "windows-machine-config-operator-tests-ext",
		Short: "OpenShift Windows Containers test extension (OTE)",
		Long:  "Runs the WINC test suite as an OpenShift Tests Extension binary.",
	}

	root.AddCommand(otecmd.DefaultExtensionCommands(registry)...)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
