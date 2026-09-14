// Package remote makes the daemon reachable from outside the LAN, so the
// companion apps can hand photos over from anywhere.
//
// Two mechanisms are supported, both of which avoid opening a port on the
// router:
//
//	tailscale   the daemon reports its address on your tailnet. rpsync does
//	            not run tailscaled itself — it reads the address from the
//	            host's daemon, or from a sidecar container sharing the
//	            network namespace.
//	cloudflare  rpsync supervises a `cloudflared` connector as a child
//	            process, using a tunnel token, or a quick tunnel when no
//	            token is configured.
//
// Either way the transport is encrypted and authenticated before rpsync's own
// bearer tokens come into play.
package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JoshuaAFerguson/canon-rp-sync/internal/events"
)

// Mode selects the remote-access mechanism.
const (
	ModeNone       = "none"
	ModeTailscale  = "tailscale"
	ModeCloudflare = "cloudflare"
)

// Connection states reported by Status.
const (
	StateDisabled     = "disabled"
	StateStarting     = "starting"
	StateReady        = "ready"
	StateUnavailable  = "unavailable"
	StateNotInstalled = "not installed"
)

// Status describes how the daemon can be reached.
type Status struct {
	Mode    string `json:"mode"`
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Message string `json:"message,omitempty"`
}

// Config configures the manager.
type Config struct {
	Mode string
	// Port is the local port the HTTP server listens on, used to build URLs.
	Port int
	// AdvertiseURL overrides the detected URL entirely.
	AdvertiseURL string

	TailscaleCLI      string
	TailscaleHostname string

	CloudflaredBinary string
	// CloudflareToken authenticates a named tunnel. When empty, a quick tunnel
	// is started instead, which needs no Cloudflare account.
	CloudflareToken   string
	CloudflareHost    string
	CloudflareManaged bool

	Bus    *events.Bus
	Logger *slog.Logger
}

// Manager keeps remote access running and reports its state.
type Manager struct {
	cfg Config
	log *slog.Logger
	bus *events.Bus

	mu     sync.RWMutex
	status Status
}

// New returns a manager for the configured mode.
func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Bus == nil {
		cfg.Bus = events.NewBus()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeNone
	}

	m := &Manager{cfg: cfg, log: cfg.Logger, bus: cfg.Bus}
	m.status = Status{Mode: cfg.Mode, State: StateDisabled}
	if cfg.Mode != ModeNone {
		m.status.State = StateStarting
	}
	if cfg.AdvertiseURL != "" {
		m.status.URL = cfg.AdvertiseURL
	}
	return m
}

// Status returns the current remote-access state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status
}

// Run maintains remote access until ctx is cancelled. It returns nil on a
// clean shutdown: losing a tunnel should never take the importer down with it.
func (m *Manager) Run(ctx context.Context) error {
	switch m.cfg.Mode {
	case ModeNone:
		return nil
	case ModeTailscale:
		return m.runTailscale(ctx)
	case ModeCloudflare:
		return m.runCloudflare(ctx)
	default:
		m.set(Status{Mode: m.cfg.Mode, State: StateUnavailable,
			Message: fmt.Sprintf("unknown remote mode %q", m.cfg.Mode)})
		return fmt.Errorf("remote: unknown mode %q", m.cfg.Mode)
	}
}

func (m *Manager) set(s Status) {
	m.mu.Lock()
	changed := m.status.State != s.State || m.status.URL != s.URL
	if m.cfg.AdvertiseURL != "" {
		s.URL = m.cfg.AdvertiseURL
	}
	m.status = s
	m.mu.Unlock()

	if changed {
		msg := s.State
		if s.URL != "" {
			msg += " at " + s.URL
		}
		if s.Message != "" {
			msg += ": " + s.Message
		}
		m.bus.Publishf(events.KindRemote, "%s %s", s.Mode, msg)
		m.log.Info("remote access", "mode", s.Mode, "state", s.State, "url", s.URL, "message", s.Message)
	}
}

// ---------------------------------------------------------------- tailscale

// tailscaleRefresh is how often the tailnet address is re-checked; it changes
// only when the node is renamed or reconnects.
const tailscaleRefresh = 60 * time.Second

func (m *Manager) runTailscale(ctx context.Context) error {
	for {
		status := m.checkTailscale(ctx)
		m.set(status)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(tailscaleRefresh):
		}
	}
}

func (m *Manager) checkTailscale(ctx context.Context) Status {
	if host := m.cfg.TailscaleHostname; host != "" {
		return Status{Mode: ModeTailscale, State: StateReady, URL: buildURL(host, m.cfg.Port)}
	}

	bin := m.cfg.TailscaleCLI
	if bin == "" {
		bin = "tailscale"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return Status{Mode: ModeTailscale, State: StateNotInstalled,
			Message: "tailscale CLI not found; set remote.tailscale.hostname to advertise an address anyway"}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, path, "status", "--json").Output()
	if err != nil {
		return Status{Mode: ModeTailscale, State: StateUnavailable,
			Message: "could not read tailscale status: " + err.Error()}
	}

	host, err := ParseTailscaleStatus(out)
	if err != nil {
		return Status{Mode: ModeTailscale, State: StateUnavailable, Message: err.Error()}
	}
	return Status{Mode: ModeTailscale, State: StateReady, URL: buildURL(host, m.cfg.Port)}
}

// ParseTailscaleStatus extracts this node's address from `tailscale status
// --json`, preferring the MagicDNS name over the raw tailnet IP.
func ParseTailscaleStatus(raw []byte) (string, error) {
	var status struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", fmt.Errorf("parse tailscale status: %w", err)
	}
	if status.BackendState != "" && status.BackendState != "Running" {
		return "", fmt.Errorf("tailscale is %s", strings.ToLower(status.BackendState))
	}

	if name := strings.TrimSuffix(status.Self.DNSName, "."); name != "" {
		return name, nil
	}
	for _, ip := range status.Self.TailscaleIPs {
		if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() != nil {
			return ip, nil
		}
	}
	if len(status.Self.TailscaleIPs) > 0 {
		return status.Self.TailscaleIPs[0], nil
	}
	return "", errors.New("tailscale reported no address for this node")
}

// ---------------------------------------------------------------- cloudflare

// quickTunnelPattern matches the hostname cloudflared prints for a quick tunnel.
var quickTunnelPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// restart backoff bounds for the supervised connector.
const (
	minRestartDelay = 2 * time.Second
	maxRestartDelay = 60 * time.Second
)

func (m *Manager) runCloudflare(ctx context.Context) error {
	if !m.cfg.CloudflareManaged {
		// Something else runs the connector (a sidecar container, or a system
		// service); we only report where the daemon should be reachable.
		state := StateReady
		url := ""
		if m.cfg.CloudflareHost != "" {
			url = "https://" + m.cfg.CloudflareHost
		} else if m.cfg.AdvertiseURL == "" {
			state = StateUnavailable
		}
		m.set(Status{Mode: ModeCloudflare, State: state, URL: url,
			Message: "connector is managed externally"})
		<-ctx.Done()
		return nil
	}

	bin := m.cfg.CloudflaredBinary
	if bin == "" {
		bin = "cloudflared"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		m.set(Status{Mode: ModeCloudflare, State: StateNotInstalled,
			Message: "cloudflared not found on PATH; install it or run it as a sidecar"})
		<-ctx.Done()
		return nil
	}

	delay := minRestartDelay
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		start := time.Now()
		err := m.runCloudflared(ctx, path)
		if ctx.Err() != nil {
			return nil
		}

		// A connector that stayed up for a while was healthy; reset the backoff.
		if time.Since(start) > time.Minute {
			delay = minRestartDelay
		}
		m.set(Status{Mode: ModeCloudflare, State: StateUnavailable,
			Message: fmt.Sprintf("connector exited (%v); restarting in %s", err, delay)})

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		if delay *= 2; delay > maxRestartDelay {
			delay = maxRestartDelay
		}
	}
}

// runCloudflared runs the connector once, translating its output into status.
func (m *Manager) runCloudflared(ctx context.Context, path string) error {
	args := CloudflaredArgs(m.cfg.CloudflareToken, m.cfg.Port)
	cmd := exec.CommandContext(ctx, path, args...)
	// Without this, a connector that leaves a grandchild holding the output
	// pipes would keep Wait blocked long after shutdown was requested.
	cmd.WaitDelay = 5 * time.Second
	// cloudflared logs to stderr; merge both so a quick-tunnel URL on either
	// stream is picked up.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	if m.cfg.CloudflareToken != "" {
		url := ""
		if m.cfg.CloudflareHost != "" {
			url = "https://" + m.cfg.CloudflareHost
		}
		m.set(Status{Mode: ModeCloudflare, State: StateReady, URL: url,
			Message: "named tunnel connector running"})
	} else {
		m.set(Status{Mode: ModeCloudflare, State: StateStarting,
			Message: "starting quick tunnel"})
	}

	var wg sync.WaitGroup
	for _, stream := range []io.Reader{stdout, stderr} {
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			m.scanCloudflaredOutput(r)
		}(stream)
	}
	wg.Wait()

	return cmd.Wait()
}

func (m *Manager) scanCloudflaredOutput(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m.log.Debug("cloudflared", "line", line)

		if url := quickTunnelPattern.FindString(line); url != "" {
			m.set(Status{Mode: ModeCloudflare, State: StateReady, URL: url,
				Message: "quick tunnel (temporary; use a tunnel token for a stable hostname)"})
		}
	}
}

// CloudflaredArgs builds the connector's command line: a named tunnel when a
// token is configured, otherwise a quick tunnel pointed at the local server.
func CloudflaredArgs(token string, port int) []string {
	if token != "" {
		return []string{"tunnel", "--no-autoupdate", "run", "--token", token}
	}
	return []string{"tunnel", "--no-autoupdate", "--url", buildURL("127.0.0.1", port)}
}

func buildURL(host string, port int) string {
	if host == "" {
		return ""
	}
	if port == 0 || port == 80 {
		return "http://" + host
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// LocalURLs lists the LAN addresses the daemon answers on, for display at
// startup and in the pairing instructions.
func LocalURLs(port int) []string {
	var urls []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			urls = append(urls, buildURL(ipn.IP.String(), port))
		}
	}
	return urls
}
