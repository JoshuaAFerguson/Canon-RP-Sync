package remote_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/remote"
)

func TestModeNoneIsDisabled(t *testing.T) {
	m := remote.New(remote.Config{Mode: remote.ModeNone, Port: 8787})
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := m.Status()
	if s.Mode != "none" || s.State != remote.StateDisabled {
		t.Errorf("status = %+v", s)
	}
}

func TestUnknownModeIsRejected(t *testing.T) {
	m := remote.New(remote.Config{Mode: "ngrok"})
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("want an error for an unsupported mode")
	}
	if s := m.Status(); s.State != remote.StateUnavailable {
		t.Errorf("status = %+v", s)
	}
}

func TestParseTailscaleStatus(t *testing.T) {
	raw := []byte(`{
      "BackendState": "Running",
      "Self": {"DNSName": "nas.tail1234.ts.net.", "TailscaleIPs": ["100.64.1.5", "fd7a::1"], "Online": true}
    }`)
	host, err := remote.ParseTailscaleStatus(raw)
	if err != nil {
		t.Fatalf("ParseTailscaleStatus: %v", err)
	}
	if host != "nas.tail1234.ts.net" {
		t.Errorf("host = %q, want the MagicDNS name without a trailing dot", host)
	}
}

func TestParseTailscaleStatusFallsBackToIP(t *testing.T) {
	raw := []byte(`{"BackendState":"Running","Self":{"DNSName":"","TailscaleIPs":["100.64.1.5"]}}`)
	host, err := remote.ParseTailscaleStatus(raw)
	if err != nil {
		t.Fatal(err)
	}
	if host != "100.64.1.5" {
		t.Errorf("host = %q", host)
	}
}

func TestParseTailscaleStatusReportsStoppedBackend(t *testing.T) {
	raw := []byte(`{"BackendState":"Stopped","Self":{"DNSName":"nas.ts.net."}}`)
	if _, err := remote.ParseTailscaleStatus(raw); err == nil {
		t.Fatal("a stopped tailscale backend should be reported as an error")
	}
}

func TestParseTailscaleStatusRejectsGarbage(t *testing.T) {
	if _, err := remote.ParseTailscaleStatus([]byte("not json")); err == nil {
		t.Fatal("want a parse error")
	}
	if _, err := remote.ParseTailscaleStatus([]byte(`{"BackendState":"Running","Self":{}}`)); err == nil {
		t.Fatal("want an error when no address is reported")
	}
}

func TestTailscaleHostnameOverrideSkipsTheCLI(t *testing.T) {
	m := remote.New(remote.Config{
		Mode: remote.ModeTailscale, Port: 8787,
		TailscaleHostname: "nas.tail1234.ts.net",
		TailscaleCLI:      "/nonexistent/tailscale",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s := m.Status()
	if s.State != remote.StateReady {
		t.Fatalf("status = %+v", s)
	}
	if s.URL != "http://nas.tail1234.ts.net:8787" {
		t.Errorf("URL = %q", s.URL)
	}
}

func TestTailscaleMissingCLIIsReportedNotFatal(t *testing.T) {
	m := remote.New(remote.Config{
		Mode: remote.ModeTailscale, Port: 8787,
		TailscaleCLI: "definitely-not-a-real-binary-xyz",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run should not fail the daemon: %v", err)
	}
	if s := m.Status(); s.State != remote.StateNotInstalled {
		t.Errorf("status = %+v", s)
	}
}

func TestCloudflaredArgs(t *testing.T) {
	named := remote.CloudflaredArgs("secret-token", 8787)
	if strings.Join(named, " ") != "tunnel --no-autoupdate run --token secret-token" {
		t.Errorf("named tunnel args = %v", named)
	}

	quick := remote.CloudflaredArgs("", 8787)
	joined := strings.Join(quick, " ")
	if !strings.Contains(joined, "--url http://127.0.0.1:8787") {
		t.Errorf("quick tunnel args = %v", quick)
	}
	if strings.Contains(joined, "run") {
		t.Errorf("a quick tunnel should not use `run`: %v", quick)
	}
}

func TestCloudflareExternallyManagedReportsHostname(t *testing.T) {
	m := remote.New(remote.Config{
		Mode: remote.ModeCloudflare, Port: 8787,
		CloudflareHost: "rp.example.com", CloudflareManaged: false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	s := m.Status()
	if s.URL != "https://rp.example.com" || s.State != remote.StateReady {
		t.Errorf("status = %+v", s)
	}
}

func TestCloudflareMissingBinaryIsReportedNotFatal(t *testing.T) {
	m := remote.New(remote.Config{
		Mode: remote.ModeCloudflare, Port: 8787, CloudflareManaged: true,
		CloudflaredBinary: "definitely-not-cloudflared-xyz",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("a missing connector must not fail the daemon: %v", err)
	}
	if s := m.Status(); s.State != remote.StateNotInstalled {
		t.Errorf("status = %+v", s)
	}
}

// TestCloudflareQuickTunnelURLIsDetected runs a stand-in connector that prints
// the line cloudflared prints for a quick tunnel.
func TestCloudflareQuickTunnelURLIsDetected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub connector is a shell script")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-cloudflared")
	body := "#!/bin/sh\n" +
		"echo 'INF |  https://brave-tiger-1234.trycloudflare.com  |' >&2\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	m := remote.New(remote.Config{
		Mode: remote.ModeCloudflare, Port: 8787, CloudflareManaged: true,
		CloudflaredBinary: script,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(); s.URL == "https://brave-tiger-1234.trycloudflare.com" {
			if s.State != remote.StateReady {
				t.Errorf("state = %q, want ready", s.State)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("quick tunnel URL was not detected; status = %+v", m.Status())
}

func TestAdvertiseURLOverridesDetection(t *testing.T) {
	m := remote.New(remote.Config{
		Mode: remote.ModeTailscale, Port: 8787,
		TailscaleHostname: "nas.tail1234.ts.net",
		AdvertiseURL:      "https://photos.example.com",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.Status().URL; got != "https://photos.example.com" {
		t.Errorf("URL = %q, want the configured override", got)
	}
}

func TestLocalURLs(t *testing.T) {
	urls := remote.LocalURLs(8787)
	for _, u := range urls {
		if !strings.HasPrefix(u, "http://") || !strings.HasSuffix(u, ":8787") {
			t.Errorf("malformed URL %q", u)
		}
		if strings.Contains(u, "127.0.0.1") {
			t.Errorf("loopback should not be advertised: %q", u)
		}
	}
}
