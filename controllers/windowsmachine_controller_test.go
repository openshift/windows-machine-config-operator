package controllers

import (
	"fmt"
	"testing"

	mapi "github.com/openshift/machine-api-operator/pkg/apis/machine/v1beta1"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func strToPtr(str string) *string {
	return &str
}

func TestIsValidMachine(t *testing.T) {
	r := WindowsMachineReconciler{log: logf.Log}
	invalidMachine1 := core.Node{}
	invalidMachine2 := mapi.Machine{}
	invalidMachine2.Name = "invalid_1"
	invalidMachine3 := mapi.Machine{}
	invalidMachine3.Name = "invalid_2"
	invalidMachine3.Status.Phase = strToPtr("running")
	validMachine2 := mapi.Machine{}
	validMachine2.Name = "valid_1"
	validMachine2.Status.Phase = strToPtr("something")
	validMachine2.Status.Addresses = []core.NodeAddress{
		{
			Type:    "InternalIP",
			Address: "127.0.0.1",
		},
	}

	var tests = []struct {
		machineObj     client.Object
		isValidMachine bool
	}{
		{
			machineObj:     &invalidMachine1,
			isValidMachine: false,
		},
		{
			machineObj:     &invalidMachine2,
			isValidMachine: false,
		},
		{
			machineObj:     &invalidMachine3,
			isValidMachine: false,
		},
		{
			machineObj:     &validMachine2,
			isValidMachine: true,
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("case %d", i+1), func(t *testing.T) {
			isValidMachine := r.isValidMachine(test.machineObj)
			require.Equal(t, test.isValidMachine, isValidMachine)
		})
	}

}
