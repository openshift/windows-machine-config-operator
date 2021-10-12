package condition

import (
	"testing"

	"github.com/stretchr/testify/assert"

	operators "github.com/operator-framework/api/pkg/operators/v2"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidate(t *testing.T) {
	testCases := []struct {
		name        string
		inputList   []meta.Condition
		condType    string
		status      meta.ConditionStatus
		expectedOut bool
	}{
		{
			name:        "empty list",
			inputList:   []meta.Condition{},
			condType:    "testType",
			status:      meta.ConditionTrue,
			expectedOut: false,
		},
		{
			name: "condition not found",
			inputList: []meta.Condition{
				{
					Type:   operators.Upgradeable,
					Status: meta.ConditionFalse,
				},
			},
			condType:    "testType",
			status:      meta.ConditionFalse,
			expectedOut: false,
		},
		{
			name: "condition found, wrong status",
			inputList: []meta.Condition{
				{
					Type:    operators.Upgradeable,
					Status:  meta.ConditionFalse,
					Reason:  "reason",
					Message: "msg",
				},
			},
			condType:    operators.Upgradeable,
			status:      meta.ConditionTrue,
			expectedOut: false,
		},
		{
			name: "simple happy path",
			inputList: []meta.Condition{
				{
					Type:    operators.Upgradeable,
					Status:  meta.ConditionFalse,
					Reason:  "reason",
					Message: "msg",
				},
			},
			condType:    operators.Upgradeable,
			status:      meta.ConditionFalse,
			expectedOut: true,
		},
		{
			name: "happy path with multiple conditions",
			inputList: []meta.Condition{
				{
					Type:    operators.Upgradeable,
					Status:  meta.ConditionFalse,
					Reason:  "reason",
					Message: "msg",
				},
				{
					Type:   "testType",
					Status: meta.ConditionUnknown,
				},
			},
			condType:    operators.Upgradeable,
			status:      meta.ConditionFalse,
			expectedOut: true,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			out := validate(test.inputList, test.condType, test.status)
			assert.Equal(t, out, test.expectedOut)
		})
	}
}
