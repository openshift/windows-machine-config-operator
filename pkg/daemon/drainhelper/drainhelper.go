package drainhelper

import (
	"context"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/drain"
)

// ErrWriter is a wrapper to enable error-level logging inside kubectl drainer implementation
type ErrWriter struct {
	log logr.Logger
}

func (ew ErrWriter) Write(p []byte) (n int, err error) {
	// log error
	ew.log.Error(err, string(p))
	return len(p), nil
}

// OutWriter is a wrapper to enable info-level logging inside kubectl drainer implementation
type OutWriter struct {
	log logr.Logger
}

func (ow OutWriter) Write(p []byte) (n int, err error) {
	// log info
	ow.log.Info(string(p))
	return len(p), nil
}

// NewDrainHelper returns new drain.Helper
func NewDrainHelper(ctx context.Context, client kubernetes.Interface, logger klog.Logger) *drain.Helper {
	return &drain.Helper{
		Ctx:    ctx,
		Client: client,
		ErrOut: &ErrWriter{logger},
		// Evict all pods regardless of their controller and orphan status
		Force: true,
		// Prevents erroring out in case a DaemonSet's pod is on the node
		IgnoreAllDaemonSets: true,
		Out:                 &OutWriter{logger},
	}
}
