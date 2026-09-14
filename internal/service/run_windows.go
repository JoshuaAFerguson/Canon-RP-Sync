package service

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// RunAsService runs fn under the Service Control Manager when the process was
// started by it, cancelling fn's context on a stop or shutdown request. It
// reports false when rpsync was started from a console, so the caller can run
// normally.
func RunAsService(name string, fn func(context.Context) error) (bool, error) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, err
	}
	if name == "" {
		name = DefaultName
	}
	return true, svc.Run(name, &handler{fn: fn})
}

// handler bridges SCM control requests to context cancellation.
type handler struct {
	fn func(context.Context) error
}

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.fn(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-done:
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return false, 1
			}
			return false, 0

		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				// Give an in-flight import a chance to finish cleanly; the SCM
				// timeout is generous and TimeoutStopSec has no equivalent here.
				<-done
				return false, 0
			}
		}
	}
}
