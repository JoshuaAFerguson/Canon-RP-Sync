//go:build !windows

package service

import "context"

// RunAsService is a no-op outside Windows: systemd and launchd run rpsync as an
// ordinary foreground process and signal it with SIGTERM.
func RunAsService(name string, fn func(context.Context) error) (bool, error) {
	return false, nil
}
