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
	ip           string
	errorMessage string
}{
	{name: "invalid policies in cniCfg struct",
		policies:     mockInvalidPolicyFields(),
		serviceCIDR:  "10.128.0.0/14",
		ip:           "10.0.137.49",
		errorMessage: "invalid policy fields in cniConf struct"},

	{name: "incorrect number of cniCfg policies",
		policies:     mockInvalidPolicyCount(),
		serviceCIDR:  "10.128.0.0/14",
		ip:           "10.0.137.49",
		errorMessage: "number of policies cannot be less than 3"},

	{name: "valid policies in cniCfg struct",
		policies:     mockValidPolicies(),
		ip:           "10.0.137.49",
		errorMessage: ""},
}

// TestPopulateCfgPoliciesError tests if populateCfgPolicies function throws appropriate errors when
// the policy struct passed has invalid fields to populate CNI config
func TestPopulateCfgPoliciesError(t *testing.T) {
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := populateCfgPolicies(tt.policies, tt.serviceCIDR, tt.ip)
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
	ip := "10.0.137.49"
	_ = populateCfgPolicies(policies, serviceCIDR, ip)
	if (*policies)[0].Value.Settings.ExceptionList[0] != serviceCIDR || (*policies)[1].Value.Settings.DestinationPrefix != serviceCIDR {
		t.Errorf("error populating policies in CNI config")
	}
}

// mockValidPolicies is a helper function to create a set of valid CNI config policies
// for testing populateCfgPolicies()
func mockValidPolicies() *policies {
	name := "EndPointPolicy"
	exceptionList := []string{"10.132.0.0/16"}
	setting0 := &settings{ExceptionList: exceptionList, DestinationPrefix: "10.132.0.0/16"}
	setting1 := &settings{ProviderAddress: "10.0.137.49"}
	value0 := &value{"OutBoundNAT", *setting0}
	value1 := &value{"SDNRoute", *setting0}
	value2 := &value{"ProviderAddress", *setting1}

	data0 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value0}

	data1 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value1}

	data2 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value2}

	return &policies{data0, data1, data2}
}

// mockInvalidPolicyFields is a helper function to create a set of invalid CNI config policies
// for testing populateCfgPolicies()
func mockInvalidPolicyFields() *policies {
	name := "EndPointPolicy"
	exceptionList := []string{""}
	setting0 := &settings{ExceptionList: exceptionList, DestinationPrefix: "10.132.0.0/16"}
	value0 := &value{"OutBoundNAT", *setting0}
	setting1 := &settings{ExceptionList: exceptionList, DestinationPrefix: ""}
	value1 := &value{"SDNRoute", *setting1}
	setting2 := &settings{ProviderAddress: ""}
	value2 := &value{"ProviderAddress", *setting2}

	policy0 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value0}

	policy1 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value1}

	policy2 := struct {
		Name  string `json:"name"`
		Value value  `json:"value"`
	}{name, *value2}

	return &policies{policy0, policy1, policy2}
}

// mockInvalidPolicyCount is a helper function to create a set of CNI config policies
// with incorrect number of policies
func mockInvalidPolicyCount() *policies {
	policies := *mockValidPolicies()
	if len(policies) > 0 {
		policies = policies[:len(policies)-1]
	}
	return &policies
}
