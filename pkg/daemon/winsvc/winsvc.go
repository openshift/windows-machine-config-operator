//go:build windows

package winsvc

import (
	"github.com/pkg/errors"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
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
		return service.Start()
	case svc.Stopped:
		_, err = service.Control(svc.Stop)
		return err
	default:
		return errors.New("unexpected state request")
	}
}
