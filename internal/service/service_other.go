//go:build !linux && !darwin && !windows

package service

// Install is unavailable on this platform.
func Install(cfg Config) (Result, error) { return Result{}, ErrNotSupported }

// Uninstall is unavailable on this platform.
func Uninstall(cfg Config) error { return ErrNotSupported }

// Start is unavailable on this platform.
func Start(cfg Config) error { return ErrNotSupported }

// Stop is unavailable on this platform.
func Stop(cfg Config) error { return ErrNotSupported }

// Query is unavailable on this platform.
func Query(cfg Config) (Status, error) { return Status{Manager: "unsupported"}, ErrNotSupported }
