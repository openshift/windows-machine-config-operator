package clusternetwork

import (
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"

	v1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	fakeconfigclient "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakeoperatorclient "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// TestNetworkConfigurationFactory tests if NetworkConfigurationFactory function throws appropriate errors
func TestNetworkConfigurationFactory(t *testing.T) {
	var tests = []struct {
		name         string
		networkType  string
		networkPatch []byte
		errorMessage string
	}{
		{"invalid network type", "OpenShiftSDN", nil, "OpenShiftSDN : network type not supported"},
		{"valid network type", "OVNKubernetes", []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig": ` +
			`{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}],"` +
			`hybridOverlayVXLANPort": 4800}}}}}`), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeConfigClient, fakeOperatorClient := createFakeClients(tt.networkType)
			if tt.networkPatch != nil {
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, metav1.PatchOptions{})
				require.Nil(t, err, "network patch should not throw error")
			}
			_, err := NetworkConfigurationFactory(fakeConfigClient, fakeOperatorClient)
			if tt.errorMessage == "" {
				require.Nil(t, err, "Successful check for valid network type")
			} else {
				require.Error(t, err, "Function networkConfigurationFactory did not throw an error "+
					"when it was expected to")
				assert.Contains(t, err.Error(), tt.errorMessage)
			}
		})
	}
}

// TestNetworkConfigurationValidate tests if validate() method throws error when network is of required type, but network configuration
//cannot be validated
func TestNetworkConfigurationValidate(t *testing.T) {
	var tests = []struct {
		name         string
		networkType  string
		networkPatch []byte
		errorMessage string
	}{
		{"ovnKubernetesConfig not defined", "OVNKubernetes", nil, "cluster is not configured for OVN hybrid networking"},
		{"hybridOverlayConfig not defined", "OVNKubernetes", []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{} }}}`), "cluster is not configured for OVN hybrid networking"},
		{"invalid OVN hybrid networking configuration", "OVNKubernetes", []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig":` +
			`{"hybridClusterNetwork":[]}}}}}`), "invalid OVN hybrid networking configuration"},
		{"valid OVN hybrid networking configuration", "OVNKubernetes", []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig":` +
			`{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}]}}}}}`), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeConfigClient, fakeOperatorClient := createFakeClients(tt.networkType)
			if tt.networkPatch != nil {
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, metav1.PatchOptions{})
				require.Nil(t, err, "network patch should not throw error")
			}

			network, err := NetworkConfigurationFactory(fakeConfigClient, fakeOperatorClient)
			require.Nil(t, err, "networkConfigurationFactory should not throw error")
			err = network.Validate()

			if tt.errorMessage == "" {
				require.Nil(t, err, "Successful check for valid network type")
			} else {
				require.Error(t, err, "Function networkConfigurationFactory did not throw an error "+
					"when it was expected to")
				assert.Equal(t, err.Error(), tt.errorMessage)
			}
		})
	}
}

// CreateFakeClients is a helper function to create fake OpenShift API config and operator clients
func createFakeClients(networkType string) (configclient.Interface, operatorclient.OperatorV1Interface) {
	fakeOperatorClient := fakeoperatorclient.NewSimpleClientset().OperatorV1()
	fakeConfigClient := fakeconfigclient.NewSimpleClientset()
	serviceNetworks := []string{"172.30.0.0/16", "134.20.0.0/16"}

	testNetworkConfig := &v1.Network{}
	testNetworkConfig.Name = "cluster"
	testNetworkConfig.Spec.NetworkType = networkType
	testNetworkConfig.Spec.ServiceNetwork = serviceNetworks

	testNetworkOperator := &operatorv1.Network{}
	testNetworkOperator.Name = "cluster"

	_, err := fakeConfigClient.ConfigV1().Networks().Create(context.TODO(), testNetworkConfig, metav1.CreateOptions{})
	if err != nil {
		return nil, nil
	}
	_, err = fakeOperatorClient.Networks().Create(context.TODO(), testNetworkOperator, metav1.CreateOptions{})
	if err != nil {
		return nil, nil
	}
	return fakeConfigClient, fakeOperatorClient
}

// TestGetVXLANPort checks if the custom VXLAN port is available in the network object
func TestGetVXLANPort(t *testing.T) {
	tests := []struct {
		name         string
		want         string
		networkPatch []byte
		wantErr      bool
	}{
		{
			name: "custom VXLAN",
			want: "4800",
			networkPatch: []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig": ` +
				`{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}],"` +
				`hybridOverlayVXLANPort": 4800}}}}}`),
			wantErr: false,
		},
		{
			name: "no vxlan - expect an empty string",
			want: "",
			networkPatch: []byte(`{"spec":{"defaultNetwork":{"ovnKubernetesConfig":{"hybridOverlayConfig":` +
				`{"hybridClusterNetwork":[{"cidr":"10.132.0.0/14","hostPrefix":23}]}}}}}`),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, fakeOperatorClient := createFakeClients("OVNKubernetes")
			if tt.networkPatch != nil {
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, metav1.PatchOptions{})
				require.Nil(t, err, "network patch should not throw error")
			}
			got, err := getVXLANPort(fakeOperatorClient)
			require.NoError(t, err)
			if (err != nil) != tt.wantErr {
				assert.Errorf(t, err, "getVXLANPort() error = %v, wantErr %v", err, tt.wantErr)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
