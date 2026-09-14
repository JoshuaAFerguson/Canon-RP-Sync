package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Install writes a systemd unit and enables it. Running as root installs a
// system unit; otherwise a user unit is written, which needs no privileges.
func Install(cfg Config) (Result, error) {
	if err := cfg.setDefaults(); err != nil {
		return Result{}, err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}

	path, err := unitPath(cfg)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, []byte(SystemdUnit(cfg)), 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", path, err)
	}

	res := Result{Path: path, Manager: "systemd"}
	if err := systemctl(cfg, "daemon-reload"); err != nil {
		res.Instructions = append(res.Instructions,
			"could not reload systemd automatically: "+err.Error(),
			"run: "+systemctlHint(cfg, "daemon-reload"))
		return res, nil
	}
	if err := systemctl(cfg, "enable", cfg.Name+".service"); err != nil {
		res.Instructions = append(res.Instructions, "run: "+systemctlHint(cfg, "enable --now "+cfg.Name))
		return res, nil
	}

	if !cfg.SystemWide {
		res.Instructions = append(res.Instructions,
			"to keep the service running when you are not logged in, run: sudo loginctl enable-linger "+os.Getenv("USER"))
	}
	res.Instructions = append(res.Instructions, "start it with: rpsync service start")
	return res, nil
}

// Uninstall stops, disables and removes the unit.
func Uninstall(cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}

	path, err := unitPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ErrNotInstalled
	}

	// Best effort: a unit that is already stopped must not fail the removal.
	_ = systemctl(cfg, "disable", "--now", cfg.Name+".service")
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	_ = systemctl(cfg, "daemon-reload")
	return nil
}

// Start starts the installed service.
func Start(cfg Config) error { return control(cfg, "start") }

// Stop stops the installed service.
func Stop(cfg Config) error { return control(cfg, "stop") }

func control(cfg Config, verb string) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}
	if path, err := unitPath(cfg); err == nil {
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return ErrNotInstalled
		}
	}
	return systemctl(cfg, verb, cfg.Name+".service")
}

// Query reports whether the service is installed and running.
func Query(cfg Config) (Status, error) {
	if err := cfg.setDefaults(); err != nil {
		return Status{}, err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}
	st := Status{Manager: "systemd"}

	path, err := unitPath(cfg)
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err == nil {
		st.Installed = true
	}

	out, err := systemctlOutput(cfg, "is-active", cfg.Name+".service")
	st.Detail = strings.TrimSpace(out)
	if err == nil && st.Detail == "active" {
		st.Running = true
	}
	return st, nil
}

func unitPath(cfg Config) (string, error) {
	if cfg.SystemWide {
		return filepath.Join("/etc/systemd/system", cfg.Name+".service"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", cfg.Name+".service"), nil
}

func systemctl(cfg Config, args ...string) error {
	_, err := systemctlOutput(cfg, args...)
	return err
}

func systemctlOutput(cfg Config, args ...string) (string, error) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return "", fmt.Errorf("systemctl not found; is this a systemd system? %w", err)
	}
	if !cfg.SystemWide {
		args = append([]string{"--user"}, args...)
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func systemctlHint(cfg Config, rest string) string {
	if cfg.SystemWide {
		return "sudo systemctl " + rest
	}
	return "systemctl --user " + rest
}
