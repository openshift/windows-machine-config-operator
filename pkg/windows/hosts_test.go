package windows

import (
	"testing"

	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
)

func TestWindows_addHostsEntry(t *testing.T) {
	type vm struct {
		address                string
		workerIgnitionEndpoint string
		signer                 ssh.Signer
		interact               connectivity
		vxlanPort              string
		hostName               string
		log                    logr.Logger
	}
	type args struct {
		ipAddress string
		hostname  string
		comment   string
	}
	tests := []struct {
		name    string
		vm      vm
		args    args
		wantErr bool
	}{
		{
			name:    "given empty ipAddress should error",
			vm:      vm{},
			args:    args{ipAddress: "", hostname: "hostname", comment: "comment"},
			wantErr: true,
		},
		{
			name:    "given empty hostname should error",
			vm:      vm{},
			args:    args{ipAddress: "ipAddress", hostname: "", comment: "comment"},
			wantErr: true,
		},
		// TODO: mock/stub vm.Run() to fully test
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vm := &windows{
				address:                tt.vm.address,
				workerIgnitionEndpoint: tt.vm.workerIgnitionEndpoint,
				signer:                 tt.vm.signer,
				interact:               tt.vm.interact,
				vxlanPort:              tt.vm.vxlanPort,
				hostName:               tt.vm.hostName,
				log:                    tt.vm.log,
			}
			if err := vm.addHostsEntry(tt.args.ipAddress, tt.args.hostname, tt.args.comment); (err != nil) != tt.wantErr {
				t.Errorf("addHostsEntry() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
