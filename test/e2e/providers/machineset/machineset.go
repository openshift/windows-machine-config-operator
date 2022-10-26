package machineset

import (
	mapi "github.com/openshift/api/machine/v1beta1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/openshift/windows-machine-config-operator/controllers"
	"github.com/openshift/windows-machine-config-operator/test/e2e/clusterinfo"
)

// New returns a new MachineSet for use with the e2e test suite
func New(rawProvider []byte, infrastructureName string, replicas int32, withIgnoreLabel bool, withPrefix string) *mapi.MachineSet {
	machineSetName := machineSetName(withIgnoreLabel, withPrefix)
	matchLabels := map[string]string{
		mapi.MachineClusterIDLabel:   infrastructureName,
		clusterinfo.MachineE2ELabel:  "true",
		clusterinfo.MachineOSIDLabel: "Windows",
	}
	if withIgnoreLabel {
		matchLabels[controllers.IgnoreLabel] = "true"
	}
	matchLabels[clusterinfo.MachineSetLabel] = machineSetName

	machineLabels := map[string]string{
		clusterinfo.MachineRoleLabel: "worker",
		clusterinfo.MachineTypeLabel: "worker",
	}
	// append matchlabels to machinelabels
	for k, v := range matchLabels {
		machineLabels[k] = v
	}

	// Set up the test machineSet
	return &mapi.MachineSet{
		ObjectMeta: meta.ObjectMeta{
			Name:      machineSetName,
			Namespace: clusterinfo.MachineAPINamespace,
			Labels: map[string]string{
				mapi.MachineClusterIDLabel:  infrastructureName,
				clusterinfo.MachineE2ELabel: "true",
			},
		},
		Spec: mapi.MachineSetSpec{
			Selector: meta.LabelSelector{
				MatchLabels: matchLabels,
			},
			Replicas: &replicas,
			Template: mapi.MachineTemplateSpec{
				ObjectMeta: mapi.ObjectMeta{Labels: machineLabels},
				Spec: mapi.MachineSpec{
					ObjectMeta: mapi.ObjectMeta{
						Labels: map[string]string{
							"node-role.kubernetes.io/worker": "",
						},
					},
					ProviderSpec: mapi.ProviderSpec{
						Value: &runtime.RawExtension{Raw: rawProvider},
					},
				},
			},
		},
	}
}

// machineSetName returns the name of the Windows MachineSet with the specified prefix created in the e2e tests
// depending on if the ignore label is set or not
func machineSetName(isIgnoreLabelSet bool, prefix string) string {
	if isIgnoreLabelSet {
		return prefix + "e2e"
	}
	// Designate MachineSets that will be configured by the Windows Machine controller "e2e-wm"
	return prefix + "e2e-wm"
}
