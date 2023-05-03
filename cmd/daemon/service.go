//go:build windows
// +build windows

/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"sync"

	"golang.org/x/sys/windows/svc"
	"k8s.io/klog/v2"
)

type handler struct {
	ctx context.Context
	wg  sync.WaitGroup
}

func initService(ctx context.Context) error {
	h := &handler{
		ctx: ctx,
	}

	var err error
	h.wg.Add(1)
	go func() {
		err = svc.Run("windows-instance-config-daemon", h)
		if err != nil {
			// Failure before Execute() is called
			h.wg.Done()
		}
	}()
	// Wait for h.Execute to be called through the service manager
	h.wg.Wait()

	return err
}

// Execute is a required function of the Handler interface, it will be called when there is a Windows service request
func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	// Set the status of the Windows service to start pending and allow initService() to return
	s <- svc.Status{State: svc.StartPending}
	h.wg.Done()

	// Set the service to running
	cmdsAccepted := svc.AcceptStop | svc.AcceptShutdown
	s <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
	klog.Info("Running as a Windows service")

	// Handle any incoming requests from the service manager
	ctx, cancel := context.WithCancel(h.ctx)

	// break will only end the current select, include this label to break to, in order to break from the loop + select
loop:
	for {
		select {
		case <-ctx.Done():
			s <- svc.Status{State: svc.StopPending}
			break loop
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				// The Interrogate command is always accepted by default
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				klog.Info("Service stopping")
				s <- svc.Status{State: svc.StopPending}
				cancel()
				break loop
			}
		}
	}
	return false, 0
}
