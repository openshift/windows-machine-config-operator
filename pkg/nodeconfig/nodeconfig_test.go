package nodeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseHostname(t *testing.T) {
	type args struct {
		endpointUrlString string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "Valid endpoint URL should succeed",
			args:    args{endpointUrlString: "https://api-int.abc.devcluster.openshift.com:6443"},
			want:    "api-int.abc.devcluster.openshift.com",
			wantErr: false,
		},
		{
			name:    "Invalid valid endpoint URL should error",
			args:    args{endpointUrlString: "invalid https://api-int.abc.devcluster.openshift.com:6443"},
			want:    "",
			wantErr: true,
		},
		{
			// TODO: remove, looks like this test is covered by test with name "Valid endpoint URL should succeed"
			name:    "Test case with invalid api at the last",
			args:    args{endpointUrlString: "https://api-int.abc.devcluster.openshift.com.api:6443"},
			want:    "api-int.abc.devcluster.openshift.com.api",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostname(tt.args.endpointUrlString)
			if (err != nil) != tt.wantErr {
				assert.Error(t, err)
				return
			}
			if got != tt.want {
				assert.Equal(t, tt.want, got, "parseHostname() got = %v, want %v")
			}
		})
	}
}

func disabled_TestDiscoverKubeAPIServerEndpoints(t *testing.T) {
	tests := []struct {
		name    string
		api     string
		apiInt  string
		want    string
		want1   string
		wantErr bool
	}{
		{
			name:    "Valid API server endpoints should succeed",
			api:     "https://api.abc.devcluster.openshift.com:6443",
			apiInt:  "https://api-int.abc.devcluster.openshift.com:6443",
			want:    "https://api.abc.devcluster.openshift.com:6443",
			want1:   "https://api-int.abc.devcluster.openshift.com:6443",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// arrange

			// TODO: fake
			//  	infra, err := client.ConfigV1().Infrastructures().Get(context.T ODO(), "cluster", meta.GetOptions{})
			//  where
			// 		infra.Status.APIServerURL =  tt.api
			//  and
			// 		infra.Status.APIServerInternalURL =  tt.apiInt

			// act
			got, got1, err := discoverKubeAPIServerEndpoints()

			// assert
			if (err != nil) != tt.wantErr {
				t.Errorf("discoverKubeAPIServerEndpoints() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("discoverKubeAPIServerEndpoints() got = %v, want %v", got, tt.want)
			}
			if got1 != tt.want1 {
				t.Errorf("discoverKubeAPIServerEndpoints() got1 = %v, want %v", got1, tt.want1)
			}
		})
	}
}
