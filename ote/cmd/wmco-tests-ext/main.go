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
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/openshift-eng/openshift-tests-extension/pkg/cmd"
	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
	g "github.com/openshift-eng/openshift-tests-extension/pkg/ginkgo"
	"github.com/openshift/origin/test/extended/util"
	compat_otp "github.com/openshift/origin/test/extended/util/compat_otp"
	"github.com/spf13/cobra"
	"k8s.io/component-base/logs"
	framework "k8s.io/kubernetes/test/e2e/framework"

	_ "github.com/openshift/windows-machine-config-operator/ote/test/e2e"
)

func main() {
	util.InitStandardFlags()
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

	componentSpecs.AddBeforeAll(func() {
		if err := compat_otp.InitTest(false); err != nil {
			panic(err)
		}
		util.WithCleanup(func() {})
	})

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
		spec.Lifecycle = et.LifecycleInforming
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
			Qualifiers: []string{`name.contains("[Disruptive]")`},
		},
		{
			Name:       "windows-machine-config-operator/non-disruptive",
			Qualifiers: []string{`!name.contains("[Disruptive]")`},
		},
		{
			Name: "windows-machine-config-operator/all",
		},
	}
	for _, suite := range suites {
		ext.AddSuite(suite)
	}
}
