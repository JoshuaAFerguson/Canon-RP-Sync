// Package config loads rpsync's configuration from a YAML file, environment
// variables and command-line flags, in that order of increasing precedence.
//
// Every field has a working default, so rpsync runs with no configuration at
// all: it discovers the camera over SSDP and imports into a platform-specific
// pictures directory.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the complete daemon configuration.
type Config struct {
	Camera  Camera  `yaml:"camera"`
	Storage Storage `yaml:"storage"`
	Import  Import  `yaml:"import"`
	Server  Server  `yaml:"server"`
	Remote  Remote  `yaml:"remote"`
	Log     Log     `yaml:"log"`

	// path is the file this config was loaded from ("" if defaults only).
	path string
}

// Camera describes how to reach the EOS RP.
type Camera struct {
	// Host pins the camera's address. Empty means discover it on the LAN.
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Discovery enables SSDP search. Disable it on networks that block
	// multicast; Host then becomes required.
	Discovery bool `yaml:"discovery"`
	// DiscoveryTimeout bounds a single SSDP search.
	DiscoveryTimeout Duration `yaml:"discovery_timeout"`
	// PollInterval is how often to look for the camera while it is offline.
	PollInterval Duration `yaml:"poll_interval"`
	// RequestTimeout bounds ordinary (non-long-poll) CCAPI requests.
	RequestTimeout Duration `yaml:"request_timeout"`
}

// Storage describes where imported files land.
type Storage struct {
	Dest string `yaml:"dest"`
	// Manifest is the import ledger. Empty means Dest/.rpsync-manifest.json.
	Manifest string `yaml:"manifest"`
	// Layout is a Go time layout describing the folder structure under Dest.
	Layout string `yaml:"layout"`
	// DateSource selects the timestamp used for Layout: "exif" (capture time,
	// falling back to import time) or "import".
	DateSource string `yaml:"date_source"`
	// StateDir holds device tokens and other runtime state.
	StateDir string `yaml:"state_dir"`
}

// Import controls what gets pulled off the card.
type Import struct {
	Include           []string `yaml:"include"`
	DeleteAfterImport bool     `yaml:"delete_after_import"`
}

// Server configures the local HTTP API the companion apps talk to.
type Server struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
	// AllowLocalUnauthenticated skips bearer-token auth for requests from
	// loopback. Convenient on a desktop, off by default everywhere else.
	AllowLocalUnauthenticated bool `yaml:"allow_local_unauthenticated"`
	// AdvertiseURL overrides the base URL shown for pairing (useful behind a
	// reverse proxy or tunnel).
	AdvertiseURL string `yaml:"advertise_url"`
	// MaxUploadBytes caps a single companion-app upload. 0 means 2 GiB.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
}

// Remote configures reachability from outside the LAN.
type Remote struct {
	// Mode is "none", "tailscale" or "cloudflare".
	Mode       string     `yaml:"mode"`
	Tailscale  Tailscale  `yaml:"tailscale"`
	Cloudflare Cloudflare `yaml:"cloudflare"`
}

// Tailscale reports the daemon's address on a tailnet. rpsync does not run
// tailscaled itself: it reads the address from an existing daemon (the host's,
// or the sidecar container's shared network namespace).
type Tailscale struct {
	// CLI is the tailscale binary to query. Empty means look it up on PATH.
	CLI string `yaml:"cli"`
	// Hostname overrides the detected MagicDNS name.
	Hostname string `yaml:"hostname"`
}

// Cloudflare runs a Cloudflare Tunnel connector as a supervised child process.
type Cloudflare struct {
	// Binary is the cloudflared executable. Empty means look it up on PATH.
	Binary string `yaml:"binary"`
	// Token is a tunnel token from the Cloudflare dashboard. Prefer TokenFile
	// or the RPSYNC_CLOUDFLARE_TOKEN environment variable.
	Token string `yaml:"token"`
	// TokenFile reads the token from disk (Docker/Synology secrets).
	TokenFile string `yaml:"token_file"`
	// Hostname is the public hostname the tunnel maps to this daemon. It is
	// used only to display the right URL; routing is configured in Cloudflare.
	Hostname string `yaml:"hostname"`
	// Managed runs cloudflared as a child process. Set false when a sidecar
	// container or system service already runs it.
	Managed bool `yaml:"managed"`
}

// Log controls diagnostic output.
type Log struct {
	Level string `yaml:"level"` // debug, info, warn, error
}

// Duration is a time.Duration that unmarshals from "10s"-style YAML strings.
type Duration time.Duration

// UnmarshalYAML accepts both "30s" and a bare number of seconds.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!int" || n.Tag == "!!float" {
		var secs float64
		if err := n.Decode(&secs); err != nil {
			return fmt.Errorf("line %d: %q is not a duration", n.Line, n.Value)
		}
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("line %d: %q is not a duration", n.Line, n.Value)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration in its human-readable form.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// Default returns the configuration used when nothing is specified.
func Default() Config {
	return Config{
		Camera: Camera{
			Port:             8080,
			Discovery:        true,
			DiscoveryTimeout: Duration(3 * time.Second),
			PollInterval:     Duration(15 * time.Second),
			RequestTimeout:   Duration(60 * time.Second),
		},
		Storage: Storage{
			Dest:       DefaultDestDir(),
			Layout:     "2006/2006-01-02",
			DateSource: "exif",
			StateDir:   DefaultStateDir(),
		},
		Import: Import{
			Include: []string{"jpg", "cr3"},
		},
		Server: Server{
			Enabled:        true,
			Addr:           ":8787",
			MaxUploadBytes: 2 << 30,
		},
		Remote: Remote{
			Mode:       "none",
			Cloudflare: Cloudflare{Managed: true},
		},
		Log: Log{Level: "info"},
	}
}

// Load reads configuration from path, applies environment overrides and
// normalizes the result. An empty path searches the standard locations; a
// missing file there is not an error. An explicitly requested file that does
// not exist is.
func Load(path string) (Config, error) {
	cfg := Default()

	explicit := path != ""
	if !explicit {
		path = FindConfigFile()
	}
	if path != "" {
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			// yaml.Unmarshal merges into cfg, leaving defaults for absent keys.
			if err := yaml.Unmarshal(b, &cfg); err != nil {
				return cfg, fmt.Errorf("parse %s: %w", path, err)
			}
			cfg.path = path
		case explicit || !os.IsNotExist(err):
			return cfg, fmt.Errorf("read %s: %w", path, err)
		}
	}

	cfg.applyEnv(os.Getenv)
	cfg.Normalize()
	return cfg, cfg.Validate()
}

// Path reports the file this config was loaded from, or "" for defaults.
func (c Config) Path() string { return c.path }

// envBindings maps environment variables to setters. Docker and Synology users
// configure the daemon entirely through these.
func (c *Config) applyEnv(get func(string) string) {
	str := func(key string, set func(string)) {
		if v, ok := lookup(get, key); ok {
			set(v)
		}
	}
	boolean := func(key string, set func(bool)) {
		if v, ok := lookup(get, key); ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				set(b)
			}
		}
	}
	num := func(key string, set func(int)) {
		if v, ok := lookup(get, key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				set(n)
			}
		}
	}
	dur := func(key string, set func(Duration)) {
		if v, ok := lookup(get, key); ok {
			if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
				set(Duration(d))
			}
		}
	}

	str("RPSYNC_CAMERA_HOST", func(v string) { c.Camera.Host = v })
	num("RPSYNC_CAMERA_PORT", func(v int) { c.Camera.Port = v })
	boolean("RPSYNC_CAMERA_DISCOVERY", func(v bool) { c.Camera.Discovery = v })
	dur("RPSYNC_POLL_INTERVAL", func(v Duration) { c.Camera.PollInterval = v })

	str("RPSYNC_DEST", func(v string) { c.Storage.Dest = v })
	str("RPSYNC_MANIFEST", func(v string) { c.Storage.Manifest = v })
	str("RPSYNC_LAYOUT", func(v string) { c.Storage.Layout = v })
	str("RPSYNC_DATE_SOURCE", func(v string) { c.Storage.DateSource = v })
	str("RPSYNC_STATE_DIR", func(v string) { c.Storage.StateDir = v })

	str("RPSYNC_INCLUDE", func(v string) { c.Import.Include = splitList(v) })
	boolean("RPSYNC_DELETE_AFTER_IMPORT", func(v bool) { c.Import.DeleteAfterImport = v })

	boolean("RPSYNC_SERVER_ENABLED", func(v bool) { c.Server.Enabled = v })
	str("RPSYNC_SERVER_ADDR", func(v string) { c.Server.Addr = v })
	boolean("RPSYNC_ALLOW_LOCAL_UNAUTHENTICATED", func(v bool) { c.Server.AllowLocalUnauthenticated = v })
	str("RPSYNC_ADVERTISE_URL", func(v string) { c.Server.AdvertiseURL = v })

	str("RPSYNC_REMOTE_MODE", func(v string) { c.Remote.Mode = v })
	str("RPSYNC_TAILSCALE_HOSTNAME", func(v string) { c.Remote.Tailscale.Hostname = v })
	str("RPSYNC_CLOUDFLARE_TOKEN", func(v string) { c.Remote.Cloudflare.Token = v })
	str("RPSYNC_CLOUDFLARE_TOKEN_FILE", func(v string) { c.Remote.Cloudflare.TokenFile = v })
	str("RPSYNC_CLOUDFLARE_HOSTNAME", func(v string) { c.Remote.Cloudflare.Hostname = v })
	boolean("RPSYNC_CLOUDFLARE_MANAGED", func(v bool) { c.Remote.Cloudflare.Managed = v })

	str("RPSYNC_LOG_LEVEL", func(v string) { c.Log.Level = v })
}

func lookup(get func(string) string, key string) (string, bool) {
	v := get(key)
	if v == "" {
		return "", false
	}
	return v, true
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Normalize fills in derived values and canonicalizes user input. It is safe to
// call more than once.
func (c *Config) Normalize() {
	if c.Camera.Port == 0 {
		c.Camera.Port = 8080
	}
	if c.Camera.DiscoveryTimeout <= 0 {
		c.Camera.DiscoveryTimeout = Duration(3 * time.Second)
	}
	if c.Camera.PollInterval <= 0 {
		c.Camera.PollInterval = Duration(15 * time.Second)
	}
	if c.Camera.RequestTimeout <= 0 {
		c.Camera.RequestTimeout = Duration(60 * time.Second)
	}
	c.Camera.Host = strings.TrimSpace(c.Camera.Host)

	if c.Storage.Dest == "" {
		c.Storage.Dest = DefaultDestDir()
	}
	c.Storage.Dest = expand(c.Storage.Dest)
	if c.Storage.Manifest == "" {
		c.Storage.Manifest = filepath.Join(c.Storage.Dest, ".rpsync-manifest.json")
	}
	c.Storage.Manifest = expand(c.Storage.Manifest)
	if c.Storage.Layout == "" {
		c.Storage.Layout = "2006/2006-01-02"
	}
	c.Storage.DateSource = strings.ToLower(strings.TrimSpace(c.Storage.DateSource))
	if c.Storage.DateSource == "" {
		c.Storage.DateSource = "exif"
	}
	if c.Storage.StateDir == "" {
		c.Storage.StateDir = DefaultStateDir()
	}
	c.Storage.StateDir = expand(c.Storage.StateDir)

	include := c.Import.Include[:0:0]
	for _, e := range c.Import.Include {
		e = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(e, ".")))
		if e != "" {
			include = append(include, e)
		}
	}
	c.Import.Include = include

	if c.Server.Addr == "" {
		c.Server.Addr = ":8787"
	}
	if c.Server.MaxUploadBytes <= 0 {
		c.Server.MaxUploadBytes = 2 << 30
	}
	c.Server.AdvertiseURL = strings.TrimRight(strings.TrimSpace(c.Server.AdvertiseURL), "/")

	c.Remote.Mode = strings.ToLower(strings.TrimSpace(c.Remote.Mode))
	if c.Remote.Mode == "" || c.Remote.Mode == "off" || c.Remote.Mode == "disabled" {
		c.Remote.Mode = "none"
	}
	c.Remote.Cloudflare.TokenFile = expand(c.Remote.Cloudflare.TokenFile)

	c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
}

// Validate reports configuration that cannot work.
func (c Config) Validate() error {
	if c.Camera.Host == "" && !c.Camera.Discovery {
		return fmt.Errorf("camera.host must be set when camera.discovery is false")
	}
	if c.Camera.Port < 1 || c.Camera.Port > 65535 {
		return fmt.Errorf("camera.port %d out of range", c.Camera.Port)
	}
	switch c.Storage.DateSource {
	case "exif", "import":
	default:
		return fmt.Errorf("storage.date_source %q: want \"exif\" or \"import\"", c.Storage.DateSource)
	}
	switch c.Remote.Mode {
	case "none", "tailscale", "cloudflare":
	default:
		return fmt.Errorf("remote.mode %q: want \"none\", \"tailscale\" or \"cloudflare\"", c.Remote.Mode)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level)
	}
	return nil
}

// IncludeSet returns the include list as a lookup set. An empty set means
// "import everything".
func (c Config) IncludeSet() map[string]bool {
	set := make(map[string]bool, len(c.Import.Include))
	for _, e := range c.Import.Include {
		set[e] = true
	}
	return set
}

// CloudflareToken resolves the tunnel token from its file if one is set.
func (c Config) CloudflareToken() (string, error) {
	if f := c.Remote.Cloudflare.TokenFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("read cloudflare token file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(c.Remote.Cloudflare.Token), nil
}

// Marshal renders the configuration as YAML, for `rpsync config`.
func (c Config) Marshal() ([]byte, error) { return yaml.Marshal(c) }
