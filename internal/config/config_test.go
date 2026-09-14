package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "absent.yaml"))
	if err == nil {
		t.Fatal("an explicitly requested missing file should be an error")
	}
	_ = cfg

	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.Camera.Port != 8080 {
		t.Errorf("camera.port = %d, want 8080", cfg.Camera.Port)
	}
	if !cfg.Camera.Discovery {
		t.Error("discovery should default on")
	}
	if cfg.Storage.Layout != "2006/2006-01-02" {
		t.Errorf("layout = %q", cfg.Storage.Layout)
	}
	if got := cfg.Storage.Manifest; filepath.Base(got) != ".rpsync-manifest.json" {
		t.Errorf("manifest = %q, want a default under dest", got)
	}
}

func TestLoadFileMergesOverDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rpsync.yaml")
	body := `
camera:
  host: 192.168.1.42
  poll_interval: 45s
storage:
  dest: ` + dir + `/photos
import:
  include: [JPG, .CR3, mp4]
  delete_after_import: true
server:
  addr: "127.0.0.1:9000"
remote:
  mode: cloudflare
  cloudflare:
    hostname: rp.example.com
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Camera.Host != "192.168.1.42" {
		t.Errorf("host = %q", cfg.Camera.Host)
	}
	if cfg.Camera.PollInterval.D() != 45*time.Second {
		t.Errorf("poll_interval = %s", cfg.Camera.PollInterval)
	}
	// Untouched keys keep their defaults.
	if cfg.Camera.Port != 8080 {
		t.Errorf("port = %d, want the default 8080", cfg.Camera.Port)
	}
	want := []string{"jpg", "cr3", "mp4"}
	for i, e := range want {
		if cfg.Import.Include[i] != e {
			t.Errorf("include[%d] = %q, want %q", i, cfg.Import.Include[i], e)
		}
	}
	if !cfg.Import.DeleteAfterImport {
		t.Error("delete_after_import should be true")
	}
	if cfg.Remote.Mode != "cloudflare" {
		t.Errorf("remote.mode = %q", cfg.Remote.Mode)
	}
	if cfg.Path() != path {
		t.Errorf("Path() = %q, want %q", cfg.Path(), path)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	cfg := Default()
	env := map[string]string{
		"RPSYNC_CAMERA_HOST":         "10.0.0.5",
		"RPSYNC_CAMERA_PORT":         "9090",
		"RPSYNC_INCLUDE":             "jpg, cr3,mp4",
		"RPSYNC_DELETE_AFTER_IMPORT": "true",
		"RPSYNC_POLL_INTERVAL":       "90s",
		"RPSYNC_REMOTE_MODE":         "tailscale",
		"RPSYNC_SERVER_ENABLED":      "false",
	}
	cfg.applyEnv(func(k string) string { return env[k] })
	cfg.Normalize()

	if cfg.Camera.Host != "10.0.0.5" || cfg.Camera.Port != 9090 {
		t.Errorf("camera = %+v", cfg.Camera)
	}
	if cfg.Camera.PollInterval.D() != 90*time.Second {
		t.Errorf("poll_interval = %s", cfg.Camera.PollInterval)
	}
	if len(cfg.Import.Include) != 3 {
		t.Errorf("include = %v", cfg.Import.Include)
	}
	if !cfg.Import.DeleteAfterImport {
		t.Error("delete_after_import not applied")
	}
	if cfg.Server.Enabled {
		t.Error("RPSYNC_SERVER_ENABLED=false should disable the server")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default", func(*Config) {}, false},
		{"no host, no discovery", func(c *Config) { c.Camera.Discovery = false }, true},
		{"host without discovery", func(c *Config) { c.Camera.Discovery = false; c.Camera.Host = "1.2.3.4" }, false},
		{"bad port", func(c *Config) { c.Camera.Port = 70000 }, true},
		{"bad date source", func(c *Config) { c.Storage.DateSource = "guess" }, true},
		{"bad remote mode", func(c *Config) { c.Remote.Mode = "ngrok" }, true},
		{"bad log level", func(c *Config) { c.Log.Level = "loud" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var cfg Config
	if err := unmarshalYAML([]byte("camera:\n  poll_interval: 2m\n  request_timeout: 30\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Camera.PollInterval.D() != 2*time.Minute {
		t.Errorf("poll_interval = %s", cfg.Camera.PollInterval)
	}
	if cfg.Camera.RequestTimeout.D() != 30*time.Second {
		t.Errorf("bare number should be seconds, got %s", cfg.Camera.RequestTimeout)
	}
}

func TestCloudflareTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("  secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.Remote.Cloudflare.TokenFile = tokenPath
	got, err := cfg.CloudflareToken()
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-token" {
		t.Errorf("token = %q", got)
	}
}
