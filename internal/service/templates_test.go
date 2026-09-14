package service_test

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/service"
)

func baseConfig() service.Config {
	return service.Config{
		Name:        "rpsync",
		DisplayName: "Canon RP Sync",
		Description: "Imports photos from a Canon EOS RP over Wi-Fi.",
		Executable:  "/usr/local/bin/rpsync",
		Args:        []string{"daemon", "--config", "/etc/rpsync/rpsync.yaml"},
		WorkingDir:  "/var/lib/rpsync",
		Env:         map[string]string{"RPSYNC_DEST": "/srv/photos", "RPSYNC_LOG_LEVEL": "info"},
	}
}

func TestSystemdUnit(t *testing.T) {
	cfg := baseConfig()
	cfg.SystemWide = true
	cfg.User = "rpsync"
	unit := service.SystemdUnit(cfg)

	for _, want := range []string{
		"Description=Imports photos from a Canon EOS RP over Wi-Fi.",
		"ExecStart=/usr/local/bin/rpsync daemon --config /etc/rpsync/rpsync.yaml",
		"WorkingDirectory=/var/lib/rpsync",
		"User=rpsync",
		"Restart=always",
		"WantedBy=multi-user.target",
		"After=network-online.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit missing %q:\n%s", want, unit)
		}
	}

	// Environment lines are emitted in a stable order.
	destIdx := strings.Index(unit, "Environment=RPSYNC_DEST=/srv/photos")
	logIdx := strings.Index(unit, "Environment=RPSYNC_LOG_LEVEL=info")
	if destIdx < 0 || logIdx < 0 || destIdx > logIdx {
		t.Errorf("environment ordering is not stable:\n%s", unit)
	}
}

func TestSystemdUserUnit(t *testing.T) {
	cfg := baseConfig()
	cfg.SystemWide = false
	cfg.User = "ignored"
	unit := service.SystemdUnit(cfg)

	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("a user unit should target default.target:\n%s", unit)
	}
	if strings.Contains(unit, "User=") {
		t.Errorf("a user unit must not set User=:\n%s", unit)
	}
}

func TestSystemdUnitQuotesArgumentsWithSpaces(t *testing.T) {
	cfg := baseConfig()
	cfg.Executable = "/Applications/Canon RP Sync/rpsync"
	cfg.Args = []string{"daemon", "--dest", "/Users/josh/My Photos"}

	unit := service.SystemdUnit(cfg)
	if !strings.Contains(unit, `ExecStart="/Applications/Canon RP Sync/rpsync" daemon --dest "/Users/josh/My Photos"`) {
		t.Errorf("paths with spaces were not quoted:\n%s", unit)
	}
}

func TestLaunchdPlistIsValidXML(t *testing.T) {
	cfg := baseConfig()
	body, err := service.LaunchdPlist(cfg, "com.example.rpsync", "/Users/josh/Library/Logs/rpsync")
	if err != nil {
		t.Fatalf("LaunchdPlist: %v", err)
	}

	// It must parse as XML, or launchd will reject it at load time.
	var doc any
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, body)
	}

	text := string(body)
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.example.rpsync</string>",
		"<string>/usr/local/bin/rpsync</string>",
		"<string>daemon</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/Users/josh/Library/Logs/rpsync/rpsync.log</string>",
		"<key>RPSYNC_DEST</key>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist missing %q:\n%s", want, text)
		}
	}
}

func TestLaunchdPlistEscapesSpecialCharacters(t *testing.T) {
	cfg := baseConfig()
	cfg.Args = []string{"daemon", "--dest", "/Users/josh/Photos & Video"}

	body, err := service.LaunchdPlist(cfg, "com.example.rpsync", "/tmp/logs")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Photos & Video") {
		t.Error("the ampersand was not escaped, which would break the plist")
	}
	var doc any
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("plist is not valid XML: %v", err)
	}
}

func TestPlatformIsReported(t *testing.T) {
	if got := service.Platform(); got == "" {
		t.Error("Platform should always name a manager")
	}
}
