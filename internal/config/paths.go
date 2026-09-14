package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// InContainer reports whether we look like we are running inside the official
// Docker image, where /photos and /config are the conventional mount points.
func InContainer() bool {
	if os.Getenv("RPSYNC_CONTAINER") != "" {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

// DefaultDestDir is where imports land when nothing is configured.
func DefaultDestDir() string {
	if InContainer() {
		return "/photos"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Pictures", "Canon")
	}
	return filepath.Join(".", "photos")
}

// DefaultStateDir holds device tokens and other runtime state.
//
//	Linux    ~/.local/share/rpsync   (or /var/lib/rpsync when running as root)
//	macOS    ~/Library/Application Support/rpsync
//	Windows  %AppData%\rpsync
//	Docker   /config
func DefaultStateDir() string {
	if InContainer() {
		return "/config"
	}
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return filepath.Join("/var", "lib", "rpsync")
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "rpsync")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".rpsync")
	}
	return filepath.Join(".", ".rpsync")
}

// ConfigSearchPath lists candidate configuration files, most specific first.
func ConfigSearchPath() []string {
	var paths []string
	if v := os.Getenv("RPSYNC_CONFIG"); v != "" {
		paths = append(paths, expand(v))
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, "rpsync.yaml"), filepath.Join(wd, "rpsync.yml"))
	}
	paths = append(paths, filepath.Join(DefaultStateDir(), "rpsync.yaml"))
	if InContainer() {
		paths = append(paths, "/config/rpsync.yaml", "/config/rpsync.yml")
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		paths = append(paths, filepath.Join(dir, "rpsync", "rpsync.yaml"))
	}
	if runtime.GOOS != "windows" {
		paths = append(paths, "/etc/rpsync/rpsync.yaml", "/etc/rpsync.yaml")
	}
	return dedupe(paths)
}

// FindConfigFile returns the first existing file in ConfigSearchPath, or "".
func FindConfigFile() string {
	for _, p := range ConfigSearchPath() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, p := range in {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// expand resolves a leading ~ and any environment variables in a path.
func expand(p string) string {
	if p == "" {
		return p
	}
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			p = filepath.Join(home, strings.TrimLeft(p[1:], `/\`))
		}
	}
	return filepath.Clean(p)
}
