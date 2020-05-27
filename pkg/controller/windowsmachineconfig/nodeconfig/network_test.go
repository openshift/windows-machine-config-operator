package nodeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var tests = []struct {
	name         string
	policies     *policies
	serviceCIDR  string
	errorMessage string
}{
	{name: "invalid policies in cniCfg struct",
		policies:     mockInvalidPolicies(),
		serviceCIDR:  "10.128.0.0/14",
		errorMessage: "invalid policy fields in cniConf struct"},

	{name: "valid policies in cniCfg struct",
		policies:     mockValidPolicies(),
		serviceCIDR:  "10.128.0.0/14",
		errorMessage: ""},
}

// TestPopulateCfgPoliciesError tests if populateCfgPolicies function throws appropriate errors when
// the policy struct passed has invalid fields to populate CNI config
func TestPopulateCfgPoliciesError(t *testing.T) {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := populateCfgPolicies(tt.policies, tt.serviceCIDR)
			if tt.errorMessage == "" {
				require.Nil(t, err, "Successful check for invalid CNI config template")
			} else {
				require.Error(t, err, "Function populateCniConfig did not throw an error "+
					"when it was expected to")
				assert.Contains(t, err.Error(), tt.errorMessage)
			}
		})
	}
}

// TestPopulateCfgPoliciesValues tests if populateCfgPolicies function sets appropriate values in
// the CNI config policy struct
func TestPopulateCfgPoliciesValues(t *testing.T) {
	policies := mockValidPolicies()
	serviceCIDR := "10.128.0.0/14"
	_ = populateCfgPolicies(policies, serviceCIDR)
	if (*policies)[0].Value.ExceptionList[0] != serviceCIDR || (*policies)[1].Value.DestinationPrefix != serviceCIDR {
		t.Errorf("error populating policies in CNI config")
	}
}

// mockValidPolicies is a helper function to create a set of valid CNI config policies
// for testing populateCfgPolicies()
func mockValidPolicies() *policies {
	name := "EndPointPolicy"
	exceptionList := []string{"10.132.0.0/16"}

	value0 := &value{ExceptionList: exceptionList, DestinationPrefix: "10.132.0.0/16"}
	value1 := &value{ExceptionList: exceptionList, DestinationPrefix: "10.132.0.0/16"}

	data0 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value0}

	data1 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value1}

	return &policies{data0, data1}
}

// mockValidPolicies is a helper function to create a set of invalid CNI config policies
// for testing populateCfgPolicies()
func mockInvalidPolicies() *policies {
	name := "EndPointPolicy"
	exceptionList := []string{""}

	value0 := &value{ExceptionList: exceptionList, DestinationPrefix: "10.132.0.0/16"}
	value1 := &value{ExceptionList: exceptionList, DestinationPrefix: ""}

	policy0 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value0}

	policy1 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value1}

	return &policies{policy0, policy1}
}
