//go:build windows

package winsvc

import (
	"github.com/pkg/errors"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/windows-machine-config-operator/pkg/retry"
)

type Service interface {
	Close() error
	Start(...string) error
	Config() (mgr.Config, error)
	Control(svc.Cmd) (svc.Status, error)
	Query() (svc.Status, error)
	UpdateConfig(mgr.Config) error
}

// WaitForState retries until the services reaches the expected state, or reaches timeout
func WaitForState(service Service, state svc.State) error {
	return wait.PollImmediate(retry.WindowsAPIInterval, retry.ResourceChangeTimeout, func() (bool, error) {
		status, err := service.Query()
		if err != nil {
			return false, errors.Wrap(err, "error querying service state")
		}
		return status.State == state, nil
	})
}
