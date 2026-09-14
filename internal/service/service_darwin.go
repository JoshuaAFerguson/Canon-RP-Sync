package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// labelPrefix namespaces the launchd job.
const labelPrefix = "com.github.joshuaaferguson."

// Install writes a launchd job. As a normal user this is a LaunchAgent, which
// runs while you are logged in; as root it is a LaunchDaemon that runs at boot.
func Install(cfg Config) (Result, error) {
	if err := cfg.setDefaults(); err != nil {
		return Result{}, err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}

	label := labelPrefix + cfg.Name
	path, logDir, err := plistPath(cfg)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return Result{}, err
	}

	body, err := LaunchdPlist(cfg, label, logDir)
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return Result{}, fmt.Errorf("write %s: %w", path, err)
	}

	res := Result{Path: path, Manager: "launchd"}
	// Replace any previous registration so reinstalling picks up new arguments.
	_ = launchctl("bootout", domain(cfg)+"/"+label)
	if err := launchctl("bootstrap", domain(cfg), path); err != nil {
		res.Instructions = append(res.Instructions,
			"could not load the job automatically: "+err.Error(),
			"run: launchctl bootstrap "+domain(cfg)+" "+path)
		return res, nil
	}
	res.Instructions = append(res.Instructions, "logs are written to "+logDir)
	return res, nil
}

// Uninstall unloads and removes the launchd job.
func Uninstall(cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}

	path, _, err := plistPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ErrNotInstalled
	}
	_ = launchctl("bootout", domain(cfg)+"/"+labelPrefix+cfg.Name)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Start starts the installed job.
func Start(cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}
	if err := ensureInstalled(cfg); err != nil {
		return err
	}
	return launchctl("kickstart", domain(cfg)+"/"+labelPrefix+cfg.Name)
}

// Stop stops the installed job.
func Stop(cfg Config) error {
	if err := cfg.setDefaults(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}
	if err := ensureInstalled(cfg); err != nil {
		return err
	}
	return launchctl("kill", "SIGTERM", domain(cfg)+"/"+labelPrefix+cfg.Name)
}

// Query reports whether the job is installed and running.
func Query(cfg Config) (Status, error) {
	if err := cfg.setDefaults(); err != nil {
		return Status{}, err
	}
	if os.Geteuid() == 0 {
		cfg.SystemWide = true
	}
	st := Status{Manager: "launchd"}

	path, _, err := plistPath(cfg)
	if err != nil {
		return st, err
	}
	if _, err := os.Stat(path); err == nil {
		st.Installed = true
	}

	out, err := exec.Command("launchctl", "print", domain(cfg)+"/"+labelPrefix+cfg.Name).CombinedOutput()
	if err != nil {
		st.Detail = "not loaded"
		return st, nil
	}
	text := string(out)
	st.Running = strings.Contains(text, "state = running")
	st.Detail = strings.TrimSpace(firstLineContaining(text, "state = "))
	return st, nil
}

func ensureInstalled(cfg Config) error {
	path, _, err := plistPath(cfg)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ErrNotInstalled
	}
	return nil
}

func plistPath(cfg Config) (path, logDir string, err error) {
	label := labelPrefix + cfg.Name
	if cfg.SystemWide {
		return filepath.Join("/Library/LaunchDaemons", label+".plist"),
			filepath.Join("/Library/Logs", cfg.Name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		filepath.Join(home, "Library", "Logs", cfg.Name), nil
}

func domain(cfg Config) string {
	if cfg.SystemWide {
		return "system"
	}
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func firstLineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
