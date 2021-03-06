package nodeconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Test_getClusterAddr tests the getClusterAddr function
func Test_getClusterAddr(t *testing.T) {
	type args struct {
		kubeAPIServerEndpoint string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "Valid test case",
			args:    args{kubeAPIServerEndpoint: "https://api-int.abc.devcluster.openshift.com:6443"},
			want:    "api-int.abc.devcluster.openshift.com",
			wantErr: false,
		},
		{
			name:    "Test case with invalid no-api",
			args:    args{kubeAPIServerEndpoint: "https://no-api.abc.devcluster.openshift.com:6443"},
			want:    "",
			wantErr: true,
		},
		{
			name:    "Test case with invalid api at the last",
			args:    args{kubeAPIServerEndpoint: "https://api-int.abc.devcluster.openshift.com.api:6443"},
			want:    "api-int.abc.devcluster.openshift.com.api",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getClusterAddr(tt.args.kubeAPIServerEndpoint)
			if (err != nil) != tt.wantErr {
				assert.Error(t, err)
				return
			}
			if got != tt.want {
				assert.Equal(t, tt.want, got, "getClusterAddr() got = %v, want %v")
			}
		})
	}
}
