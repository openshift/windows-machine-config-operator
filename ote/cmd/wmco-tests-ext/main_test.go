package main

import (
	"fmt"
	"reflect"
	"testing"

	e "github.com/openshift-eng/openshift-tests-extension/pkg/extension"
	et "github.com/openshift-eng/openshift-tests-extension/pkg/extension/extensiontests"
)

func registeredExtension() *e.Extension {
	ext := e.NewExtension("openshift", "payload", "windows-machine-config-operator")
	registerSuites(ext)
	registry := e.NewRegistry()
	registry.Register(ext)
	return registry.Get(e.DefaultExtension)
}

func TestReleaseVerificationSuite(t *testing.T) {
	t.Parallel()
	ext := registeredExtension()
	suite, err := ext.GetSuite("windows-machine-config-operator/release-verification")
	if err != nil {
		t.Fatal(err)
	}
	if len(suite.Qualifiers) != 1 || len(suite.Parents) != 0 {
		t.Fatalf("suite must have one combined qualifier and no parents: %+v", suite)
	}
	for _, other := range ext.Suites {
		for _, parent := range other.Parents {
			if parent == suite.Name {
				t.Fatalf("%s must not union tests into %s", other.Name, suite.Name)
			}
		}
	}

	exclusions := []string{"ChkUpgrade", "ConnectedOnly", "[Disruptive]", "[Serial]"}
	for mask := 0; mask < 1<<len(exclusions); mask++ {
		for _, smokerun := range []bool{false, true} {
			name := "[OTP][sig-windows] Windows_Containers case"
			if smokerun {
				name += " Smokerun"
			}
			for index, exclusion := range exclusions {
				if mask&(1<<index) != 0 {
					name += " " + exclusion
				}
			}
			t.Run(fmt.Sprintf("smokerun=%t/exclusions=%04b", smokerun, mask), func(t *testing.T) {
				assertSuiteSelects(t, suite, name, ext.Component.Identifier(), smokerun && mask == 0)
			})
		}
	}
	for _, test := range []struct {
		name string
		want bool
	}{
		{"Smokerun-Author:example-Critical-12345-case", true},
		{"Author:example-Smokerun-Medium-12345-case", true},
		{"Smokerun [Serially] [NonDisruptive]", true},
		{"NotSmokerun", true},
		{"smokerun", false},
		{"SmokeRun", false},
		{"[Serial] Smokerun [Disruptive]", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertSuiteSelects(t, suite, test.name, ext.Component.Identifier(), test.want)
		})
	}
	t.Run("other component", func(t *testing.T) {
		assertSuiteSelects(t, suite, "Smokerun", "openshift:payload:other", false)
	})
}

func assertSuiteSelects(t *testing.T, suite *e.Suite, name, source string, want bool) {
	t.Helper()
	specs := et.ExtensionTestSpecs{&et.ExtensionTestSpec{Name: name, Source: source}}
	selected, err := specs.Filter(suite.Qualifiers)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(selected) == 1; got != want {
		t.Fatalf("%s selects %q from %s = %t, want %t", suite.Name, name, source, got, want)
	}
}

func TestExistingSuitesUnchanged(t *testing.T) {
	t.Parallel()
	ext := registeredExtension()
	for _, test := range []struct {
		name      string
		parents   []string
		qualifier string
	}{
		{"conformance/parallel", []string{"openshift/conformance/parallel"}, `name.contains("[Level0]") && !(name.contains("[Serial]") || name.contains("[Disruptive]"))`},
		{"conformance/serial", []string{"openshift/conformance/serial"}, `name.contains("[Level0]") && name.contains("[Serial]") && !name.contains("[Disruptive]")`},
		{"disruptive", []string{"openshift/disruptive"}, `name.contains("[Disruptive]")`},
		{"proxy", []string{"openshift/disruptive"}, `name.contains("[node-proxy]")`},
		{"non-disruptive", nil, `!name.contains("[Disruptive]")`},
		{"smokerun", nil, `name.contains("Smokerun")`},
		{"all", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, err := ext.GetSuite("windows-machine-config-operator/" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			qualifier := fmt.Sprintf("source == %q", ext.Component.Identifier())
			if test.qualifier != "" {
				qualifier = fmt.Sprintf("(%s) && (%s)", qualifier, test.qualifier)
			}
			if !reflect.DeepEqual(suite.Parents, test.parents) || !reflect.DeepEqual(suite.Qualifiers, []string{qualifier}) {
				t.Fatalf("existing suite changed: %+v", suite)
			}
		})
	}
	smokerun, err := ext.GetSuite("windows-machine-config-operator/smokerun")
	if err != nil {
		t.Fatal(err)
	}
	assertSuiteSelects(t, smokerun, "Smokerun ChkUpgrade ConnectedOnly [Disruptive][Serial]", ext.Component.Identifier(), true)
}
