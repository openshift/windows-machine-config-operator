package windows

import (
	"testing"

	config "github.com/openshift/api/config/v1"
)

func TestWindows_parseHostname(t *testing.T) {
	type args struct {
		urlString string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "given empty urlString should error",
			args:    args{urlString: ""},
			want:    "",
			wantErr: true,
		},
		{
			name:    "given invalid urlString should error",
			args:    args{urlString: "invalid https://api.cluster.openshift.com:6443"},
			want:    "",
			wantErr: true,
		},
		{
			name:    "given urlString without scheme should error",
			args:    args{urlString: "api.cluster.openshift.com:6443"},
			want:    "",
			wantErr: true,
		},
		{
			name:    "given valid urlString should pass",
			args:    args{urlString: "https://api.cluster.openshift.com:6443"},
			want:    "api.cluster.openshift.com",
			wantErr: false,
		},
		{
			name:    "given valid urlString without port should pass",
			args:    args{urlString: "https://api.cluster.openshift.com"},
			want:    "api.cluster.openshift.com",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostname(tt.args.urlString)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseHostname() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("parseHostname() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWindows_getApiServerInternalIpForPlatformStatus(t *testing.T) {
	type args struct {
		platformStatus *config.PlatformStatus
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name:    "given no platformStatus should error",
			args:    args{platformStatus: nil},
			want:    "",
			wantErr: true,
		},
		{
			name: "given unsupported platformStatus.Type should error (BareMetal)",
			args: args{
				platformStatus: &config.PlatformStatus{
					Type: config.BareMetalPlatformType,
					BareMetal: &config.BareMetalPlatformStatus{
						APIServerInternalIP: "1.2.3.4"},
				},
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "given unsupported platformStatus.Type should error (AWS)",
			args: args{
				platformStatus: &config.PlatformStatus{
					Type: config.AWSPlatformType,
					AWS:  &config.AWSPlatformStatus{},
				},
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "given unsupported platformStatus.Type should error (Azure)",
			args: args{
				platformStatus: &config.PlatformStatus{
					Type:  config.AzurePlatformType,
					Azure: &config.AzurePlatformStatus{},
				},
			},
			want:    "",
			wantErr: true,
		},
		{
			name: "given valid platformStatus.Type should pass (VSphere)",
			args: args{
				platformStatus: &config.PlatformStatus{
					Type: config.VSpherePlatformType,
					VSphere: &config.VSpherePlatformStatus{
						APIServerInternalIP: "1.2.3.4"},
				},
			},
			want:    "1.2.3.4",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getApiServerInternalIpForPlatformStatus(tt.args.platformStatus)
			if (err != nil) != tt.wantErr {
				t.Errorf("getApiServerInternalIpForPlatformStatus() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("getApiServerInternalIpForPlatformStatus() got = %v, want %v", got, tt.want)
			}
		})
	}
}
