package windows

import (
	"github.com/openshift/windows-machine-config-operator/pkg/instances"
	"golang.org/x/crypto/ssh"
	"reflect"
	"testing"
)

func TestBuildWorkerIgnitionEndpointUrl(t *testing.T) {
	type args struct {
		hostname string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "Given valid hostname should succeed",
			args: args{hostname: "api.cluster.domain.com"},
			want: "https://api.cluster.domain.com:22623/config/worker",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildWorkerIgnitionEndpointUrl(tt.args.hostname); got != tt.want {
				t.Errorf("buildWorkerIgnitionEndpointUrl(%s) = %v, want %v", tt.args.hostname, got, tt.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	type args struct {
		apiServerHostname         string
		apiServerInternalHostname string
		vxlanPort                 string
		instance                  *instances.InstanceInfo
		signer                    ssh.Signer
	}
	tests := []struct {
		name    string
		args    args
		want    Windows
		wantErr bool
	}{
		{
			name:    "Given no apiServerHostname should error",
			args:    args{apiServerInternalHostname: "valid.hostname", vxlanPort: "", instance: nil, signer: nil},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Given empty apiServerHostname should error",
			args:    args{apiServerHostname: "", apiServerInternalHostname: "valid.hostname", vxlanPort: "", instance: nil, signer: nil},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Given no apiServerInternalHostname should error",
			args:    args{apiServerHostname: "valid.hostname", vxlanPort: "", instance: nil, signer: nil},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Given empty apiServerInternalHostname should error",
			args:    args{apiServerHostname: "valid.hostname", apiServerInternalHostname: "", vxlanPort: "", instance: nil, signer: nil},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "Given nil instance should error",
			args:    args{apiServerHostname: "valid.hostname", apiServerInternalHostname: "valid.hostname", vxlanPort: "", instance: nil, signer: nil},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.args.apiServerHostname, tt.args.apiServerInternalHostname, tt.args.vxlanPort, tt.args.instance, tt.args.signer)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("New() got = %v, want %v", got, tt.want)
			}
		})
	}
}
