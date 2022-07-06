package cluster

import (
	"context"
	"testing"

	oconfig "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	fakeconfigclient "github.com/openshift/client-go/config/clientset/versioned/fake"
	fakeoperatorclient "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
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
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, meta.PatchOptions{})
				require.Nil(t, err, "network patch should not throw error")
			}
			_, err := networkConfigurationFactory(fakeConfigClient, fakeOperatorClient)
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
// cannot be validated
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
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, meta.PatchOptions{})
				require.Nil(t, err, "network patch should not throw error")
			}

			network, err := networkConfigurationFactory(fakeConfigClient, fakeOperatorClient)
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

	testNetworkConfig := &oconfig.Network{}
	testNetworkConfig.Name = "cluster"
	testNetworkConfig.Spec.NetworkType = networkType
	testNetworkConfig.Spec.ServiceNetwork = serviceNetworks

	testNetworkOperator := &operatorv1.Network{}
	testNetworkOperator.Name = "cluster"

	_, err := fakeConfigClient.ConfigV1().Networks().Create(context.TODO(), testNetworkConfig, meta.CreateOptions{})
	if err != nil {
		return nil, nil
	}
	_, err = fakeOperatorClient.Networks().Create(context.TODO(), testNetworkOperator, meta.CreateOptions{})
	if err != nil {
		return nil, nil
	}
	return fakeConfigClient, fakeOperatorClient
}

// TestIsValidKubernetesVersion tests if validateK8sVersion function throws error if K8s version is not a supported k8s version
func TestIsValidKubernetesVersion(t *testing.T) {
	fakeConfigClient := fakeconfigclient.NewSimpleClientset()
	var tests = []struct {
		name    string
		version string
		error   bool
	}{
		{"cluster version lower than supported version ", "v1.17.1", true},
		{"cluster version equals supported version", "v1.24.0", false},
		{"cluster version equals supported version", "v1.25.4", false},
		{"cluster version greater than supported version ", "v1.26.2", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// specify version to be tested
			fakeConfigClient.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{
				GitVersion: tt.version,
			}
			clusterconfig := config{oclient: fakeConfigClient}
			err := clusterconfig.validateK8sVersion()
			if tt.error {
				require.Error(t, err, "Function getK8sVersion did not throw an error "+
					"when it was expected to")
			} else {
				require.Nil(t, err, "Successful check for valid network type")
			}
		})
	}
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
				_, err := fakeOperatorClient.Networks().Patch(context.TODO(), "cluster", k8stypes.MergePatchType, tt.networkPatch, meta.PatchOptions{})
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

// TestGetDNS tests the DNS server IP generation from a given subnet
func TestGetDNS(t *testing.T) {
	type args struct {
		subnet string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "empty subnet",
			args:    args{subnet: ""},
			want:    "",
			wantErr: true,
		},
		{
			name:    "invalid subnet",
			args:    args{subnet: "invalid"},
			want:    "",
			wantErr: true,
		},
		{
			name:    "no IP in subnet",
			args:    args{subnet: "172.30.0.0/32"},
			want:    "",
			wantErr: true,
		},
		{
			name:    "valid subnet",
			args:    args{subnet: "172.30.0.0/16"},
			want:    "172.30.0.10",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetDNS(tt.args.subnet)
			if (err != nil) != tt.wantErr {
				assert.Errorf(t, err, "error = %v, wantErr %v", err, tt.wantErr)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
