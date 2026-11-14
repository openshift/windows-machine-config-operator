// wmco-tests-ext is the OTE (OpenShift Tests Extension) binary for WMCO.
// Tests are standard Ginkgo g.Describe/g.It blocks under ote/test/e2e/.
// BuildExtensionTestSpecsFromOpenShiftGinkgoSuite() discovers them automatically,
// so adding a new test only requires a new g.It block -- no registration code needed.
//
// References:
//   - OTE framework: https://github.com/openshift-eng/openshift-tests-extension
//   - Migration epic: https://issues.redhat.com/browse/WINC-1536
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"
	framework "k8s.io/kubernetes/test/e2e/framework"

	winc "github.com/openshift/windows-machine-config-operator/ote/test/e2e"
)

const tlsAdherenceResourcePool = "wmco-tls-adherence"

func main() {
	util.InitStandardFlags()
	if framework.TestContext.Provider == "" {
		fmt.Fprintln(os.Stderr, "The --provider flag is not set. Continuing as if --provider=skeleton had been used.")
		framework.TestContext.Provider = "skeleton"
	}
	framework.AfterReadingAllFlags(&framework.TestContext)

	logs.InitLogs()
	defer logs.FlushLogs()

	registry := e.NewRegistry()
	ext := e.NewExtension("openshift", "payload", "windows-machine-config-operator")

	registerSuites(ext)

	allSpecs, err := g.BuildExtensionTestSpecsFromOpenShiftGinkgoSuite()
	if err != nil {
		panic(fmt.Errorf("couldn't build extension test specs from ginkgo: %w", err))
	}

	componentSpecs := allSpecs.Select(func(spec *et.ExtensionTestSpec) bool {
		for _, loc := range spec.CodeLocations {
			if strings.Contains(loc, "/test/e2e/") &&
				!strings.Contains(loc, "/go/pkg/mod/") &&
				!strings.Contains(loc, "/vendor/") {
				return true
			}
		}
		return false
	})

	configureStartup(componentSpecs, func() error {
		if err := compat_otp.InitTest(false); err != nil {
			return err
		}
		util.WithCleanup(func() {})
		return nil
	}, winc.InitializeBYOHSession)

	componentSpecs.Walk(func(spec *et.ExtensionTestSpec) {
		for label := range spec.Labels {
			if strings.HasPrefix(label, "Platform:") {
				platformName := strings.TrimPrefix(label, "Platform:")
				spec.Include(et.PlatformEquals(platformName))
			}
		}
		re := regexp.MustCompile(`\[platform:([a-z]+)\]`)
		if match := re.FindStringSubmatch(spec.Name); match != nil {
			spec.Include(et.PlatformEquals(match[1]))
		}
		spec.Lifecycle = et.LifecycleBlocking
		if strings.Contains(spec.Name, "[tls-adherence]") {
			if spec.Resources.ResourcePools == nil {
				spec.Resources.ResourcePools = map[string]int{}
			}
			spec.Resources.ResourcePools[tlsAdherenceResourcePool] = 1
		}
	})

	ext.AddSpecs(componentSpecs)
	registry.Register(ext)

	root := &cobra.Command{
		Use:   "wmco-tests-ext",
		Short: "WMCO OpenShift Tests Extension (OTE) binary",
		Long:  "Windows Machine Config Operator Tests",
	}
	root.AddCommand(cmd.DefaultExtensionCommands(registry)...)

	if err := func() error {
		return root.Execute()
	}(); err != nil {
		logs.FlushLogs()
		os.Exit(1)
	}
}

func configureStartup(specs et.ExtensionTestSpecs, initializeCompatibility, initializeBYOH func() error) {
	var compatibilityOnce, preparationOnce sync.Once
	var compatibilityErr, preparationErr error
	initialize := func() error {
		compatibilityOnce.Do(func() { compatibilityErr = initializeCompatibility() })
		return compatibilityErr
	}
	specs.Select(func(spec *et.ExtensionTestSpec) bool {
		return !strings.Contains(spec.Name, "[BYOHPool]")
	}).AddBeforeAll(func() {
		if err := initialize(); err != nil {
			panic(err)
		}
	})
	prepare := func() error {
		preparationOnce.Do(func() {
			if err := initialize(); err != nil {
				preparationErr = fmt.Errorf("BYOH compatibility initialization failed")
				return
			}
			preparationErr = initializeBYOH()
		})
		return preparationErr
	}
	guard := func(run func(context.Context) *et.ExtensionTestResult) func(context.Context) *et.ExtensionTestResult {
		return func(ctx context.Context) *et.ExtensionTestResult {
			if err := prepare(); err != nil {
				return &et.ExtensionTestResult{Result: et.ResultFailed, Error: err.Error()}
			}
			return run(ctx)
		}
	}
	specs.Select(func(spec *et.ExtensionTestSpec) bool {
		return strings.Contains(spec.Name, "[BYOHPool]")
	}).Walk(func(spec *et.ExtensionTestSpec) {
		spec.Run = guard(spec.Run)
		if spec.RunParallel != nil {
			spec.RunParallel = guard(spec.RunParallel)
		}
	})
}

func registerSuites(ext *e.Extension) {
	suites := []e.Suite{
		{
			Name:    "windows-machine-config-operator/conformance/parallel",
			Parents: []string{"openshift/conformance/parallel"},
			Qualifiers: []string{
				`name.contains("[Level0]") && !(name.contains("[Serial]") || name.contains("[Disruptive]"))`,
			},
		},
		{
			Name:    "windows-machine-config-operator/conformance/serial",
			Parents: []string{"openshift/conformance/serial"},
			Qualifiers: []string{
				`name.contains("[Level0]") && name.contains("[Serial]") && !name.contains("[Disruptive]")`,
			},
		},
		{
			Name:       "windows-machine-config-operator/disruptive",
			Parents:    []string{"openshift/disruptive"},
			Qualifiers: []string{`name.contains("[Disruptive]") && !name.contains("[BYOHPool]")`},
		},
		{
			Name:       "windows-machine-config-operator/proxy",
			Parents:    []string{"openshift/disruptive"},
			Qualifiers: []string{`name.contains("[node-proxy]")`},
		},
		{
			Name:          "windows-machine-config-operator/tls-adherence",
			Parents:       []string{"openshift/disruptive"},
			Qualifiers:    []string{`name.contains("[tls-adherence]")`},
			ResourcePools: map[string]int{tlsAdherenceResourcePool: 1},
		},
		{
			Name:       "windows-machine-config-operator/non-disruptive",
			Qualifiers: []string{`!name.contains("[Disruptive]") && !name.contains("[BYOHPool]")`},
		},
		{
			Name:       "windows-machine-config-operator/byoh-pool",
			Qualifiers: []string{`name.contains("[BYOHPool]")`},
		},
		// release-verification keeps release validation focused on stable Smokerun
		// coverage while excluding upgrade/connected/disruptive/serial scopes.
		{
			Name:       "windows-machine-config-operator/release-verification",
			Qualifiers: []string{`name.contains("Smokerun") && !name.contains("ChkUpgrade") && !name.contains("ConnectedOnly") && !name.contains("[Disruptive]") && !name.contains("[Serial]")`},
		},
		{
			Name:       "windows-machine-config-operator/smokerun",
			Qualifiers: []string{`name.contains("Smokerun")`},
		},
		{
			Name: "windows-machine-config-operator/all",
		},
	}
	for _, suite := range suites {
		ext.AddSuite(suite)
	}
}
