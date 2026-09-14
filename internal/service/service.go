// Package service installs rpsync as a background service using whatever the
// host platform provides: systemd on Linux, launchd on macOS, and the Service
// Control Manager on Windows.
//
// The goal is that `rpsync service install` is the whole story on a consumer
// desktop, while the Docker image stays the path for a NAS.
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultName is the service identifier used when none is given.
const DefaultName = "rpsync"

// ErrNotSupported is returned on platforms without an integration.
var ErrNotSupported = errors.New("service: installation is not supported on this platform")

// ErrNotInstalled is returned when an operation targets a service that is not
// registered.
var ErrNotInstalled = errors.New("service: not installed")

// Config describes the service to install.
type Config struct {
	// Name is the service identifier, e.g. "rpsync".
	Name string
	// DisplayName is shown in service managers.
	DisplayName string
	// Description explains the service to a human.
	Description string
	// Executable is the absolute path to the rpsync binary. Empty means the
	// currently running binary.
	Executable string
	// Args are the arguments passed to the executable, e.g. ["daemon"].
	Args []string
	// WorkingDir is the service's working directory.
	WorkingDir string
	// Env holds extra environment variables.
	Env map[string]string
	// SystemWide installs for all users (requires root/administrator) instead
	// of for the current user only.
	SystemWide bool
	// User runs a system-wide unit as this account. Linux only.
	User string
}

// Result describes what an install did, so the CLI can tell the user where
// things went and what to run next.
type Result struct {
	// Path is the unit/plist file written, where the platform uses one.
	Path string
	// Manager names the platform integration: systemd, launchd or scm.
	Manager string
	// Instructions are next steps for the user, if any.
	Instructions []string
}

// Status is the reported state of an installed service.
type Status struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Manager   string `json:"manager"`
	Detail    string `json:"detail,omitempty"`
}

// setDefaults resolves the pieces the caller left blank.
func (c *Config) setDefaults() error {
	if c.Name == "" {
		c.Name = DefaultName
	}
	if c.DisplayName == "" {
		c.DisplayName = "Canon RP Sync"
	}
	if c.Description == "" {
		c.Description = "Imports photos from a Canon EOS RP over Wi-Fi."
	}
	if c.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate the rpsync binary: %w", err)
		}
		resolved, err := filepath.EvalSymlinks(exe)
		if err == nil {
			exe = resolved
		}
		c.Executable = exe
	}
	if !filepath.IsAbs(c.Executable) {
		abs, err := filepath.Abs(c.Executable)
		if err != nil {
			return err
		}
		c.Executable = abs
	}
	if len(c.Args) == 0 {
		c.Args = []string{"daemon"}
	}
	if c.WorkingDir == "" {
		c.WorkingDir = filepath.Dir(c.Executable)
	}
	return nil
}

// quoteArgs renders arguments for a shell-style ExecStart line, quoting only
// what needs it so generated units stay readable.
func quoteArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$") {
			parts = append(parts, `"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a)+`"`)
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// sortedEnv returns environment entries in a stable order so generated files do
// not churn between runs.
func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	// Small maps; insertion sort keeps this dependency-free and obvious.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Platform names the service manager used on this host.
func Platform() string {
	switch runtime.GOOS {
	case "linux":
		return "systemd"
	case "darwin":
		return "launchd"
	case "windows":
		return "scm"
	default:
		return "unsupported"
	}
}
