package service

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// Install registers rpsync with the Service Control Manager. It requires an
// elevated prompt, and configures the service to restart itself if it crashes.
func Install(cfg Config) (Result, error) {
	if err := cfg.setDefaults(); err != nil {
		return Result{}, err
	}

	m, err := mgr.Connect()
	if err != nil {
		return Result{}, fmt.Errorf("connect to the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()

	if existing, err := m.OpenService(cfg.Name); err == nil {
		existing.Close()
		return Result{}, fmt.Errorf("service %q is already installed", cfg.Name)
	}

	s, err := m.CreateService(cfg.Name, cfg.Executable, mgr.Config{
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
		StartType:   mgr.StartAutomatic,
		// The network stack needs to be up before discovery can find anything.
		Dependencies: []string{"Tcpip"},
	}, cfg.Args...)
	if err != nil {
		return Result{}, fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Restart on failure rather than leaving imports silently stopped.
	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(recovery, uint32((24 * time.Hour).Seconds())); err != nil {
		// Not fatal: the service is installed and will still run.
		return Result{
			Manager:      "scm",
			Instructions: []string{"could not configure automatic restart: " + err.Error()},
		}, nil
	}

	return Result{
		Manager:      "scm",
		Instructions: []string{"start it with: rpsync service start"},
	}, nil
}

// Uninstall removes the service registration.
func Uninstall(cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(cfg.Name)
	if err != nil {
		return ErrNotInstalled
	}
	defer s.Close()

	// Stop first so the binary is not left running without a registration.
	if status, err := s.Query(); err == nil && status.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err != nil {
			return fmt.Errorf("stop service: %w", err)
		}
		waitForState(s, svc.Stopped, 20*time.Second)
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	return nil
}

// Start starts the installed service.
func Start(cfg Config) error {
	s, m, err := openService(cfg)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	return nil
}

// Stop stops the installed service.
func Stop(cfg Config) error {
	s, m, err := openService(cfg)
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()

	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	waitForState(s, svc.Stopped, 20*time.Second)
	return nil
}

// Query reports whether the service is installed and running.
func Query(cfg Config) (Status, error) {
	st := Status{Manager: "scm"}

	s, m, err := openService(cfg)
	if err != nil {
		if errors.Is(err, ErrNotInstalled) {
			return st, nil
		}
		return st, err
	}
	defer m.Disconnect()
	defer s.Close()

	st.Installed = true
	status, err := s.Query()
	if err != nil {
		return st, err
	}
	st.Running = status.State == svc.Running
	st.Detail = stateName(status.State)
	return st, nil
}

func openService(cfg Config) (*mgr.Service, *mgr.Mgr, error) {
	if err := cfg.setDefaults(); err != nil {
		return nil, nil, err
	}
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the service manager: %w", err)
	}
	s, err := m.OpenService(cfg.Name)
	if err != nil {
		m.Disconnect()
		return nil, nil, ErrNotInstalled
	}
	return s, m, nil
}

func waitForState(s *mgr.Service, want svc.State, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := s.Query()
		if err != nil || status.State == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.PausePending, svc.Paused, svc.ContinuePending:
		return "paused"
	default:
		return "unknown"
	}
}
