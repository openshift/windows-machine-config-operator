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

// EnsureServiceState ensures the service is in the given state
func EnsureServiceState(service Service, state svc.State) error {
	status, err := service.Query()
	if err != nil {
		return errors.Wrap(err, "error querying service state")
	}
	if status.State == state {
		return nil
	}
	switch state {
	case svc.Running:
		err = service.Start()
		if err != nil {
			return err
		}
	case svc.Stopped:
		_, err = service.Control(svc.Stop)
		if err != nil {
			return err
		}
	default:
		return errors.New("unexpected state request")
	}
	// Wait for the state change to actually take place
	return wait.PollImmediate(retry.WindowsAPIInterval, retry.ResourceChangeTimeout, func() (bool, error) {
		status, err := service.Query()
		if err != nil {
			return false, errors.Wrap(err, "error querying service state")
		}
		if status.State == state {
			return true, nil
		}
		return false, nil
	})
}
